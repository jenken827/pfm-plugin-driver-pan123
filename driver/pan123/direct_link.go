package pan123

import (
	"context"
	"fmt"
	"strconv"

	"jenken827.com/jenken827/pfm/driver/types"
)

// 编译期断言：驱动改签名时在这里直接失败，而不是静默失去直链能力
var _ types.DirectLinker = (*Pan123Driver)(nil)

// DirectLink 实现 types.DirectLinker：返回 123 云盘的下载直链
//
// 与 OpenChunk 共用 getDownloadLink（端点级限速）。直链 URL 自包含鉴权，
// 无额外请求头；有效期接口未给出，置零让调用方按"尽快使用"处理。
func (d *Pan123Driver) DirectLink(ctx context.Context, ref types.FileRef) (*types.DirectLink, error) {
	if ref.ProviderFileID == "" {
		return nil, fmt.Errorf("provider_file_id is empty")
	}

	fileID, err := strconv.ParseInt(ref.ProviderFileID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid fileId %q: %w", ref.ProviderFileID, err)
	}

	url, err := d.getDownloadLink(ctx, fileID)
	if err != nil {
		return nil, err
	}

	return &types.DirectLink{
		URL:           url,
		SupportsRange: true,
	}, nil
}
