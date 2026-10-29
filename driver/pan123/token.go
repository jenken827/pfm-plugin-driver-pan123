package pan123

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk/logx"
)

// preservedAccessTokenLifetime 手填 access_token 的保守有效期。
//
// 与 OpenList（drivers/123_open/driver.go:46）一致取 90 天：123 JWT 的 exp
// 虽可解出（实测 30 天），但服务端判「过期」的口径与 JWT 不一致
// （2026-10-01 实测：exp 剩 720 小时仍被拒为 token is expired），故不信任
// JWT 自身有效期，用保守值先让手填令牌生效；真正过期由接口 401 触发续期。
const preservedAccessTokenLifetime = 90 * 24 * time.Hour

// 续期节流参数。
//
// forceRefreshMinInterval：两次「成功续期」之间的最小间隔。force=true（401
// 触发）会绕过有效期缓存，若不加限流，服务端持续 401 时 requestJSON 的 3 次
// 重试 + 分片上传每片一次，会在秒级内连打 renewapi，触发官方频率限制。
// 取 60s：远小于 access_token 的有效期，不影响正常续期，又能把突发重试压缩
// 成一次网络请求。
//
// refreshFailureCooldown：续期失败后的冷却期。历史实现是 blockRefresh 永久
// 置位，一次瞬时失败（网络抖动、renewapi 短暂 5xx）就把驱动锁死到进程结束，
// 且报错信息不提示可恢复。改为冷却期后自动解禁，冷却期内若有可用 token
// 仍允许调用方继续使用。
const (
	forceRefreshMinInterval = 60 * time.Second
	refreshFailureCooldown  = 2 * time.Minute
)

// tokenManager 管理 access_token 的有效期与刷新
type tokenManager struct {
	mu          sync.Mutex
	accessToken string
	expiredAt   time.Time
	// refreshToken oplist renewapi 续期令牌（刷新后会被新值覆盖）
	refreshToken string
	// blockRefresh 续期失败后置位的软阻断标志（配合 blockUntil 冷却）
	blockRefresh bool
	// blockUntil 软阻断的解除时刻；到期后自动允许重新尝试续期
	blockUntil time.Time
	// lastRenewAt 上次成功续期的时刻，用于 force 续期的最小间隔节流
	lastRenewAt time.Time
	// renewed 标记本次进程内是否发生过真实续期（用于决定是否需要回写配置）
	renewed bool
}

// credentials 驱动持有的认证相关配置
type credentials struct {
	clientID     string
	clientSecret string
	apiURL       string // oplist renewapi 地址（默认 RenewAPIDefault）
}

// getAccessToken 返回可用的 access_token，必要时自动刷新。
//
// 刷新顺序（与 OpenList drivers/123_open/token.go 一致）：
//  1. oplist renewapi（配了 refresh_token 时）：首次用用户填入的 refresh_token，
//     之后用接口返回的新 refresh_token；
//  2. 官方开发者凭据 client_id/client_secret：POST /api/v1/access_token；
//  3. 都不具备则只可能依赖 config 里手填的 access_token（由调用方保证有效）。
//
// 续期节流（force=true 尤其重要）：
//   - force=true 会绕过「有效期内直接用」的缓存判定，历史上每次都要打
//     renewapi；requestJSON 的 401 重试最多 3 次、分片上传每片各走一遍
//     requestJSON —— 服务端持续 401 时会在极短时间内连打 renewapi，
//     触发官方续期频率限制。
//   - 故对成功续期加最小间隔 forceRefreshMinInterval：间隔内的 force 请求
//     直接复用当前 access_token（若为空则返回错误），不打网络。
//   - blockRefresh 不再是「永久置位」：改为带冷却期的软阻断，冷却结束后
//     允许再次尝试续期，避免一次瞬时失败把驱动永久锁死。
func (tm *tokenManager) getAccessToken(ctx context.Context, c credentials, force bool) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	now := time.Now()

	// 软阻断：续期失败后进入冷却，冷却期内不再打续期接口
	if tm.blockRefresh {
		if now.Before(tm.blockUntil) {
			if tm.accessToken != "" {
				// 仍有可用 token：先让调用方用它（真正的失效由接口 401 暴露）
				return tm.accessToken, nil
			}
			return "", fmt.Errorf("123pan authentication expired and refresh is blocked (retry after %s)",
				time.Until(tm.blockUntil).Round(time.Second))
		}
		// 冷却结束，解除阻断再试
		tm.blockRefresh = false
	}

	// 非强制：有效期内直接复用（提前 5 分钟视为过期，避免请求途中失效）
	if !force && tm.accessToken != "" && now.Before(tm.expiredAt.Add(-5*time.Minute)) {
		return tm.accessToken, nil
	}
	// 强制续期也要节流：距上次成功续期不足最小间隔时复用现有 token
	if force && tm.accessToken != "" && !tm.lastRenewAt.IsZero() &&
		now.Sub(tm.lastRenewAt) < forceRefreshMinInterval {
		return tm.accessToken, nil
	}

	if err := tm.flush(ctx, c); err != nil {
		// 续期失败：进入冷却期，避免后续每个请求都打续期接口
		tm.blockRefresh = true
		tm.blockUntil = now.Add(refreshFailureCooldown)
		return "", err
	}
	tm.lastRenewAt = time.Now()
	return tm.accessToken, nil
}

// flush 执行实际刷新（调用方已持锁）
func (tm *tokenManager) flush(ctx context.Context, c credentials) error {
	if tm.refreshToken != "" {
		apiURL := c.apiURL
		if apiURL == "" {
			apiURL = RenewAPIDefault
		}
		token, refreshToken, expiresIn, err := renewViaOnlineAPI(ctx, apiURL, tm.refreshToken)
		if err != nil {
			return err
		}
		tm.accessToken = token
		tm.refreshToken = refreshToken
		tm.expiredAt = time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
		tm.blockRefresh = false
		tm.blockUntil = time.Time{}
		tm.renewed = true
		return nil
	}

	if c.clientID != "" && c.clientSecret != "" {
		token, expiredAt, err := requestAccessToken(ctx, c.clientID, c.clientSecret)
		if err != nil {
			return err
		}
		tm.accessToken = token
		tm.expiredAt = expiredAt
		tm.blockRefresh = false
		tm.blockUntil = time.Time{}
		tm.renewed = true
		return nil
	}

	return fmt.Errorf("no valid authentication method available: " +
		"need client_id/client_secret, refresh_token or a pre-set access_token")
}

// initTokenManager 用配置初始化 token 管理器
//
// 优先级：**手填 access_token 优先**，refresh_token / client 凭据只作续期兜底。
//
// 历史缺陷：只要配了 refresh_token 或 client 凭据就无条件清空手填的
// access_token（`tm.accessToken = ""`），随后 warmup 立即走续期——续期路径
// 不可用（renewapi 令牌失效 / 无 api_url）时直接全盘失效：
//
//	blockRefresh 置位 → 所有请求报 "authentication expired and refresh is blocked"，
//
// 而用户填的、可能完全可用的 access_token 已被丢弃，无从回退。
// 2026-10-01 事故即此形态（oplist 签发的令牌 aud 归属应用异常）。
//
// OpenList 的对应判断（drivers/123_open/driver.go:37）为
//
//	(d.UseOnlineAPI && d.RefreshToken != "" && len(d.APIAddress) > 0) || (d.ClientID != "" && ...)
//
// 比 pfm 多两个前置条件；此处改为更保守的策略：不清空手填 token，
// 让它先按保守有效期使用，401 时再由 getAccessToken(force=true) 触发续期。
func initTokenManager(cfg *Config) *tokenManager {
	tm := &tokenManager{}
	if cfg == nil {
		return tm
	}

	tm.accessToken = cfg.AccessToken
	tm.refreshToken = cfg.RefreshToken

	if tm.accessToken != "" {
		// 手填 access_token：按保守有效期使用（123 JWT 的 exp 虽可解出，
		// 但服务端判过期口径不一致，见 2026-10-01 排查记录，故与 OpenList
		// 一致用保守值），到期或 401 时再由续期路径接管。
		tm.expiredAt = time.Now().Add(preservedAccessTokenLifetime)
		return tm
	}

	// 无手填 token：只能靠续期（refresh_token 或 client 凭据）
	return tm
}

// requestAccessToken 用官方开发者凭据换取 access_token
func requestAccessToken(ctx context.Context, clientID, clientSecret string) (string, time.Time, error) {
	body, err := json.Marshal(map[string]string{
		"clientID":     clientID,
		"clientSecret": clientSecret,
	})
	if err != nil {
		return "", time.Time{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		APIBase+endpointAccessToken, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("platform", platformHeader)

	resp, err := newHTTPClient().Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("access token request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read access token response failed: %w", err)
	}

	var parsed AccessTokenResp
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", time.Time{}, fmt.Errorf("parse access token response failed: %w", err)
	}
	if parsed.Code != 0 {
		return "", time.Time{}, fmt.Errorf("get access token failed: %s", parsed.Message)
	}
	if parsed.Data.AccessToken == "" || parsed.Data.ExpiredAt == "" {
		return "", time.Time{}, fmt.Errorf("invalid token payload from developer API")
	}

	expiredAt, err := time.Parse(time.RFC3339, parsed.Data.ExpiredAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse expire time failed: %w", err)
	}
	return parsed.Data.AccessToken, expiredAt.UTC(), nil
}

// renewViaOnlineAPI 通过 oplist renewapi 在线续期
//
// 参数 refreshUI 即 refresh_ui 令牌：首次为用户填入的值，之后为上次响应返回的新值。
func renewViaOnlineAPI(ctx context.Context, apiURL, refreshUI string) (accessToken, refreshToken string, expiresIn int64, err error) {
	if apiURL == "" {
		apiURL = RenewAPIDefault
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", "", 0, err
	}
	q := req.URL.Query()
	q.Set("refresh_ui", refreshUI)
	q.Set("server_use", "true")
	q.Set("driver_txt", "123cloud_oa")
	req.URL.RawQuery = q.Encode()

	resp, err := newHTTPClient().Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("renew api request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", 0, fmt.Errorf("read renew api response failed: %w", err)
	}

	var parsed RenewTokenResp
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", "", 0, fmt.Errorf("parse renew api response failed: %w", err)
	}
	if parsed.AccessToken == "" || parsed.RefreshToken == "" {
		msg := parsed.ErrorDescription
		if msg == "" {
			msg = parsed.Text
		}
		if msg == "" {
			msg = parsed.Message
		}
		if msg == "" {
			msg = parsed.Error
		}
		if msg != "" {
			return "", "", 0, fmt.Errorf("failed to refresh token: %s", msg)
		}
		return "", "", 0, fmt.Errorf("empty access_token or refresh_token returned from renew api")
	}
	if parsed.ExpiresIn <= 0 {
		return "", "", 0, fmt.Errorf("invalid expires_in from renew api")
	}

	return parsed.AccessToken, parsed.RefreshToken, parsed.ExpiresIn, nil
}

// persistTokens 把续期结果写回内存配置并持久化到数据库。
//
// refresh_token 必须回写轮换值：123 官方接口文档明确「refresh_token 单次有效，
// 刷新后返回新的 refresh_token，请保存以便下一次刷新使用」。不回写等于下一
// 次续期仍用过期的单次令牌，服务端返回
//
//	HTTP 500 {"text":"The provided authorization grant ... is invalid, expired, revoked"}
//
// （与 aliyundrive_open 的 persistTokens 行为一致）
func (d *Pan123Driver) persistTokens() {
	// 先写回内存配置，再序列化（顺序不能反，否则持久化的是旧 token）
	d.Config.AccessToken = d.tm.accessToken
	if d.tm.refreshToken != "" {
		d.Config.RefreshToken = d.tm.refreshToken
	}

	cfgJSON, err := json.Marshal(d.Config)
	if err != nil {
		logx.L().Errorf("123pan marshal config failed: %v", err)
		return
	}

	if d.configPersister == nil || d.providerID == 0 {
		return
	}
	if err := d.configPersister.UpdateConfig(d.providerID, string(cfgJSON)); err != nil {
		logx.L().Errorf("123pan persist refreshed token failed: %v", err)
	}
}
