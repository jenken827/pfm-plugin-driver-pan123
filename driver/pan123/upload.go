package pan123

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk/logx"
)

// sha1Reuse 尝试 SHA1 秒传（123 侧命中已存在内容时直接返回 fileId）
func (d *Pan123Driver) sha1Reuse(ctx context.Context, parentFileID int64, filename, sha1Hash string, size int64) (*SHA1ReuseResp, error) {
	body, err := d.requestJSON(ctx, http.MethodPost, endpointSHA1Reuse, nil, map[string]any{
		"parentFileID": parentFileID,
		"filename":     filename,
		"sha1":         strings.ToLower(sha1Hash),
		"size":         size,
		// duplicate=2：同名文件自动重命名（OpenList 同值），避免覆盖已有对象
		"duplicate": 2,
	})
	if err != nil {
		return nil, err
	}
	var resp SHA1ReuseResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// create 创建上传任务（带 etag，命中秒传时 reuse=true 且 fileID 有效）
func (d *Pan123Driver) create(ctx context.Context, parentFileID int64, filename, etag string, size int64) (*UploadCreateRespV2, error) {
	body, err := d.requestJSON(ctx, http.MethodPost, endpointUploadCreateV2, nil, map[string]any{
		"parentFileId": parentFileID,
		"filename":     filename,
		"etag":         strings.ToLower(etag),
		"size":         size,
		"duplicate":    2,
		"containDir":   false,
	})
	if err != nil {
		return nil, err
	}
	var resp UploadCreateRespV2
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// uploadSlices 并发上传分片
//
// 123 的分片接口是 multipart/form-data：preuploadID / sliceNo / sliceMD5 三个
// 表单字段 + 文件字段 slice。上传域名来自 create 返回的 servers[0]。
func (d *Pan123Driver) uploadSlices(ctx context.Context, file *os.File, createResp *UploadCreateRespV2, fileName string, fileSize int64) error {
	if len(createResp.Data.Servers) == 0 {
		return fmt.Errorf("123pan upload create returned no server")
	}
	uploadDomain := createResp.Data.Servers[0]
	sliceSize := createResp.Data.SliceSize
	if sliceSize <= 0 {
		return fmt.Errorf("123pan upload create returned invalid sliceSize: %d", sliceSize)
	}

	sliceCount := (fileSize + sliceSize - 1) / sliceSize
	threads := d.uploadThreads()
	if int64(threads) > sliceCount {
		threads = int(sliceCount)
	}

	sem := make(chan struct{}, threads)
	var wg sync.WaitGroup
	errChan := make(chan error, sliceCount)

	for i := int64(0); i < sliceCount; i++ {
		offset := i * sliceSize
		size := sliceSize
		if offset+size > fileSize {
			size = fileSize - offset
		}
		sliceNo := i + 1 // 123 的分片号从 1 开始

		wg.Add(1)
		go func(sliceNo, offset, size int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// 每个分片单独取 token：大文件上传可能持续很久，
			// 开头取的 token 可能在途中过期（与 OpenList 一致）。
			token, err := d.tm.getAccessToken(ctx, d.creds, false)
			if err != nil {
				errChan <- fmt.Errorf("slice %d get token failed: %w", sliceNo, err)
				return
			}

			if err := d.uploadSlice(ctx, uploadDomain, token, file,
				createResp.Data.PreuploadID, fileName, sliceNo, offset, size); err != nil {
				errChan <- fmt.Errorf("upload slice %d failed: %w", sliceNo, err)
				return
			}
			logx.C(ctx).Debugf("123pan uploaded slice %d/%d", sliceNo, sliceCount)
		}(sliceNo, offset, size)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		return err
	}
	return nil
}

// uploadSlice 上传单个分片
func (d *Pan123Driver) uploadSlice(ctx context.Context, uploadDomain, token string,
	file *os.File, preuploadID, fileName string, sliceNo, offset, size int64) error {

	// 分片 MD5（123 要求每个分片单独提供 sliceMD5）
	section := io.NewSectionReader(file, offset, size)
	hasher := md5.New()
	if _, err := io.Copy(hasher, section); err != nil {
		return fmt.Errorf("calculate slice md5 failed: %w", err)
	}
	sliceMD5 := hex.EncodeToString(hasher.Sum(nil))

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	if err := writer.WriteField("preuploadID", preuploadID); err != nil {
		return err
	}
	if err := writer.WriteField("sliceNo", strconv.FormatInt(sliceNo, 10)); err != nil {
		return err
	}
	if err := writer.WriteField("sliceMD5", sliceMD5); err != nil {
		return err
	}

	part, err := writer.CreateFormFile("slice", fmt.Sprintf("%s.part%d", fileName, sliceNo))
	if err != nil {
		return err
	}
	reader := io.NewSectionReader(file, offset, size)
	if _, err := io.Copy(part, reader); err != nil {
		return fmt.Errorf("write slice body failed: %w", err)
	}
	contentType := writer.FormDataContentType()
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		uploadDomain+endpointUploadSliceV2, bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("platform", platformHeader)

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("slice %d request failed: %w", sliceNo, err)
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("read slice %d response failed: %w", sliceNo, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slice %d upload failed, status=%d, body=%s",
			sliceNo, resp.StatusCode, truncateBody(string(respBody), 256))
	}

	var parsed BaseResp
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return fmt.Errorf("parse slice %d response failed: %w", sliceNo, err)
	}
	if parsed.Code != 0 {
		// token 可能在长上传中途过期，返回错误让上层重试整体上传
		return fmt.Errorf("slice %d upload failed: code=%d, message=%s", sliceNo, parsed.Code, parsed.Message)
	}
	return nil
}

// complete 合并分片（123 需要轮询直到 completed=true）
//
// 服务端在合并期间返回 code=20103（文件正在校验中，请间隔1秒后再试）——
// 这是中间状态，必须继续轮询而非当失败终止（OpenList driver.go:233-248
// 同样是忽略一切错误继续轮询）。
//
// 轮询采用**线性退避**：1s、2s、3s、4s……直到 maxPollDelay 封顶。
// 官方只说「间隔1秒后再试」，固定 1s 会在服务端校验耗时较久时持续高频
// 打接口；指数退避（1/2/4/8…）在长尾时等待过久，线性递增兼顾两者：
// 既满足"至少间隔 1s"，又让总时长可控。
func (d *Pan123Driver) complete(ctx context.Context, preuploadID string) (int64, error) {
	const (
		maxPoll       = 60
		minPollDelay  = time.Second
		pollDelayStep = time.Second
		maxPollDelay  = 10 * time.Second
	)

	var lastErr error
	delay := minPollDelay
	for i := 0; i < maxPoll; i++ {
		body, err := d.requestJSON(ctx, http.MethodPost, endpointUploadCompleteV2, nil, map[string]any{
			"preuploadID": preuploadID,
		})
		if err != nil {
			if isPan123Code(err, Pan123CodeVerifying) {
				// 校验中：记录并退避后继续轮询
				lastErr = err
				if !sleepOrDone(ctx, delay) {
					return 0, ctx.Err()
				}
				delay = nextPollDelay(delay, pollDelayStep, maxPollDelay)
				continue
			}
			return 0, err
		}
		var resp UploadCompleteRespV2
		if err := json.Unmarshal(body, &resp); err != nil {
			return 0, err
		}
		if resp.Data.Completed && resp.Data.FileID != 0 {
			return resp.Data.FileID, nil
		}
		// completed=false 时退避后继续轮询（官方要求至少间隔 1 秒）
		if !sleepOrDone(ctx, delay) {
			return 0, ctx.Err()
		}
		delay = nextPollDelay(delay, pollDelayStep, maxPollDelay)
	}
	if lastErr != nil {
		return 0, fmt.Errorf("123pan upload complete timeout after %d polls (last: %w)", maxPoll, lastErr)
	}
	return 0, fmt.Errorf("123pan upload complete timeout after %d polls", maxPoll)
}

// nextPollDelay 线性退避：每次加 step 但不超过 max
func nextPollDelay(cur, step, max time.Duration) time.Duration {
	next := cur + step
	if next > max {
		return max
	}
	return next
}

// sleepOrDone 等待 d 或 ctx 结束；返回 false 表示 ctx 已取消
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// uploadThreads 解析上传并发数（1-32，默认 3）
func (d *Pan123Driver) uploadThreads() int {
	if d.Config != nil {
		if t := int(d.Config.UploadThread.Int()); t >= 1 && t <= 32 {
			return t
		}
	}
	return 3
}

// computeFileHashes 计算文件的 MD5（create 用 etag）与 SHA1（秒传用）。
// 一次顺序读取同时产出两个摘要。
func computeFileHashes(file *os.File) (md5Hex, sha1Hex string, err error) {
	md5Hash := md5.New()
	sha1Hash := sha1.New()

	if _, err := io.Copy(io.MultiWriter(md5Hash, sha1Hash), file); err != nil {
		return "", "", fmt.Errorf("calculate file hashes failed: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", fmt.Errorf("seek file failed: %w", err)
	}
	return hex.EncodeToString(md5Hash.Sum(nil)),
		hex.EncodeToString(sha1Hash.Sum(nil)), nil
}
