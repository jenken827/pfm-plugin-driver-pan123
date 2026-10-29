package pan123

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jenken827/pfm-plugin-sdk/driver/types"
	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk"
	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk/logx"
	"github.com/jenken827/pfm-plugin-sdk/proto/gen/abi"
)

// DriverID 是本驱动的身份（落库在 storage_providers.provider_type）。
//
// 驱动身份由驱动自己声明：SDK 不认识任何具体驱动。发布后不可更改。
const DriverID = "pan123"

// errNoCredentials 未配置任何可用认证方式
var errNoCredentials = errors.New("no authentication configured: " +
	"need client_id+client_secret, refresh_token or access_token")

func init() {
	metadata := &abi.DriverInfo{
		DriverId: sdk.StringPtr(DriverID),
		Capabilities: []abi.DriverCapability{
			abi.DriverCapability_DRIVER_CAPABILITY_BATCH_DELETE,
			abi.DriverCapability_DRIVER_CAPABILITY_DIRECT_LINK,
			abi.DriverCapability_DRIVER_CAPABILITY_REMOTE_LIST,
		},
		Name:        sdk.StringPtr("PAN123"),
		DisplayName: sdk.StringPtr("123云盘"),
		Description: sdk.StringPtr("123云盘开放平台存储提供者"),
		IconName:    sdk.StringPtr("cloud"),
		ConfigFields: []*abi.ConfigField{
			{
				Name:        sdk.StringPtr("client_id"),
				FieldType:   abi.ConfigFieldType_STRING.Enum(),
				Label:       sdk.StringPtr("Client ID"),
				Placeholder: sdk.StringPtr("123云盘开放平台 Client ID"),
				HelperText:  sdk.StringPtr("在 https://www.123pan.com/developer 申请，与 Client Secret 搭配使用"),
				Required:    sdk.BoolPtr(false),
				IconName:    sdk.StringPtr("vpn_key"),
				InputType:   sdk.StringPtr("text"),
			},
			{
				Name:        sdk.StringPtr("client_secret"),
				FieldType:   abi.ConfigFieldType_STRING.Enum(),
				Label:       sdk.StringPtr("Client Secret"),
				Placeholder: sdk.StringPtr("123云盘开放平台 Client Secret"),
				HelperText:  sdk.StringPtr("与 Client ID 搭配使用，优先于 Refresh Token"),
				Required:    sdk.BoolPtr(false),
				IconName:    sdk.StringPtr("lock"),
				InputType:   sdk.StringPtr("password"),
			},
			{
				Name:        sdk.StringPtr("refresh_token"),
				FieldType:   abi.ConfigFieldType_STRING.Enum(),
				Label:       sdk.StringPtr("Refresh Token"),
				Placeholder: sdk.StringPtr("oplist renewapi 的 refresh_ui 令牌"),
				HelperText:  sdk.StringPtr("无开发者凭据时填写；首次用本值请求，后续自动改用返回的新值"),
				Required:    sdk.BoolPtr(false),
				IconName:    sdk.StringPtr("vpn_key"),
				InputType:   sdk.StringPtr("password"),
			},
			{
				Name:        sdk.StringPtr("access_token"),
				FieldType:   abi.ConfigFieldType_STRING.Enum(),
				Label:       sdk.StringPtr("Access Token"),
				Placeholder: sdk.StringPtr("直接填写的 access_token"),
				HelperText:  sdk.StringPtr("兜底选项，自带有效期；配置了上面两项时会被忽略"),
				Required:    sdk.BoolPtr(false),
				IconName:    sdk.StringPtr("lock"),
				InputType:   sdk.StringPtr("password"),
			},
			{
				Name:         sdk.StringPtr("api_url"),
				FieldType:    abi.ConfigFieldType_STRING.Enum(),
				Label:        sdk.StringPtr("续期服务地址"),
				Placeholder:  sdk.StringPtr(RenewAPIDefault),
				HelperText:   sdk.StringPtr("oplist renewapi 在线续期服务地址"),
				Required:     sdk.BoolPtr(false),
				DefaultValue: sdk.StringPtr(RenewAPIDefault),
				IconName:     sdk.StringPtr("settings_ethernet"),
			},
			{
				Name:         sdk.StringPtr("root_file_id"),
				FieldType:    abi.ConfigFieldType_STRING.Enum(),
				Label:        sdk.StringPtr("根目录 FileId"),
				Placeholder:  sdk.StringPtr("0"),
				HelperText:   sdk.StringPtr("123云盘以 fileId 组织目录，0 表示根目录"),
				Required:     sdk.BoolPtr(false),
				DefaultValue: sdk.StringPtr("0"),
				IconName:     sdk.StringPtr("folder_open"),
			},
			{
				Name:         sdk.StringPtr("upload_thread"),
				FieldType:    abi.ConfigFieldType_NUMBER.Enum(),
				Label:        sdk.StringPtr("上传线程数"),
				Placeholder:  sdk.StringPtr("3"),
				HelperText:   sdk.StringPtr("并发上传分片线程数（1-32）"),
				Required:     sdk.BoolPtr(false),
				DefaultValue: sdk.StringPtr("3"),
				IconName:     sdk.StringPtr("speed"),
				InputType:    sdk.StringPtr("number"),
			},
		},
	}

	types.RegisterStorageDriverWithMetadata(func(provider *types.Provider) (types.StorageDriver, error) {
		cfg := &Config{}
		if provider.ConfigJSON != "" {
			if err := json.Unmarshal([]byte(provider.ConfigJSON), cfg); err != nil {
				logx.L().Errorf("123pan unmarshal config failed: %v", err)
				return nil, err
			}
		}

		// 凭据有效性预检：缺失凭据不阻断初始化（与百度驱动一致），
		// 仅告警，实际请求时才返回认证错误。
		if err := checkCredentials(cfg); err != nil {
			logx.L().Warnf("123pan credentials incomplete for provider %d: %v",
				provider.ID, err)
		}

		d := &Pan123Driver{
			Config:         cfg,
			providerID:     provider.ID,
			ProviderConfig: provider.Config,
			creds: credentials{
				clientID:     cfg.ClientID,
				clientSecret: cfg.ClientSecret,
				apiURL:       cfg.APIURL,
			},
			tm:     initTokenManager(cfg),
			client: newHTTPClient(),
		}

		// 预热 token 并验证有效性（失败仅告警）
		if err := d.warmup(); err != nil {
			logx.L().Warnf("123pan warmup failed for provider %d: %v", provider.ID, err)
		}

		return d, nil
	}, metadata)
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
	logx.L().Infof("123pan user ready, uid: %d, space_permanent: %d, space_used: %d",
		userInfo.Data.UID, userInfo.Data.SpacePermanent, userInfo.Data.SpaceUsed)
	return nil
}

// checkCredentials 校验至少有一种可用认证方式
func checkCredentials(cfg *Config) error {
	if cfg == nil {
		return errNoCredentials
	}
	hasClient := cfg.ClientID != "" && cfg.ClientSecret != ""
	hasRefresh := cfg.RefreshToken != ""
	hasToken := cfg.AccessToken != ""
	if !hasClient && !hasRefresh && !hasToken {
		return errNoCredentials
	}
	return nil
}
