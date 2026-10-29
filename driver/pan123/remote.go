package pan123

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jenken827/pfm-plugin-sdk/driver/types"
)

var _ types.RemoteLister = (*Pan123Driver)(nil)

// RemoteRoot 实现 types.RemoteLister：pfm 在 123 盘上的逻辑根（由 root_file_id 映射）
func (d *Pan123Driver) RemoteRoot() string {
	return "/"
}

// resolveExistingDir 解析已存在目录的 fileId（缺目录直接报错，不创建）
func (d *Pan123Driver) resolveExistingDir(ctx context.Context, dir string) (int64, error) {
	b := &dirBackend{d: d}
	cur := d.rootFileID()
	for _, seg := range splitPath(dir) {
		id, err := b.findChild(ctx, cur, seg)
		if err != nil {
			return 0, err
		}
		if id == 0 {
			return 0, fmt.Errorf("目录不存在: %s", dir)
		}
		cur = id
	}
	return cur, nil
}

// ListRemote 实现 types.RemoteLister：列出 123 盘目录下的直接子项。
// Path 与上传时一致（pathJoin(dir, name)，保留前导 "/"），可直接与 file_locations.path 比对。
func (d *Pan123Driver) ListRemote(ctx context.Context, dir string) ([]types.RemoteEntry, error) {
	dirID, err := d.resolveExistingDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	files, err := d.getFiles(ctx, dirID)
	if err != nil {
		return nil, err
	}
	out := make([]types.RemoteEntry, 0, len(files))
	for _, f := range files {
		out = append(out, types.RemoteEntry{
			Path:  pathJoin(dir, f.FileName),
			Name:  f.FileName,
			ID:    strconv.FormatInt(f.FileID, 10),
			IsDir: f.IsDir(),
			Size:  f.Size,
		})
	}
	return out, nil
}
