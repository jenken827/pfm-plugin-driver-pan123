package pan123

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/jenken827/pfm-plugin-sdk/driver/types"
	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk/logx"
)

// rootFileID 返回配置的根目录 fileId（默认 0 = 根目录）
func (d *Pan123Driver) rootFileID() int64 {
	if d.Config != nil {
		if v := d.Config.RootFileID; v != "" {
			if id, err := strconv.ParseInt(v, 10, 64); err == nil && id >= 0 {
				return id
			}
			logx.L().Warnf("123pan invalid root_file_id %q, fallback to 0", v)
		}
	}
	return 0
}

// dirBackend 把 AllocateUploadDir 的「路径」语义映射到 123 的 fileId 树：
// 路径形如 /2026/09，root 段由 root_file_id 决定，其余段逐级解析/创建。
type dirBackend struct {
	d *Pan123Driver
	// resolved 缓存 path → fileId，避免 AllocateUploadDir 对同一路径反复请求
	resolved map[string]int64
}

// List 实现 types.DirBackend
func (b *dirBackend) List(ctx context.Context, dir string) ([]types.DirEntry, error) {
	fileID, err := b.resolve(ctx, dir)
	if err != nil {
		return nil, err
	}
	files, err := b.d.getFiles(ctx, fileID)
	if err != nil {
		return nil, err
	}
	entries := make([]types.DirEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, types.DirEntry{Name: f.FileName, IsDir: f.IsDir()})
	}
	return entries, nil
}

// Mkdir 实现 types.DirBackend
func (b *dirBackend) Mkdir(ctx context.Context, dir string) error {
	_, err := b.resolve(ctx, dir)
	return err
}

// resolve 解析路径对应 fileId，缺失的中间目录会被创建
func (b *dirBackend) resolve(ctx context.Context, dir string) (int64, error) {
	if b.resolved == nil {
		b.resolved = make(map[string]int64)
	}
	if id, ok := b.resolved[dir]; ok {
		return id, nil
	}

	current := b.d.rootFileID()
	currentPath := "/"

	segments := splitPath(dir)
	for _, seg := range segments {
		// 缓存键必须是规范化路径：与函数入口的 dir（以及 AllocateUploadDir
		// 后续对同一路径的重复 resolve）保持同一键空间。历史实现写成
		// currentPath + seg，缺 "/" 分隔符——"/2026" 再拼 "10" 得到
		// "/202610"，与入口用的 "/2026/10" 不等价：该路径的解析结果无法被
		// 后续同路径调用复用，重复触发 findChild/mkdir，在123 盘上表现为
		// 目录层级错乱（2026-10-01 事故：记录路径 /2026/10/2026/xxx.enc）。
		childPath := pathJoin(currentPath, seg)
		if id, ok := b.resolved[childPath]; ok {
			current, currentPath = id, childPath
			continue
		}

		// 先查是否存在
		fileID, err := b.findChild(ctx, current, seg)
		if err != nil {
			return 0, err
		}
		if fileID == 0 {
			// 不存在则创建（并发下可能已存在，容忍失败后重查）
			created, cErr := b.d.mkdir(ctx, current, seg)
			if cErr != nil {
				// 并发创建同名目录会报错，重查一次确认
				created, err = b.findChild(ctx, current, seg)
				if err != nil {
					return 0, fmt.Errorf("create directory %s failed: %w", childPath, cErr)
				}
			}
			// 防御：mkdir 返回 0（响应字段名不符/服务端未回填 id）时回退查询。
			// 若把 0 当作 parent 继续下探，下一级目录会被建到根目录下，
			// 表现为层级错乱（2026-10-01 事故：根目录出现 2026/10 平级目录）。
			if created == 0 {
				created, err = b.findChild(ctx, current, seg)
				if err != nil {
					return 0, fmt.Errorf("create directory %s then lookup failed: %w", childPath, err)
				}
			}
			if created == 0 {
				return 0, fmt.Errorf("create directory %s failed: %w", childPath, cErr)
			}
			fileID = created
		}
		b.resolved[childPath] = fileID
		current, currentPath = fileID, childPath
	}
	return current, nil
}

// findChild 在父目录下按名称查找子项 fileId，不存在返回 0
func (b *dirBackend) findChild(ctx context.Context, parentFileID int64, name string) (int64, error) {
	files, err := b.d.getFiles(ctx, parentFileID)
	if err != nil {
		return 0, err
	}
	for _, f := range files {
		if f.FileName == name {
			return f.FileID, nil
		}
	}
	return 0, nil
}

// splitPath 拆分路径段（忽略空段与 "."）
func splitPath(p string) []string {
	var segments []string
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." {
			continue
		}
		segments = append(segments, seg)
	}
	return segments
}

// uploadFileRef 由 fileId/路径/大小/hash 组装单个 FileRef（pfm 按 ref 落库）
func uploadFileRef(fileID int64, fullPath, name string, size int64, hash string) types.FileRef {
	return types.FileRef{
		ProviderFileID: strconv.FormatInt(fileID, 10),
		Name:           name,
		Path:           fullPath,
		Size:           uint64(size),
		ChunkSize:      uint64(size),
		Hash:           hash,
	}
}

// tryRapidUpload 依次尝试 SHA1 秒传与 etag 秒传。
//
// 返回值：
//   - rapidRef != nil：秒传命中，直接用它作为结果
//   - createResp != nil：未命中秒传，但上传任务已创建，交给调用方继续上传分片
//   - 两者都为 nil 且 err != nil：请求失败
func (d *Pan123Driver) tryRapidUpload(ctx context.Context, dirFileID int64,
	fullPath, uploadName, md5Hex, sha1Hex string, fileSize int64) (
	rapidRef *types.FileRef, createResp *UploadCreateRespV2, err error) {

	// SHA1 秒传（命中则无需上传任何分片）
	if reuseResp, rErr := d.sha1Reuse(ctx, dirFileID, uploadName, sha1Hex, fileSize); rErr == nil {
		if reuseResp.Data.Reuse && reuseResp.Data.FileID != 0 {
			logx.C(ctx).Infof("123pan rapid upload success (sha1): %s, file_id: %d",
				uploadName, reuseResp.Data.FileID)
			ref := uploadFileRef(reuseResp.Data.FileID, fullPath, uploadName, fileSize, md5Hex)
			return &ref, nil, nil
		}
	} else {
		// 秒传检查失败不阻断：继续走 create + 分片上传
		logx.C(ctx).Debugf("123pan sha1 reuse check failed, fallback to upload: %v", rErr)
	}

	// 创建上传任务（etag=MD5，仍可能秒传）
	createResp, err = d.create(ctx, dirFileID, uploadName, md5Hex, fileSize)
	if err != nil {
		return nil, nil, fmt.Errorf("create upload task failed: %w", err)
	}
	if createResp.Data.Reuse && createResp.Data.FileID != 0 {
		logx.C(ctx).Infof("123pan rapid upload success (etag): %s, file_id: %d",
			uploadName, createResp.Data.FileID)
		ref := uploadFileRef(createResp.Data.FileID, fullPath, uploadName, fileSize, md5Hex)
		return &ref, nil, nil
	}
	// 未命中秒传：把 create 响应交给调用方继续上传分片
	return nil, createResp, nil
}

// uploadPrepared 分片上传 + 合并（create 未命中秒传时调用）
func (d *Pan123Driver) uploadPrepared(ctx context.Context, file *os.File,
	createResp *UploadCreateRespV2, fullPath, uploadName, md5Hex string, fileSize int64) ([]types.FileRef, error) {

	if err := d.uploadSlices(ctx, file, createResp, uploadName, fileSize); err != nil {
		return nil, fmt.Errorf("upload slices failed: %w", err)
	}

	fileID, err := d.complete(ctx, createResp.Data.PreuploadID)
	if err != nil {
		return nil, fmt.Errorf("complete upload failed: %w", err)
	}

	logx.C(ctx).Infof("123pan upload success: %s, file_id: %d, size: %d",
		uploadName, fileID, fileSize)
	return []types.FileRef{
		uploadFileRef(fileID, fullPath, uploadName, fileSize, md5Hex),
	}, nil
}

// Upload 上传文件（单文件，不做 pfm 侧分片——分片由 123 服务端按 sliceSize 切）
//
// 流程：加密准备 → 年/月目录分配 → 计算 MD5+SHA1 → SHA1 秒传 →
// create（etag 秒传）→ 上传分片 → complete 轮询。
func (d *Pan123Driver) Upload(ctx context.Context, src types.UploadSource, opt types.UploadOptions) ([]types.FileRef, error) {
	// 1. 准备上传内容（加密管道共享逻辑）
	uploadFilePath, uploadName, encSize, err := types.PackToTempFile(src.TempPath, src.FileName, opt)
	if err != nil {
		return nil, err
	}
	if uploadFilePath != src.TempPath {
		defer os.Remove(uploadFilePath)
	}
	fileSize := int64(encSize)

	// 123 不支持空文件
	if fileSize < 1 {
		return nil, fmt.Errorf("123pan does not allow empty files")
	}

	// 2. 分配物理目录（年/月/数字子目录）
	backend := &dirBackend{d: d}
	uploadDir, err := types.AllocateUploadDir(ctx, "/", backend)
	if err != nil {
		return nil, fmt.Errorf("allocate upload directory failed: %w", err)
	}
	dirFileID, err := backend.resolve(ctx, uploadDir)
	if err != nil {
		return nil, fmt.Errorf("resolve upload directory failed: %w", err)
	}
	fullPath := pathJoin(uploadDir, uploadName)

	// 3. 打开文件并计算摘要
	file, err := os.Open(uploadFilePath)
	if err != nil {
		return nil, fmt.Errorf("open upload file failed: %w", err)
	}
	defer file.Close()

	md5Hex, sha1Hex, err := computeFileHashes(file)
	if err != nil {
		return nil, err
	}

	// 4. 秒传（SHA1 → etag），未命中则继续分片上传
	rapidRef, createResp, err := d.tryRapidUpload(ctx, dirFileID, fullPath, uploadName,
		md5Hex, sha1Hex, fileSize)
	if err != nil {
		return nil, err
	}
	if rapidRef != nil {
		return []types.FileRef{*rapidRef}, nil
	}

	// create 未命中秒传 → 上传分片 + 合并
	return d.uploadPrepared(ctx, file, createResp, fullPath, uploadName, md5Hex, fileSize)
}

// Open 打开文件（支持分片合并与 Range 读取）
func (d *Pan123Driver) Open(ctx context.Context, refs []types.FileRef, opt types.OpenOptions) (types.ReadSeekCloser, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("no file refs provided")
	}

	sort.Slice(refs, func(i, j int) bool {
		return refs[i].ChunkNumber < refs[j].ChunkNumber
	})

	return types.NewMultiChunkReader(ctx, d, refs, opt.Encryptor)
}

// OpenChunk 实现 types.ChunkOpener：Range 下载指定分片
func (d *Pan123Driver) OpenChunk(ctx context.Context, ref types.FileRef, offset int64) (io.ReadCloser, error) {
	if ref.ProviderFileID == "" {
		return nil, fmt.Errorf("provider_file_id is empty")
	}
	fileID, err := strconv.ParseInt(ref.ProviderFileID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid fileId %q: %w", ref.ProviderFileID, err)
	}

	downloadURL, err := d.getDownloadLink(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("get download link failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed: status=%d", resp.StatusCode)
	}

	// 服务端忽略 Range（返回 200）时手动丢弃前置字节
	if offset > 0 && resp.StatusCode == http.StatusOK {
		if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("skip bytes failed: %w", err)
		}
	}
	return resp.Body, nil
}

// OpenChunkRangeSeek 实现 types.ChunkOpener：供加密文件解密使用
func (d *Pan123Driver) OpenChunkRangeSeek(ctx context.Context, ref types.FileRef) types.OpenRangeSeek {
	return func(ctx context.Context, offset, limit int64) (io.ReadCloser, error) {
		// 加密文件必须从头读，让上层 Decrypt 处理 seek
		rc, err := d.OpenChunk(ctx, ref, 0)
		if err != nil {
			return nil, err
		}
		if offset > 0 {
			if seeker, ok := rc.(io.Seeker); ok {
				if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
					rc.Close()
					return nil, fmt.Errorf("seek failed: %w", err)
				}
			} else {
				if _, err := io.CopyN(io.Discard, rc, offset); err != nil {
					rc.Close()
					return nil, fmt.Errorf("skip bytes failed: %w", err)
				}
			}
		}
		if limit > 0 {
			return io.NopCloser(io.LimitReader(rc, limit)), nil
		}
		return rc, nil
	}
}

// BatchDelete 实现 types.BatchDeleter：按 fileId 移入回收站
func (d *Pan123Driver) BatchDelete(ctx context.Context, refs []types.FileRef) ([]types.DeleteResult, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	results := make([]types.DeleteResult, len(refs))

	fileIDs := make([]int64, 0, len(refs))
	indexed := make(map[int64][]int, len(refs))
	for i, ref := range refs {
		results[i] = types.DeleteResult{ID: ref.LocationID, Success: false}
		if ref.ProviderFileID == "" {
			results[i].Error = fmt.Errorf("provider_file_id is empty")
			continue
		}
		fileID, err := strconv.ParseInt(ref.ProviderFileID, 10, 64)
		if err != nil {
			results[i].Error = fmt.Errorf("invalid fileId %q: %w", ref.ProviderFileID, err)
			continue
		}
		fileIDs = append(fileIDs, fileID)
		indexed[fileID] = append(indexed[fileID], i)
	}

	if len(fileIDs) == 0 {
		// 全部 fileId 非法：返回失败结果供上层标 DELETED_FAILED
		return results, nil
	}

	if err := d.trash(ctx, fileIDs); err != nil {
		logx.C(ctx).Errorf("123pan batch delete failed: %v", err)
		for i := range refs {
			results[i].Error = err
		}
		return results, nil
	}

	for fileID, idxs := range indexed {
		for _, i := range idxs {
			results[i].Success = true
		}
		logx.C(ctx).Debugf("123pan trashed file_id: %d", fileID)
	}
	logx.C(ctx).Infof("123pan batch delete success: %d/%d files",
		countSuccess(results), len(results))
	return results, nil
}

// countSuccess 统计成功删除数
func countSuccess(results []types.DeleteResult) int {
	n := 0
	for _, r := range results {
		if r.Success {
			n++
		}
	}
	return n
}

// Close 实现 types.Closer（123 无需释放资源，保留接口一致性）
func (d *Pan123Driver) Close() {}
