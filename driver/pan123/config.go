package pan123

import (
	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk"
)

// Config 123 云盘驱动的配置
//
// 配置结构是驱动私有的数据定义：宿主只把 storage_providers.config 当作**不透明
// JSON 字符串**存取，不再为每个驱动维护一份 proto 消息。JSON 键名必须**逐字**
// 等于元数据里 ConfigField.Name（前端表单按字段名拼 JSON），驱动按下面的 tag 解析。
//
// 键名沿用旧版 proto 消息的 snake_case，已有 provider 落库的配置可以直接读。
type Config struct {
	// ClientID / ClientSecret 123 开放平台开发者凭据（与 refresh_token 二选一）
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// RefreshToken oplist renewapi 续期用的 refresh_ui 令牌：首次用本值请求，
	// 之后改用接口返回的新 refresh_token（驱动会自动持久化覆盖）
	RefreshToken string `json:"refresh_token,omitempty"`
	// AccessToken 直接填写的 access_token，兜底用；非空时优先使用，
	// 过期或接口 401 后由续期路径接管
	AccessToken string `json:"access_token,omitempty"`
	// APIURL 在线续期服务地址（留空等价于默认的 OpenList 公共授权地址）
	APIURL string `json:"api_url,omitempty"`
	// RootFileID 根目录 fileId（123 云盘以 fileId 组织目录，根目录为 0）
	RootFileID string `json:"root_file_id,omitempty"`
	// UploadThread 分片上传并发数（1-32）
	UploadThread sdk.FormNum `json:"upload_thread,omitempty"`
}
