package pan123

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"jenken827.com/jenken827/pfm/driver/types"
	"jenken827.com/jenken827/pfm/pkg/llog"
	"jenken827.com/jenken827/pfm/proto/gen/entity"
	"jenken827.com/jenken827/pfm/proto/gen/native"
)

// Pan123Driver 123云盘驱动（开放平台 Open API 模式）
type Pan123Driver struct {
	Config *native.Pan123DriverConfig
	Orm    *entity.StorageProviderORM

	ProviderConfig  types.ProviderConfig
	configPersister types.ProviderConfigPersister

	creds credentials
	tm    *tokenManager
	uid   uint64

	// qps 按端点限速（rate.Limiter），见 apiEndpoints
	qpsMu sync.Mutex
	qps   map[string]*rate.Limiter

	client *http.Client
}

// GetType 获取存储驱动类型
func (d *Pan123Driver) GetType() string {
	return types.DriverPan123
}

// GetName 获取驱动名称
func (d *Pan123Driver) GetName() string {
	return types.DriverPan123
}

// GetConfig 获取驱动配置
func (d *Pan123Driver) GetConfig() any {
	return d.Config
}

// GetProviderConfig 获取存储提供者的类型化配置视图
func (d *Pan123Driver) GetProviderConfig() *types.ProviderConfig {
	return &d.ProviderConfig
}

// SetConfigPersister 注入配置持久化器（token 刷新后持久化）
func (d *Pan123Driver) SetConfigPersister(p types.ProviderConfigPersister) {
	d.configPersister = p
}

// newHTTPClient 创建 123 API 专用客户端。
//
// 不设 http.Client.Timeout：它是整个请求的绝对超时，大文件上传/下载途中
// 会强断仍在传输的连接。只用 ResponseHeaderTimeout 约束响应头等待，
// body 读取由请求 ctx 控制（与百度驱动一致的原因，见 pfm-plugin-baidu 的 init.go）。
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   16,
			MaxConnsPerHost:       32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			DisableKeepAlives:     false,
		},
	}
}

// limiterFor 获取（并惰性创建）某端点的限速器。
// burst=1：不允许突发，严格按 apiEndpoints 的 QPS 均速放行。
func (d *Pan123Driver) limiterFor(endpoint string) *rate.Limiter {
	qps := apiEndpoints[endpoint]
	if qps <= 0 {
		return nil
	}
	d.qpsMu.Lock()
	defer d.qpsMu.Unlock()
	if d.qps == nil {
		d.qps = make(map[string]*rate.Limiter)
	}
	l, ok := d.qps[endpoint]
	if !ok {
		l = rate.NewLimiter(rate.Limit(qps), 1)
		d.qps[endpoint] = l
	}
	return l
}

// requestJSON 发送带鉴权的 JSON 请求，返回原始响应体。
//
// 错误处理：
//   - code=0：成功
//   - code=401：强制刷新 token 后重试一次
//   - code=429：等待 500ms 后重试
//   - 其它：返回带 message 的错误
func (d *Pan123Driver) requestJSON(ctx context.Context, method, endpoint string, query url.Values, body any) ([]byte, error) {
	// 最多重试 3 次（401/429 各消耗一次）
	const maxRetries = 3

	for attempt := 0; attempt < maxRetries; attempt++ {
		token, err := d.tm.getAccessToken(ctx, d.creds, false)
		if err != nil {
			return nil, err
		}

		var reader io.Reader
		if body != nil {
			raw, mErr := json.Marshal(body)
			if mErr != nil {
				return nil, mErr
			}
			reader = bytes.NewReader(raw)
		}

		reqURL := APIBase + endpoint
		if len(query) > 0 {
			reqURL += "?" + query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("platform", platformHeader)
		req.Header.Set("Content-Type", "application/json")

		if l := d.limiterFor(endpoint); l != nil {
			if err := l.Wait(ctx); err != nil {
				return nil, fmt.Errorf("rate limiter wait failed: %w", err)
			}
		}

		resp, err := d.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("123pan request %s failed: %w", endpoint, err)
		}
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read 123pan response failed: %w", readErr)
		}

		var base BaseResp
		if err := json.Unmarshal(respBody, &base); err != nil {
			return nil, fmt.Errorf("parse 123pan response failed: %w, body=%s",
				err, truncateBody(string(respBody), 256))
		}

		switch base.Code {
		case 0:
			return respBody, nil
		case 401:
			// token 失效：强制刷新后重试
			llog.C(ctx).Warnf("123pan token unauthorized on %s, refreshing", endpoint)
			if _, err := d.tm.getAccessToken(ctx, d.creds, true); err != nil {
				return nil, fmt.Errorf("refresh token failed: %w", err)
			}
			continue
		case 429:
			llog.C(ctx).Warnf("123pan rate limited on %s, retry after 500ms", endpoint)
			time.Sleep(500 * time.Millisecond)
			continue
		default:
			return nil, &Pan123APIError{Endpoint: endpoint, Code: base.Code, Message: base.Message}
		}
	}
	return nil, fmt.Errorf("123pan request %s failed after %d retries", endpoint, maxRetries)
}

// Pan123APIError 携带服务端 code 的错误，供调用方按码分支处理
// （如 complete 轮询需识别 20103「文件正在校验中」继续轮询而非终止）。
type Pan123APIError struct {
	Endpoint string
	Code     int
	Message  string
}

func (e *Pan123APIError) Error() string {
	return fmt.Sprintf("123pan api error on %s: code=%d, message=%s", e.Endpoint, e.Code, e.Message)
}

// isPan123Code 判断错误是否由指定服务端 code 引起
func isPan123Code(err error, code int) bool {
	var apiErr *Pan123APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == code
	}
	return false
}
func (d *Pan123Driver) getUserInfo(ctx context.Context) (*UserInfoResp, error) {
	body, err := d.requestJSON(ctx, http.MethodGet, endpointUserInfo, nil, nil)
	if err != nil {
		return nil, err
	}
	var resp UserInfoResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// getFiles 列目录（游标分页，直到 lastFileId 为 0 或 -1）
func (d *Pan123Driver) getFiles(ctx context.Context, parentFileID int64) ([]PanFile, error) {
	const limit = 100
	var all []PanFile
	lastFileID := int64(0)

	for {
		query := url.Values{}
		query.Set("parentFileId", strconv.FormatInt(parentFileID, 10))
		query.Set("limit", strconv.Itoa(limit))
		query.Set("lastFileId", strconv.FormatInt(lastFileID, 10))
		query.Set("trashed", "false")

		body, err := d.requestJSON(ctx, http.MethodGet, endpointFileList, query, nil)
		if err != nil {
			return nil, err
		}
		var resp FileListResp
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}

		// trashed 参数目前不生效（123 侧行为），仍按 OpenList 的做法遍历过滤
		for _, f := range resp.Data.FileList {
			if f.Trashed == 0 {
				all = append(all, f)
			}
		}

		lastFileID = resp.Data.LastFileID
		if lastFileID == 0 || lastFileID == -1 || len(resp.Data.FileList) == 0 {
			break
		}
	}
	return all, nil
}

// getDownloadLink 获取文件下载直链
func (d *Pan123Driver) getDownloadLink(ctx context.Context, fileID int64) (string, error) {
	query := url.Values{}
	query.Set("fileId", strconv.FormatInt(fileID, 10))

	body, err := d.requestJSON(ctx, http.MethodGet, endpointDownloadInfo, query, nil)
	if err != nil {
		return "", err
	}
	var resp DownloadInfoResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", err
	}
	if resp.Data.DownloadURL == "" {
		return "", fmt.Errorf("empty download url for file %d", fileID)
	}
	return resp.Data.DownloadURL, nil
}

// mkdir 创建目录，返回新目录 fileId（已存在时也返回）
//
// 响应字段是 dirID（见 MkdirResp 注释）。返回 0 表示无法确定新目录 id，
// 调用方（dirBackend.resolve）会回退到 findChild 重新查询。
func (d *Pan123Driver) mkdir(ctx context.Context, parentFileID int64, name string) (int64, error) {
	body, err := d.requestJSON(ctx, http.MethodPost, endpointMkdir, nil, map[string]any{
		"parentID": parentFileID,
		"name":     name,
	})
	if err != nil {
		return 0, err
	}
	var resp MkdirResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, err
	}
	return resp.Data.DirID, nil
}

// trash 批量删除（移入回收站）
func (d *Pan123Driver) trash(ctx context.Context, fileIDs []int64) error {
	if len(fileIDs) == 0 {
		return nil
	}
	_, err := d.requestJSON(ctx, http.MethodPost, endpointTrash, nil, map[string]any{
		"fileIDs": fileIDs,
	})
	return err
}

// truncateBody 截断响应体用于日志/错误信息
func truncateBody(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// parseFileIDs 从 FileRef 列表提取 fileId（123 用 int64 fileId 作 ProviderFileID）
func parseFileIDs(refs []types.FileRef) ([]int64, error) {
	ids := make([]int64, 0, len(refs))
	for _, ref := range refs {
		if ref.ProviderFileID == "" {
			return nil, fmt.Errorf("empty provider_file_id for location %d", ref.LocationID)
		}
		id, err := strconv.ParseInt(ref.ProviderFileID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid fileId %q: %w", ref.ProviderFileID, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// pathJoin 拼接 123 侧路径（保持前导 /，与百度 rootPath 约定一致）
func pathJoin(base, name string) string {
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		return "/" + name
	}
	return base + "/" + name
}
