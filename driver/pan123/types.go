package pan123

// 123云盘开放平台 API 端点与响应体定义。
// 参考实现：OpenList drivers/123_open（2026-09 版本）。
//
// 端点与 QPS 限制来自 OpenList util.go，各端点单独限速（超限返回 code=429）。
var (
	// APIBase 开放平台根地址
	APIBase = "https://open-api.123pan.com"
	// RenewAPIDefault 默认在线续期服务（oplist 公共服务）
	RenewAPIDefault = "https://api.oplist.org/123cloud/renewapi"
)

const (
	// endpointAccessToken 开发者凭据换 access_token
	endpointAccessToken = "/api/v1/access_token"
	// endpointUserInfo 用户信息（含空间配额）
	endpointUserInfo = "/api/v1/user/info"
	// endpointFileList 文件列表（游标分页）
	endpointFileList = "/api/v2/file/list"
	// endpointDownloadInfo 下载直链
	endpointDownloadInfo = "/api/v1/file/download_info"
	// endpointMkdir 创建目录
	endpointMkdir = "/upload/v1/file/mkdir"
	// endpointTrash 批量删除（移入回收站）
	endpointTrash = "/api/v1/file/trash"
	// endpointUploadCreateV2 创建上传任务（支持秒传）
	endpointUploadCreateV2 = "/upload/v2/file/create"
	// endpointUploadSliceV2 上传分片（上传域名，不在 APIBase 下）
	endpointUploadSliceV2 = "/upload/v2/file/slice"
	// endpointUploadCompleteV2 合并分片
	endpointUploadCompleteV2 = "/upload/v2/file/upload_complete"
	// endpointSHA1Reuse SHA1 秒传
	endpointSHA1Reuse = "/upload/v2/file/sha1_reuse"
)

// Pan123CodeVerifying 文件正在校验中，请间隔 1 秒后再试。
//
// /upload/v2/file/upload_complete 的**中间状态**而非致命错误：分片合并后
// 服务端需要时间校验，此码表示"还没好，稍后再查"。调用方（complete 轮询）
// 必须识别并继续轮询，否则上传在最后一步失败。
// OpenList 的做法是忽略 complete 的一切错误继续轮询
// （drivers/123_open/driver.go:233-248，注释「返回错误代码未知，如：20103」）。
const Pan123CodeVerifying = 20103

// 平台标识头：123 开放平台要求同时带 Authorization 与 platform
const platformHeader = "open_platform"

// apiEndpoints 各端点的每秒请求上限（QPS），0 表示不限速
var apiEndpoints = map[string]int{
	endpointUserInfo:         1,
	endpointFileList:         3,
	endpointDownloadInfo:     5,
	endpointMkdir:            2,
	endpointTrash:            2,
	endpointUploadCreateV2:   2,
	endpointUploadCompleteV2: 0,
	endpointSHA1Reuse:        2,
}

// BaseResp 通用响应（123 所有 Open API 统一外层）
type BaseResp struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	XTraceID string `json:"x-traceID"`
}

// AccessTokenResp 开发者凭据换取 access_token 的响应
type AccessTokenResp struct {
	BaseResp
	Data struct {
		AccessToken string `json:"accessToken"`
		ExpiredAt   string `json:"expiredAt"`
	} `json:"data"`
}

// RenewTokenResp oplist renewapi 在线续期响应
type RenewTokenResp struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Code             int    `json:"code"`
	Message          string `json:"message"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Text             string `json:"text"`
}

// UserInfoResp 用户信息响应
type UserInfoResp struct {
	BaseResp
	Data struct {
		UID uint64 `json:"uid"`
		// Username    string `json:"username"`
		// DisplayName string `json:"displayName"`
		SpaceUsed      int64 `json:"spaceUsed"`
		SpacePermanent int64 `json:"spacePermanent"`
		SpaceTemp      int64 `json:"spaceTemp"`
	} `json:"data"`
}

// PanFile 123云盘文件/目录信息
//
// 注意：123 返回的时间无时区信息，按 UTC+8 解析（与 OpenList 一致）。
type PanFile struct {
	FileName     string `json:"filename"`
	Size         int64  `json:"size"`
	CreateAt     string `json:"createAt"`
	UpdateAt     string `json:"updateAt"`
	FileID       int64  `json:"fileId"`
	Type         int    `json:"type"` // 1=目录 2=文件
	Etag         string `json:"etag"`
	ParentFileID int64  `json:"parentFileId"`
	Trashed      int    `json:"trashed"`
}

// IsDir 是否为目录
func (f PanFile) IsDir() bool {
	return f.Type == 1
}

// FileListResp 文件列表响应
type FileListResp struct {
	BaseResp
	Data struct {
		LastFileID int64     `json:"lastFileId"`
		FileList   []PanFile `json:"fileList"`
	} `json:"data"`
}

// DownloadInfoResp 下载直链响应
type DownloadInfoResp struct {
	BaseResp
	Data struct {
		DownloadURL string `json:"downloadUrl"`
	} `json:"data"`
}

// MkdirResp 创建目录响应
//
// 注意字段名是 **dirID** 而非 fileId：123 的 /upload/v1/file/mkdir 返回
//
//	{"code":0,"message":"ok","data":{"dirID":65263931}}
//
// 历史实现把 tag 写成 `fileId`，导致 Data.FileID 恒为 0 —— resolve 拿到
// parent=0 继续往下建，月目录被建到根目录下、与年目录平级
// （2026-10-01 事故：根目录出现 `2026`+`10` 两组平级目录、记录路径
// /2026/10/2026/xxx.enc）。OpenList 的 mkdir 干脆不解析返回值
// （drivers/123_open/util.go:194-206 只返回 error），靠 findChild 回查，
// 天然规避该字段名差异。
type MkdirResp struct {
	BaseResp
	Data struct {
		DirID int64 `json:"dirID"`
	} `json:"data"`
}

// UploadCreateRespV2 创建上传任务响应
type UploadCreateRespV2 struct {
	BaseResp
	Data struct {
		FileID      int64    `json:"fileID"`
		PreuploadID string   `json:"preuploadID"`
		Reuse       bool     `json:"reuse"`
		SliceSize   int64    `json:"sliceSize"`
		Servers     []string `json:"servers"`
	} `json:"data"`
}

// UploadCompleteRespV2 分片合并响应
type UploadCompleteRespV2 struct {
	BaseResp
	Data struct {
		Completed bool  `json:"completed"`
		FileID    int64 `json:"fileID"`
	} `json:"data"`
}

// SHA1ReuseResp SHA1 秒传响应
type SHA1ReuseResp struct {
	BaseResp
	Data struct {
		FileID int64 `json:"fileID"`
		Reuse  bool  `json:"reuse"`
	} `json:"data"`
}

// TrashResp 批量删除响应
type TrashResp struct {
	BaseResp
}
