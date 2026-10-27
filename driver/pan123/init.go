package pan123

import (
	"context"
	"encoding/json"
	"errors"

	"jenken827.com/jenken827/pfm/driver/types"
	"jenken827.com/jenken827/pfm/pkg/llog"
	"jenken827.com/jenken827/pfm/proto/gen/entity"
	"jenken827.com/jenken827/pfm/proto/gen/native"
	"jenken827.com/jenken827/pfm/utils"
)

// errNoCredentials 未配置任何可用认证方式
var errNoCredentials = errors.New("no authentication configured: " +
	"need client_id+client_secret, refresh_token or access_token")

func init() {
	metadata := &native.DriverInfo{
		DriverId: utils.StringPtr(types.DriverPan123),
		Capabilities: []native.DriverCapability{
			native.DriverCapability_DRIVER_CAPABILITY_BATCH_DELETE,
			native.DriverCapability_DRIVER_CAPABILITY_DIRECT_LINK,
			native.DriverCapability_DRIVER_CAPABILITY_REMOTE_LIST,
		},
		Name:        utils.StringPtr("PAN123"),
		DisplayName: utils.StringPtr("123云盘"),
		Description: utils.StringPtr("123云盘开放平台存储提供者"),
		IconName:    utils.StringPtr("cloud"),
		ConfigFields: []*native.ConfigField{
			{
				Name:        utils.StringPtr("client_id"),
				FieldType:   native.ConfigFieldType_STRING.Enum(),
				Label:       utils.StringPtr("Client ID"),
				Placeholder: utils.StringPtr("123云盘开放平台 Client ID"),
				HelperText:  utils.StringPtr("在 https://www.123pan.com/developer 申请，与 Client Secret 搭配使用"),
				Required:    utils.BoolPtr(false),
				IconName:    utils.StringPtr("vpn_key"),
				InputType:   utils.StringPtr("text"),
			},
			{
				Name:        utils.StringPtr("client_secret"),
				FieldType:   native.ConfigFieldType_STRING.Enum(),
				Label:       utils.StringPtr("Client Secret"),
				Placeholder: utils.StringPtr("123云盘开放平台 Client Secret"),
				HelperText:  utils.StringPtr("与 Client ID 搭配使用，优先于 Refresh Token"),
				Required:    utils.BoolPtr(false),
				IconName:    utils.StringPtr("lock"),
				InputType:   utils.StringPtr("password"),
			},
			{
				Name:        utils.StringPtr("refresh_token"),
				FieldType:   native.ConfigFieldType_STRING.Enum(),
				Label:       utils.StringPtr("Refresh Token"),
				Placeholder: utils.StringPtr("oplist renewapi 的 refresh_ui 令牌"),
				HelperText:  utils.StringPtr("无开发者凭据时填写；首次用本值请求，后续自动改用返回的新值"),
				Required:    utils.BoolPtr(false),
				IconName:    utils.StringPtr("vpn_key"),
				InputType:   utils.StringPtr("password"),
			},
			{
				Name:        utils.StringPtr("access_token"),
				FieldType:   native.ConfigFieldType_STRING.Enum(),
				Label:       utils.StringPtr("Access Token"),
				Placeholder: utils.StringPtr("直接填写的 access_token"),
				HelperText:  utils.StringPtr("兜底选项，自带有效期；配置了上面两项时会被忽略"),
				Required:    utils.BoolPtr(false),
				IconName:    utils.StringPtr("lock"),
				InputType:   utils.StringPtr("password"),
			},
			{
				Name:         utils.StringPtr("api_url"),
				FieldType:    native.ConfigFieldType_STRING.Enum(),
				Label:        utils.StringPtr("续期服务地址"),
				Placeholder:  utils.StringPtr(RenewAPIDefault),
				HelperText:   utils.StringPtr("oplist renewapi 在线续期服务地址"),
				Required:     utils.BoolPtr(false),
				DefaultValue: utils.StringPtr(RenewAPIDefault),
				IconName:     utils.StringPtr("settings_ethernet"),
			},
			{
				Name:         utils.StringPtr("root_file_id"),
				FieldType:    native.ConfigFieldType_STRING.Enum(),
				Label:        utils.StringPtr("根目录 FileId"),
				Placeholder:  utils.StringPtr("0"),
				HelperText:   utils.StringPtr("123云盘以 fileId 组织目录，0 表示根目录"),
				Required:     utils.BoolPtr(false),
				DefaultValue: utils.StringPtr("0"),
				IconName:     utils.StringPtr("folder_open"),
			},
			{
				Name:         utils.StringPtr("upload_thread"),
				FieldType:    native.ConfigFieldType_STRING.Enum(),
				Label:        utils.StringPtr("上传线程数"),
				Placeholder:  utils.StringPtr("3"),
				HelperText:   utils.StringPtr("并发上传分片线程数（1-32）"),
				Required:     utils.BoolPtr(false),
				DefaultValue: utils.StringPtr("3"),
				IconName:     utils.StringPtr("speed"),
				InputType:    utils.StringPtr("number"),
			},
		},
	}

	types.RegisterStorageDriverWithMetadata(newDriver, metadata)
}

// newDriver 构造 123云盘驱动实例
func newDriver(provider *entity.StorageProviderORM) (types.StorageDriver, error) {
	cfg := &native.Pan123DriverConfig{}
	if provider.Config != nil && *provider.Config != "" {
		if err := json.Unmarshal([]byte(*provider.Config), cfg); err != nil {
			llog.L().Errorf("123pan unmarshal config failed: %v", err)
			return nil, err
		}
	}

	// 凭据有效性预检：缺失凭据不阻断初始化（与百度驱动一致），
	// 仅告警，实际请求时才返回认证错误。
	if err := checkCredentials(cfg); err != nil {
		llog.L().Warnf("123pan credentials incomplete for provider %d: %v",
			providerID(provider), err)
	}

	d := &Pan123Driver{
		Config:         cfg,
		Orm:            provider,
		ProviderConfig: types.ProviderConfigFromORM(provider),
		creds: credentials{
			clientID:     cfg.GetClientId(),
			clientSecret: cfg.GetClientSecret(),
			apiURL:       cfg.GetApiUrl(),
		},
		tm:     initTokenManager(cfg),
		client: newHTTPClient(),
	}

	// 预热 token 并验证有效性（失败仅告警）
	if err := d.warmup(); err != nil {
		llog.L().Warnf("123pan warmup failed for provider %d: %v", providerID(provider), err)
	}

	return d, nil
}

// warmup 预取 token 并读取用户信息
func (d *Pan123Driver) warmup() error {
	ctx := context.Background()

	if _, err := d.tm.getAccessToken(ctx, d.creds, false); err != nil {
		return err
	}

	// 仅在「本次确实发生了续期」时持久化：手填 access_token 优先的策略下，
	// token 可能原样来自配置，写回纯属无谓（且会把未变化的 refresh_token
	// 重新序列化一遍）。续期过的标志是 expiredAt 被 flush 改写为
	// now+expires_in，与 initTokenManager 设的保守值区分开。
	if d.tm.renewed {
		d.persistTokens()
	}

	userInfo, err := d.getUserInfo(ctx)
	if err != nil {
		return err
	}
	d.uid = userInfo.Data.UID
	llog.L().Infof("123pan user ready, uid: %d, space_permanent: %d, space_used: %d",
		userInfo.Data.UID, userInfo.Data.SpacePermanent, userInfo.Data.SpaceUsed)
	return nil
}

// checkCredentials 校验至少有一种可用认证方式
func checkCredentials(cfg *native.Pan123DriverConfig) error {
	hasClient := cfg.GetClientId() != "" && cfg.GetClientSecret() != ""
	hasRefresh := cfg.GetRefreshToken() != ""
	hasToken := cfg.GetAccessToken() != ""
	if !hasClient && !hasRefresh && !hasToken {
		return errNoCredentials
	}
	return nil
}

// providerID 安全读取 provider id（日志用）
func providerID(provider *entity.StorageProviderORM) uint32 {
	if provider == nil || provider.Id == nil {
		return 0
	}
	return *provider.Id
}
