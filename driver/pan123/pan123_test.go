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
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jenken827.com/jenken827/pfm/driver/types"
	"jenken827.com/jenken827/pfm/proto/gen/entity"
	"jenken827.com/jenken827/pfm/proto/gen/native"
	"jenken827.com/jenken827/pfm/utils"
)

// fakeNode 假服务器上的文件/目录节点
type fakeNode struct {
	id       int64
	name     string
	parentID int64
	isDir    bool
	content  []byte
}

// fake123 是 123 开放平台的本地假实现，用于端到端验证驱动链路。
//
// 覆盖：access_token、renewapi、user/info、file/list、file/mkdir、sha1_reuse、
// upload/v2/file/create、upload/v2/file/slice、upload_complete、download_info、file/trash。
type fake123 struct {
	mu sync.Mutex

	baseURL    string // 假服务器地址（start 时填充）
	nextID     int64
	nodes      map[int64]*fakeNode
	trashed    map[int64]bool
	slices     map[string]map[int]string // preuploadID → sliceNo → md5
	sliceData  map[string]map[int][]byte
	createCall int
	tokenCall  int
	renewCall  int

	// mkdirCalls 记录 mkdir 的 (parentID, name) 调用序列，用于诊断目录层级
	mkdirCalls []MkdirCall

	requireToken bool
}

// MkdirCall 一次 mkdir 调用的入参
type MkdirCall struct {
	ParentID int64
	Name     string
}

func newFake123() *fake123 {
	return &fake123{
		nextID:    1000,
		nodes:     map[int64]*fakeNode{},
		trashed:   map[int64]bool{},
		slices:    map[string]map[int]string{},
		sliceData: map[string]map[int][]byte{},
	}
}

// writeJSON 输出统一响应格式
func (f *fake123) writeJSON(w http.ResponseWriter, code int, message string, data any) {
	resp := map[string]any{"code": code, "message": message, "x-traceID": "trace-test"}
	if data != nil {
		resp["data"] = data
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// start 启动假服务器并替换包级端点，返回清理函数
func (f *fake123) start(t *testing.T) func() {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.route))
	oldBase, oldRenew := APIBase, RenewAPIDefault
	f.baseURL = srv.URL
	APIBase = srv.URL + "/open-api"
	RenewAPIDefault = srv.URL + "/renewapi"
	return func() {
		APIBase, RenewAPIDefault = oldBase, oldRenew
		srv.Close()
	}
}

func (f *fake123) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, endpointAccessToken):
		f.handleAccessToken(w, r)
	case strings.HasSuffix(path, "/renewapi"):
		f.handleRenew(w, r)
	case strings.HasPrefix(path, "/download/"):
		f.handleDownload(w, r)
	case strings.HasSuffix(path, endpointUserInfo):
		f.guardAuth(w, r, f.handleUserInfo)
	case strings.HasSuffix(path, endpointFileList):
		f.guardAuth(w, r, f.handleFileList)
	case strings.HasSuffix(path, endpointMkdir):
		f.guardAuth(w, r, f.handleMkdir)
	case strings.HasSuffix(path, endpointSHA1Reuse):
		f.guardAuth(w, r, f.handleSHA1Reuse)
	case strings.HasSuffix(path, endpointUploadCreateV2):
		f.guardAuth(w, r, f.handleUploadCreate)
	case strings.HasSuffix(path, endpointUploadSliceV2):
		f.guardAuth(w, r, f.handleUploadSlice)
	case strings.HasSuffix(path, endpointUploadCompleteV2):
		f.guardAuth(w, r, f.handleUploadComplete)
	case strings.HasSuffix(path, endpointDownloadInfo):
		f.guardAuth(w, r, f.handleDownloadInfo)
	case strings.HasSuffix(path, endpointTrash):
		f.guardAuth(w, r, f.handleTrash)
	default:
		http.NotFound(w, r)
	}
}

// guardAuth 校验 Authorization 头后再交给处理器
func (f *fake123) guardAuth(w http.ResponseWriter, r *http.Request, h func(http.ResponseWriter, *http.Request)) {
	if f.requireToken {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || len(auth) <= len("Bearer ") {
			f.writeJSON(w, 401, "missing token", nil)
			return
		}
	}
	h(w, r)
}

// allocID 分配新节点 id（调用方须已持锁）
func (f *fake123) allocID() int64 {
	f.nextID++
	return f.nextID
}

// childByName 在父目录下按名称查找节点（调用方须已持锁）
func (f *fake123) childByName(parentID int64, name string) *fakeNode {
	for _, n := range f.nodes {
		if n.parentID == parentID && n.name == name && !f.trashed[n.id] {
			return n
		}
	}
	return nil
}

func (f *fake123) handleAccessToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokenCall++
	f.mu.Unlock()

	var body struct {
		ClientID     string `json:"clientID"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.writeJSON(w, 400, "bad body", nil)
		return
	}
	if body.ClientID != "test-client" || body.ClientSecret != "test-secret" {
		f.writeJSON(w, 1000, "invalid client credentials", nil)
		return
	}
	f.writeJSON(w, 0, "success", map[string]any{
		"accessToken": "token-from-developer-api",
		"expiredAt":   "2099-01-01T00:00:00Z",
	})
}

func (f *fake123) handleRenew(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.renewCall++
	f.mu.Unlock()

	if r.URL.Query().Get("refresh_ui") == "" {
		f.writeJSON(w, 400, "missing refresh_ui", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  "token-from-renew-api",
		"refresh_token": "refresh-token-rotated",
		"expires_in":    3600,
	})
}

func (f *fake123) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	f.writeJSON(w, 0, "success", map[string]any{
		"uid":            uint64(88888),
		"spaceUsed":      1024,
		"spacePermanent": 1099511627776,
		"spaceTemp":      0,
	})
}

func (f *fake123) handleFileList(w http.ResponseWriter, r *http.Request) {
	parentID, _ := strconv.ParseInt(r.URL.Query().Get("parentFileId"), 10, 64)
	lastFileID, _ := strconv.ParseInt(r.URL.Query().Get("lastFileId"), 10, 64)

	f.mu.Lock()
	defer f.mu.Unlock()

	// 仅支持单页：第一页返回全部子项并给 lastFileId=-1 表示结束
	if lastFileID != 0 {
		f.writeJSON(w, 0, "success", map[string]any{"lastFileId": -1, "fileList": []any{}})
		return
	}

	list := []map[string]any{}
	for _, n := range f.nodes {
		if n.parentID != parentID || n.isDir == false && false {
			continue
		}
		if !n.isDir || f.trashed[n.id] {
			continue
		}
		list = append(list, map[string]any{
			"filename":     n.name,
			"fileId":       n.id,
			"type":         1,
			"size":         0,
			"parentFileId": n.parentID,
			"trashed":      0,
		})
	}
	f.writeJSON(w, 0, "success", map[string]any{"lastFileId": -1, "fileList": list})
}

func (f *fake123) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ParentID int64  `json:"parentID"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.writeJSON(w, 400, "bad body", nil)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.mkdirCalls = append(f.mkdirCalls, MkdirCall{ParentID: body.ParentID, Name: body.Name})
	id := f.allocID()
	f.nodes[id] = &fakeNode{id: id, name: body.Name, parentID: body.ParentID, isDir: true}
	// 与真实 123 服务端一致：字段名是 dirID 而非 fileId
	// （历史驱动曾按 fileId 解析导致恒为 0，月目录被建到根目录下）
	f.writeJSON(w, 0, "success", map[string]any{"dirID": id})
}

func (f *fake123) handleSHA1Reuse(w http.ResponseWriter, r *http.Request) {
	// 默认不命中秒传
	f.writeJSON(w, 0, "success", map[string]any{"reuse": false, "fileID": 0})
}

func (f *fake123) handleUploadCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filename string `json:"filename"`
		ParentID int64  `json:"parentFileId"`
		Size     int64  `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.writeJSON(w, 400, "bad body", nil)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCall++
	f.writeJSON(w, 0, "success", map[string]any{
		"fileID":      0,
		"preuploadID": "preupload-test-1",
		"reuse":       false,
		// 8 字节分片：便于验证多分片 + 并发
		"sliceSize": 8,
		"servers":   []string{f.baseURL + "/upload"},
	})
}

func (f *fake123) handleUploadSlice(w http.ResponseWriter, r *http.Request) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		http.Error(w, "bad multipart", http.StatusBadRequest)
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])

	var preuploadID, sliceMD5 string
	var sliceNo int
	var content []byte

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "multipart read failed", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(part)
		if err != nil {
			http.Error(w, "part read failed", http.StatusBadRequest)
			return
		}
		switch part.FormName() {
		case "preuploadID":
			preuploadID = string(data)
		case "sliceNo":
			sliceNo, _ = strconv.Atoi(string(data))
		case "sliceMD5":
			sliceMD5 = string(data)
		case "slice":
			content = data
		}
		part.Close()
	}

	// 123 服务端会校验 sliceMD5 与实际内容一致
	sum := md5.Sum(content)
	if got := hex.EncodeToString(sum[:]); got != sliceMD5 {
		f.writeJSON(w, 20103, "sliceMD5 mismatch: got "+sliceMD5+" want "+got, nil)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.slices[preuploadID] == nil {
		f.slices[preuploadID] = map[int]string{}
		f.sliceData[preuploadID] = map[int][]byte{}
	}
	f.slices[preuploadID][sliceNo] = sliceMD5
	f.sliceData[preuploadID][sliceNo] = content
	f.writeJSON(w, 0, "success", nil)
}

func (f *fake123) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PreuploadID string `json:"preuploadID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.writeJSON(w, 400, "bad body", nil)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	data := f.sliceData[body.PreuploadID]
	if len(data) == 0 {
		f.writeJSON(w, 20103, "no slices uploaded", nil)
		return
	}
	var merged []byte
	for i := 1; i <= len(data); i++ {
		merged = append(merged, data[i]...)
	}

	id := f.allocID()
	f.nodes[id] = &fakeNode{id: id, name: "uploaded.bin", parentID: 0, content: merged}
	f.writeJSON(w, 0, "success", map[string]any{"completed": true, "fileID": id})
}

func (f *fake123) handleDownloadInfo(w http.ResponseWriter, r *http.Request) {
	fileID, _ := strconv.ParseInt(r.URL.Query().Get("fileId"), 10, 64)

	f.mu.Lock()
	n, ok := f.nodes[fileID]
	f.mu.Unlock()
	if !ok {
		f.writeJSON(w, 1004, "file not found", nil)
		return
	}
	f.writeJSON(w, 0, "success", map[string]any{
		"downloadUrl": f.baseURL + "/download/" + strconv.FormatInt(n.id, 10),
	})
}

// handleDownload 提供文件内容，支持 Range（验证 OpenChunk 的偏移读取）
func (f *fake123) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/download/"), 10, 64)

	f.mu.Lock()
	n, ok := f.nodes[id]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	data := n.content

	if rangeHeader := r.Header.Get("Range"); strings.HasPrefix(rangeHeader, "bytes=") {
		var start int64
		fmt.Sscanf(rangeHeader, "bytes=%d-", &start)
		if start > int64(len(data)) {
			start = int64(len(data))
		}
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start:])
		return
	}
	_, _ = w.Write(data)
}

func (f *fake123) handleTrash(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FileIDs []int64 `json:"fileIDs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.writeJSON(w, 400, "bad body", nil)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range body.FileIDs {
		f.trashed[id] = true
	}
	f.writeJSON(w, 0, "success", nil)
}

// fileContent 读取假服务器上的文件内容
func (f *fake123) fileContent(fileID int64) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[fileID]
	if !ok {
		return nil, false
	}
	return n.content, true
}

// newTestDriver 用给定配置构造指向假服务器的驱动
func newTestDriver(cfg *native.Pan123DriverConfig) *Pan123Driver {
	return &Pan123Driver{
		Config:         cfg,
		ProviderConfig: types.ProviderConfig{},
		creds: credentials{
			clientID:     cfg.GetClientId(),
			clientSecret: cfg.GetClientSecret(),
			apiURL:       cfg.GetApiUrl(),
		},
		tm:     initTokenManager(cfg),
		client: newHTTPClient(),
	}
}

// TestUploadDirParentage 断言年/月目录的父子挂载关系正确。
//
// 2026-10-01 生产事故：DB 记录的路径为
//
//	/2026/10/2026/fd603e41_20261001143219.enc
//
// 即 "2026" 出现两次。本用例直接把 123 的 mkdir(parentID, name) 调用序列
// 打出来，并断言月目录 "10" 的 parentID 必须是年目录 "2026" 的 fileId，
// 而不是根目录（parentID=0）——后者会让 2026 与 10 变成同级。
func TestUploadDirParentage(t *testing.T) {
	fake := newFake123()
	cleanup := fake.start(t)
	defer cleanup()

	d := newTestDriver(&native.Pan123DriverConfig{
		RefreshToken: utils.StringPtr("rt1"),
		RootFileId:   utils.StringPtr("0"),
		ApiUrl:       utils.StringPtr(fake.baseURL + "/renewapi"),
	})
	if err := d.warmup(); err != nil {
		t.Fatalf("warmup failed: %v", err)
	}

	content := []byte("parentage-probe-payload-0123456789")
	srcPath := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(srcPath, content, 0o600); err != nil {
		t.Fatalf("write source failed: %v", err)
	}
	if _, err := d.Upload(context.Background(), types.UploadSource{
		TempPath: srcPath, FileName: "probe.txt", FileSize: uint64(len(content)),
	}, types.UploadOptions{}); err != nil {
		t.Fatalf("upload failed: %v", err)
	}

	fake.mu.Lock()
	calls := append([]MkdirCall(nil), fake.mkdirCalls...)
	fake.mu.Unlock()

	year, month := time.Now().Format("2006"), time.Now().Format("01")
	var monthParent int64
	monthFound := false
	for _, c := range calls {
		if c.Name == month {
			monthParent, monthFound = c.ParentID, true
		}
	}
	if !monthFound {
		t.Fatalf("月目录 %q 的 mkdir 未发生，实际调用序列: %+v", month, calls)
	}
	if monthParent == 0 {
		t.Errorf("月目录 %q 的 parentID=0（挂在根目录，年/月将变成同级）；实际调用序列: %+v", month, calls)
	}
	if len(calls) == 0 || calls[0].Name != year {
		t.Errorf("首个 mkdir 应为年目录 %q，实际序列: %+v", year, calls)
	}
}

// TestUploadDirParentageExistingYear 年目录已存在、月目录新建时的挂载关系。
//
// 覆盖 2026-10-01 生产事故的真实前提：123 盘上 /2026 已存在（此前其他上传
// 建过），本次需要新建月目录 10。若 resolve 的缓存键缺少 "/" 分隔符
// （driver.go:75childPath := currentPath + seg），"/2026/10" 的第二段会
// 算成 "/202610"，与前一次 Mkdir("/2026") 写入的键 "/2026" 不衔接，
// 从而在错误的位置查找/创建月目录。
func TestUploadDirParentageExistingYear(t *testing.T) {
	fake := newFake123()
	cleanup := fake.start(t)
	defer cleanup()

	// 预置：根目录下已存在名为 2026 的年目录
	year := time.Now().Format("2006")
	fake.mu.Lock()
	yearID := fake.allocID()
	fake.nodes[yearID] = &fakeNode{id: yearID, name: year, parentID: 0, isDir: true}
	fake.mu.Unlock()

	d := newTestDriver(&native.Pan123DriverConfig{
		RefreshToken: utils.StringPtr("rt1"),
		RootFileId:   utils.StringPtr("0"),
		ApiUrl:       utils.StringPtr(fake.baseURL + "/renewapi"),
	})
	if err := d.warmup(); err != nil {
		t.Fatalf("warmup failed: %v", err)
	}

	content := []byte("existing-year-probe-payload-0123456789")
	srcPath := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(srcPath, content, 0o600); err != nil {
		t.Fatalf("write source failed: %v", err)
	}
	refs, err := d.Upload(context.Background(), types.UploadSource{
		TempPath: srcPath, FileName: "probe.txt", FileSize: uint64(len(content)),
	}, types.UploadOptions{})
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}

	month := time.Now().Format("01")
	fake.mu.Lock()
	var monthID, monthParent int64 = -1, -1
	for id, n := range fake.nodes {
		if n.isDir && n.name == month {
			monthID, monthParent = id, n.parentID
		}
	}
	calls := append([]MkdirCall(nil), fake.mkdirCalls...)
	fake.mu.Unlock()

	if monthID < 0 {
		t.Fatalf("月目录 %q 未创建；mkdir 序列: %+v", month, calls)
	}
	if monthParent != yearID {
		t.Errorf("月目录 %q 的 parentID=%d，应为年目录 %q 的 fileId=%d；mkdir 序列: %+v",
			month, monthParent, year, yearID, calls)
	}
	if got := refs[0].Path; strings.Contains(got, "/"+year+"/"+year+"/") {
		t.Errorf("ref.Path 出现重复年目录段: %q", got)
	}
}

// TestResolveCacheKeysAreNormalized 断言 resolve 的缓存键与传入路径同一
// 键空间（2026-10-01 事故回归守门）。
//
// 历史 bug：childPath := currentPath + seg 缺 "/" 分隔符，"/2026/10" 的
// 末段键写成 "/202610"。入口第66 行用完整 dir 查缓存，两套键不等价 →
// 同路径重复解析、重复 findChild/mkdir，123 盘上层级错乱。
func TestResolveCacheKeysAreNormalized(t *testing.T) {
	fake := newFake123()
	cleanup := fake.start(t)
	defer cleanup()

	d := newTestDriver(&native.Pan123DriverConfig{
		RefreshToken: utils.StringPtr("rt1"),
		RootFileId:   utils.StringPtr("0"),
		ApiUrl:       utils.StringPtr(fake.baseURL + "/renewapi"),
	})
	if err := d.warmup(); err != nil {
		t.Fatalf("warmup failed: %v", err)
	}

	b := &dirBackend{d: d}
	// 按 AllocateUploadDir 的真实调用顺序驱动
	for _, dir := range []string{"/", "/2026", "/2026/10", "/2026/10"} {
		if _, err := b.resolve(context.Background(), dir); err != nil {
			t.Fatalf("resolve(%q) failed: %v", dir, err)
		}
	}

	for _, want := range []string{"/2026", "/2026/10"} {
		if _, ok := b.resolved[want]; !ok {
			t.Errorf("缓存缺少规范化键 %q，实际键集合: %v", want, mapKeys(b.resolved))
		}
	}
	for k := range b.resolved {
		if k != "/" && !strings.HasPrefix(k, "/") {
			t.Errorf("缓存键 %q 缺少前导 /", k)
		}
		// 规范化键不应出现 "/202610" 这类粘连
		if strings.Contains(k, "202610") {
			t.Errorf("缓存键 %q 出现路径段粘连（缺 / 分隔符）", k)
		}
	}
}

func mapKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// fakePersister 记录写入的配置 JSON
type fakePersister struct {
	last string
	n    int
}

func (f *fakePersister) UpdateConfig(_ uint32, cfgJSON string) error {
	f.last = cfgJSON
	f.n++
	return nil
}

// TestHandFilledAccessTokenTakesPriority 断言手填 access_token 优先于续期：
// 即使配置了 refresh_token，也不清空、不立即续期（2026-10-01 事故回归守门）。
//
// 历史缺陷：initTokenManager 只要配了 refresh_token 就无条件
// `tm.accessToken = ""`，warmup 立即走续期；续期路径不可用时（renewapi
// 令牌失效）用户填的可用 access_token 已被丢弃，且 blockRefresh 置位后
// 所有请求报 "authentication expired and refresh is blocked"，无从回退。
func TestHandFilledAccessTokenTakesPriority(t *testing.T) {
	fake := newFake123()
	cleanup := fake.start(t)
	defer cleanup()

	const handToken = "hand-filled-access-token"
	d := newTestDriver(&native.Pan123DriverConfig{
		AccessToken:  utils.StringPtr(handToken),
		RefreshToken: utils.StringPtr("rt-should-not-be-used"),
		RootFileId:   utils.StringPtr("0"),
		ApiUrl:       utils.StringPtr(fake.baseURL + "/renewapi"),
	})

	if d.tm.accessToken != handToken {
		t.Fatalf("手填 access_token 被丢弃：got %q, want %q", d.tm.accessToken, handToken)
	}
	if d.tm.expiredAt.IsZero() {
		t.Error("手填 access_token 未设置有效期（expiredAt 为零值会导致每次请求都续期）")
	}
	if d.tm.renewed {
		t.Error("初始化阶段不应标记 renewed")
	}

	// warmup 会先取 token：手填 token 有效 → 不应打 renewapi
	_ = d.warmup() // user/info 在 fake 上可能失败，此处只关心是否发生续期

	if d.tm.renewed {
		t.Error("手填 access_token 有效时不应触发续期（renewapi 被调用）")
	}
	if d.tm.accessToken != handToken {
		t.Errorf("续期不应改变 access_token：got %q, want %q", d.tm.accessToken, handToken)
	}
}

// TestForceRefreshThrottled 断言 401 触发的强制续期有最小间隔节流。
//
// 场景：服务端持续 401（令牌被作废），requestJSON 的 3 次重试 + 分片上传
// 每片各走一遍 requestJSON，会反复以 force=true 调用 getAccessToken。
// 无节流时每次都打 renewapi → 触发官方频率限制。
func TestForceRefreshThrottled(t *testing.T) {
	renewCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renewCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "tok-new",
			"refresh_token": "rt-new",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	tm := &tokenManager{refreshToken: "rt-1"}
	creds := credentials{apiURL: srv.URL}

	// 首次 force 续期 → 真打一次
	if _, err := tm.getAccessToken(context.Background(), creds, true); err != nil {
		t.Fatalf("first force refresh failed: %v", err)
	}
	if renewCalls != 1 {
		t.Fatalf("renewCalls = %d, want 1", renewCalls)
	}

	// 紧随其后的 force 续期 → 被最小间隔节流，不应再打网络
	for i := 0; i < 3; i++ {
		if _, err := tm.getAccessToken(context.Background(), creds, true); err != nil {
			t.Fatalf("throttled force refresh #%d failed: %v", i, err)
		}
	}
	if renewCalls != 1 {
		t.Errorf("节流失效：renewCalls = %d, want 1（最小间隔内不应重复打 renewapi）", renewCalls)
	}
}

// TestRefreshFailureCooldownRecovers 断言续期失败后的阻断带冷却期，
// 冷却结束能自动恢复（历史实现是永久置位，一次瞬时失败锁死驱动）。
func TestRefreshFailureCooldownRecovers(t *testing.T) {
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"text":"temporary failure"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "tok-ok",
			"refresh_token": "rt-ok",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	tm := &tokenManager{refreshToken: "rt-1"}
	creds := credentials{apiURL: srv.URL}

	// 第一次失败 → 进入冷却
	if _, err := tm.getAccessToken(context.Background(), creds, true); err == nil {
		t.Fatal("expected refresh failure")
	}
	if !tm.blockRefresh {
		t.Fatal("失败后应置位 blockRefresh")
	}
	if tm.blockUntil.IsZero() {
		t.Fatal("失败后应设置冷却截止时间 blockUntil")
	}

	// 冷却期内：无可用 token → 报错且提示可重试时间
	if _, err := tm.getAccessToken(context.Background(), creds, true); err == nil {
		t.Error("冷却期内无 token 时应返回错误")
	}

	// 模拟冷却结束
	tm.blockUntil = time.Now().Add(-time.Second)
	fail = false
	// 冷却结束 + force 但 lastRenewAt 为零（首次续期从未成功）→ 应真正重试
	tok, err := tm.getAccessToken(context.Background(), creds, true)
	if err != nil {
		t.Fatalf("冷却结束后应能恢复续期，got: %v", err)
	}
	if tok != "tok-ok" {
		t.Errorf("恢复后 token = %q, want tok-ok", tok)
	}
	if tm.blockRefresh {
		t.Error("续期成功后应清除 blockRefresh")
	}
}

// TestMkdirRespParsesDirID 断言 mkdir 响应的字段名解析正确。
//
// 2026-10-01 事故根因：/upload/v1/file/mkdir 返回 {"data":{"dirID":65263931}}，
// 而 MkdirResp 的 tag 写成 `fileId` → Data.FileID 恒为 0 → resolve 拿到
// parent=0 继续下探 → 月目录被建到根目录下，与年目录平级
// （根目录出现 2026/10 两组平级目录、记录路径 /2026/10/2026/xxx.enc）。
func TestMkdirRespParsesDirID(t *testing.T) {
	// 服务端真实响应形状
	raw := `{"code":0,"message":"ok","data":{"dirID":65263931}}`
	var resp MkdirResp
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if resp.Data.DirID != 65263931 {
		t.Errorf("DirID = %d, want 65263931（字段名必须是 dirID，写成 fileId 会恒为 0）",
			resp.Data.DirID)
	}
}

// TestResolveFallsBackWhenMkdirReturnsZero 断言 mkdir 返回 0 时 resolve
// 回退到 findChild 重新查询，绝不把 0 当作父目录继续下探。
func TestResolveFallsBackWhenMkdirReturnsZero(t *testing.T) {
	fake := newFake123()
	cleanup := fake.start(t)
	defer cleanup()

	d := newTestDriver(&native.Pan123DriverConfig{
		RefreshToken: utils.StringPtr("rt1"),
		RootFileId:   utils.StringPtr("0"),
		ApiUrl:       utils.StringPtr(fake.baseURL + "/renewapi"),
	})
	if err := d.warmup(); err != nil {
		t.Fatalf("warmup failed: %v", err)
	}

	// 预置年目录已存在（走 findChild 命中，不受 mkdir 返回值影响）
	year := time.Now().Format("2006")
	fake.mu.Lock()
	yearID := fake.allocID()
	fake.nodes[yearID] = &fakeNode{id: yearID, name: year, parentID: 0, isDir: true}
	fake.mu.Unlock()

	b := &dirBackend{d: d}
	monthID, err := b.resolve(context.Background(), "/"+year+"/"+time.Now().Format("01"))
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if monthID == 0 {
		t.Fatal("resolve 返回 0（月目录 id 未解析到）")
	}

	// 关键断言：月目录的 parent 必须是年目录，不能是根(0)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	n, ok := fake.nodes[monthID]
	if !ok {
		t.Fatalf("月目录 id=%d 不存在于 fake 节点表", monthID)
	}
	if n.parentID != yearID {
		t.Errorf("月目录 parentID = %d, want 年目录 id=%d（parent=0 表示被建到根目录，产生平级目录）",
			n.parentID, yearID)
	}
}

// TestCompletePollsThroughVerifyingCode 断言 complete 轮询能穿过 20103
// （文件正在校验中）继续等待，而不是当致命错误终止。
func TestCompletePollsThroughVerifyingCode(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n <= 2 {
			// 前两次：校验中
			_, _ = w.Write([]byte(`{"code":20103,"message":"文件正在校验中,请间隔1秒后再试"}`))
			return
		}
		// 第三次：完成
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"completed":true,"fileID":123456}}`))
	}))
	defer srv.Close()

	oldBase := APIBase
	APIBase = srv.URL
	defer func() { APIBase = oldBase }()

	d := newTestDriver(&native.Pan123DriverConfig{})
	d.tm = &tokenManager{accessToken: "tok", expiredAt: time.Now().Add(time.Hour)}

	fid, err := d.complete(context.Background(), "preupload-1")
	if err != nil {
		t.Fatalf("complete 应在 20103 后继续轮询并最终成功，got: %v", err)
	}
	if fid != 123456 {
		t.Errorf("fileID = %d, want 123456", fid)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("轮询次数 = %d, want 3（前两次 20103 应被当作中间状态）", got)
	}
}

// TestPersistTokensWritesRotatedRefreshToken 断言续期后写回的配置包含
// renewapi 返回的**轮换后** refresh_token（123 官方语义）。
//
// 官方接口文档：「refresh_token 单次有效，刷新后返回新的 refresh_token，
// 请保存以便下一次刷新使用」。因此 persistTokens 必须回写轮换值——
// 若只回写 access_token，下一次续期仍用过期的单次令牌，服务端返回
//
//	HTTP 500 {"text":"The provided authorization grant ... is invalid, expired, revoked"}
//
// 表现为 warmup 失败 + blockRefresh 置位后所有请求报
// "123pan authentication expired and refresh is blocked"（2026-10-01 事故）。
//
// 另：access_token 也必须回写，因为「刷新后，旧 access_token 立即失效」。
func TestPersistTokensWritesRotatedRefreshToken(t *testing.T) {
	fake := newFake123()
	cleanup := fake.start(t)
	defer cleanup()

	// fake 的 renewapi 返回 refresh_token = "refresh-token-rotated"
	d := newTestDriver(&native.Pan123DriverConfig{
		RefreshToken: utils.StringPtr("rt-user-original-value"),
		RootFileId:   utils.StringPtr("0"),
		ApiUrl:       utils.StringPtr(fake.baseURL + "/renewapi"),
	})
	d.Orm = &entity.StorageProviderORM{Id: utils.Uint32Ptr(7)}
	persister := &fakePersister{}
	d.SetConfigPersister(persister)

	if err := d.warmup(); err != nil {
		t.Fatalf("warmup failed: %v", err)
	}

	if persister.n == 0 {
		t.Fatal("persistTokens 未写回配置")
	}

	var saved native.Pan123DriverConfig
	if err := json.Unmarshal([]byte(persister.last), &saved); err != nil {
		t.Fatalf("parse saved config failed: %v", err)
	}
	// 轮换值必须落库，否则下一次续期用过期的单次令牌
	if got := saved.GetRefreshToken(); got != "refresh-token-rotated" {
		t.Errorf("写回的 refresh_token = %q，应为 renewapi 返回的轮换值 %q（官方要求保存新值）",
			got, "refresh-token-rotated")
	}
	if got := saved.GetAccessToken(); got != "token-from-renew-api" {
		t.Errorf("写回的 access_token = %q，应为本次续期得到的 %q", got, "token-from-renew-api")
	}
}

// TestNextPollDelay 断言轮询间隔按线性退避递增并封顶。
func TestNextPollDelay(t *testing.T) {
	const (
		step = time.Second
		max  = 10 * time.Second
	)
	cases := []struct{ cur, want time.Duration }{
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 3 * time.Second},
		{3 * time.Second, 4 * time.Second},
		{8 * time.Second, 9 * time.Second},
		{9 * time.Second, 10 * time.Second},
		{10 * time.Second, 10 * time.Second}, // 已封顶保持
	}
	for _, c := range cases {
		if got := nextPollDelay(c.cur, step, max); got != c.want {
			t.Errorf("nextPollDelay(%v) = %v, want %v", c.cur, got, c.want)
		}
	}
}

// TestUploadDownloadDelete 全链路：开发者凭据换 token → 预热读用户信息 →
// 年/月目录分配 → 分片上传 → 合并 → 完整读取 → Range 读取 → 批量删除
func TestUploadDownloadDelete(t *testing.T) {
	fake := newFake123()
	fake.requireToken = true
	cleanup := fake.start(t)
	defer cleanup()

	d := newTestDriver(&native.Pan123DriverConfig{
		ClientId:     utils.StringPtr("test-client"),
		ClientSecret: utils.StringPtr("test-secret"),
		RootFileId:   utils.StringPtr("0"),
		UploadThread: utils.StringPtr("4"),
	})

	// 1. 初始化：换 token + 读用户信息
	if err := d.warmup(); err != nil {
		t.Fatalf("warmup failed: %v", err)
	}
	if fake.tokenCall != 1 {
		t.Errorf("access_token called %d times, want 1", fake.tokenCall)
	}
	if d.uid != 88888 {
		t.Errorf("uid = %d, want 88888", d.uid)
	}

	// 2. 源文件（长度 > 8 字节分片，保证多分片）
	content := []byte("hello-123pan-driver-integration-test-payload-0123456789")
	srcPath := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(srcPath, content, 0o600); err != nil {
		t.Fatalf("write source failed: %v", err)
	}

	// 3. 上传
	refs, err := d.Upload(context.Background(), types.UploadSource{
		TempPath: srcPath,
		FileName: "test.txt",
		FileSize: uint64(len(content)),
	}, types.UploadOptions{})
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	ref := refs[0]

	if ref.Size != uint64(len(content)) {
		t.Errorf("ref.Size = %d, want %d", ref.Size, len(content))
	}
	if !strings.HasPrefix(ref.Path, "/") {
		t.Errorf("ref.Path should keep leading slash, got %q", ref.Path)
	}
	// 物理文件名按 pfm 约定统一重命名为 原名_随机_时间戳.扩展名
	// （types.GetUniqueFileName，避免年/月共享目录内同名覆盖）
	now := time.Now()
	wantDirPrefix := fmt.Sprintf("/%04d/%02d/test_", now.Year(), int(now.Month()))
	if !strings.HasPrefix(ref.Path, wantDirPrefix) ||
		!strings.HasSuffix(ref.Path, ".txt") {
		t.Errorf("ref.Path = %q, want prefix %q", ref.Path, wantDirPrefix)
	}
	wantMD5 := md5.Sum(content)
	if ref.Hash != hex.EncodeToString(wantMD5[:]) {
		t.Errorf("ref.Hash = %q, want md5 %x", ref.Hash, wantMD5)
	}

	fileID, err := strconv.ParseInt(ref.ProviderFileID, 10, 64)
	if err != nil {
		t.Fatalf("invalid provider_file_id %q: %v", ref.ProviderFileID, err)
	}

	// 分片被完整拼接
	stored, ok := fake.fileContent(fileID)
	if !ok {
		t.Fatalf("file %d not found on fake server", fileID)
	}
	if !bytes.Equal(stored, content) {
		t.Errorf("stored content mismatch:\n got %q\nwant %q", stored, content)
	}
	wantSlices := (len(content) + 7) / 8
	if got := len(fake.slices["preupload-test-1"]); got != wantSlices {
		t.Errorf("uploaded %d slices, want %d", got, wantSlices)
	}

	// 年/月目录已创建（2026/09 两级）
	fake.mu.Lock()
	dirNames := map[string]bool{}
	for _, n := range fake.nodes {
		if n.isDir {
			dirNames[n.name] = true
		}
	}
	fake.mu.Unlock()
	year, month := time.Now().Format("2006"), time.Now().Format("01")
	if !dirNames[year] || !dirNames[month] {
		t.Errorf("year/month directories not created, got %v (want %s/%s)", dirNames, year, month)
	}

	// 4. Open 完整读取
	rc, err := d.Open(context.Background(), []types.FileRef{ref}, types.OpenOptions{})
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("read content mismatch:\n got %q\nwant %q", got, content)
	}

	// 5. OpenChunk 带偏移（Range）
	offset := int64(10)
	chunkRC, err := d.OpenChunk(context.Background(), ref, offset)
	if err != nil {
		t.Fatalf("open chunk failed: %v", err)
	}
	tail, err := io.ReadAll(chunkRC)
	chunkRC.Close()
	if err != nil {
		t.Fatalf("read chunk failed: %v", err)
	}
	if !bytes.Equal(tail, content[offset:]) {
		t.Errorf("chunk content mismatch:\n got %q\nwant %q", tail, content[offset:])
	}

	// 6. 批量删除
	results, err := d.BatchDelete(context.Background(), []types.FileRef{
		{LocationID: 7, ProviderFileID: ref.ProviderFileID, Path: ref.Path},
	})
	if err != nil {
		t.Fatalf("batch delete failed: %v", err)
	}
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("batch delete results = %+v, want 1 success", results)
	}
	if results[0].ID != 7 {
		t.Errorf("result ID = %d, want 7 (LocationID 透传)", results[0].ID)
	}
	if !fake.trashed[fileID] {
		t.Errorf("file %d not trashed", fileID)
	}
}

// TestBatchDeleteInvalidFileID fileId 非法时逐条标失败而非整批报错
func TestBatchDeleteInvalidFileID(t *testing.T) {
	fake := newFake123()
	cleanup := fake.start(t)
	defer cleanup()

	d := newTestDriver(&native.Pan123DriverConfig{
		AccessToken: utils.StringPtr("manual-token"),
	})

	results, err := d.BatchDelete(context.Background(), []types.FileRef{
		{LocationID: 1, ProviderFileID: "not-a-number"},
		{LocationID: 2, ProviderFileID: "42"},
	})
	if err != nil {
		t.Fatalf("batch delete should not return error, got %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Success || results[0].Error == nil {
		t.Errorf("result[0] should fail with error, got %+v", results[0])
	}
	if !results[1].Success {
		t.Errorf("result[1] should succeed, got %+v", results[1])
	}
}

// TestRenewAPIRefreshToken oplist renewapi：首次用配置里的 refresh_token，
// 之后改用接口返回的新值
func TestRenewAPIRefreshToken(t *testing.T) {
	var seenRefreshUI []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenRefreshUI = append(seenRefreshUI, r.URL.Query().Get("refresh_ui"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "rotated-access-token",
			"refresh_token": "rotated-refresh-token",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	tm := initTokenManager(&native.Pan123DriverConfig{
		RefreshToken: utils.StringPtr("user-provided-refresh-token"),
		ApiUrl:       utils.StringPtr(srv.URL + "/renewapi"),
	})
	// 无手填 access_token 时走续期路径；手填 token 优先的新语义下
	// accessToken 为空（未配置），warmup/first call 会真正续期
	if tm.accessToken != "" {
		t.Fatalf("未配置 access_token 时应为空，got %q", tm.accessToken)
	}
	// credentials 必须带 apiURL，否则会回落到真实的 RenewAPIDefault
	creds := credentials{apiURL: srv.URL + "/renewapi"}

	tok, err := tm.getAccessToken(context.Background(), creds, false)
	if err != nil {
		t.Fatalf("first refresh failed: %v", err)
	}
	if tok != "rotated-access-token" {
		t.Errorf("token = %q, want rotated-access-token", tok)
	}
	// 内存中的 refresh_token 已被接口返回值覆盖
	if tm.refreshToken != "rotated-refresh-token" {
		t.Errorf("tm.refreshToken = %q, want rotated-refresh-token", tm.refreshToken)
	}

	// 强制刷新受最小间隔节流：紧随第一次成功续期之后不会重复打 renewapi
	if _, err := tm.getAccessToken(context.Background(), creds, true); err != nil {
		t.Fatalf("second refresh failed: %v", err)
	}

	mu.Lock()
	if len(seenRefreshUI) != 1 {
		mu.Unlock()
		t.Fatalf("renew api called %d times, want 1（最小间隔内应被节流）", len(seenRefreshUI))
	}
	if seenRefreshUI[0] != "user-provided-refresh-token" {
		t.Errorf("first refresh_ui = %q, want user-provided-refresh-token", seenRefreshUI[0])
	}
	mu.Unlock()

	// 把上次续期时间回拨，越过最小间隔后再 force → 应真正续期并使用轮换后的 refresh_token
	tm.mu.Lock()
	tm.lastRenewAt = time.Now().Add(-forceRefreshMinInterval - time.Second)
	tm.mu.Unlock()

	if _, err := tm.getAccessToken(context.Background(), creds, true); err != nil {
		t.Fatalf("third refresh failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenRefreshUI) != 2 {
		t.Fatalf("renew api called %d times, want 2（越过间隔后应再续期）", len(seenRefreshUI))
	}
	if seenRefreshUI[1] != "rotated-refresh-token" {
		t.Errorf("second refresh_ui = %q, want rotated-refresh-token", seenRefreshUI[1])
	}
}

// TestManualAccessTokenFallback 手填 access_token 时不主动刷新，按 90 天过期处理
func TestManualAccessTokenFallback(t *testing.T) {
	tm := initTokenManager(&native.Pan123DriverConfig{
		AccessToken: utils.StringPtr("manual-token"),
	})
	if tm.accessToken != "manual-token" {
		t.Errorf("tm.accessToken = %q, want manual-token", tm.accessToken)
	}
	if tm.refreshToken != "" {
		t.Errorf("tm.refreshToken should be empty, got %q", tm.refreshToken)
	}

	// 90 天内直接复用，不触发任何刷新
	tok, err := tm.getAccessToken(context.Background(), credentials{}, false)
	if err != nil {
		t.Fatalf("should reuse manual token, got error: %v", err)
	}
	if tok != "manual-token" {
		t.Errorf("token = %q, want manual-token", tok)
	}
}

// TestTokenBlockedAfterFailure 刷新失败后置位阻断，避免每次请求都打续期接口
func TestTokenBlockedAfterFailure(t *testing.T) {
	var calls int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"error_description": "invalid refresh token"})
	}))
	defer srv.Close()

	tm := initTokenManager(&native.Pan123DriverConfig{
		RefreshToken: utils.StringPtr("bad-token"),
		ApiUrl:       utils.StringPtr(srv.URL + "/renewapi"),
	})
	creds := credentials{apiURL: srv.URL + "/renewapi"}

	if _, err := tm.getAccessToken(context.Background(), creds, false); err == nil {
		t.Fatal("expected refresh error on first call")
	}
	// 第二次应被阻断，不再打接口
	if _, err := tm.getAccessToken(context.Background(), creds, false); err == nil {
		t.Fatal("expected blocked error on second call")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("renew api called %d times, want 1 (refresh blocked after failure)", calls)
	}
}

// TestRequest401TriggersRefresh 请求返回 401 时应强制刷新并重试
func TestRequest401TriggersRefresh(t *testing.T) {
	var accessCalls, apiCalls int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/renewapi":
			mu.Lock()
			accessCalls++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "fresh-token",
				"refresh_token": "rotated",
				"expires_in":    3600,
			})
		case "/open-api" + endpointUserInfo:
			mu.Lock()
			apiCalls++
			current := apiCalls
			mu.Unlock()
			// 第一次用过期 token → 401，刷新后第二次成功
			if current == 1 {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "token invalid"})
				return
			}
			if r.Header.Get("Authorization") != "Bearer fresh-token" {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "wrong token"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "message": "ok",
				"data": map[string]any{"uid": 1, "spacePermanent": 10, "spaceUsed": 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldBase, oldRenew := APIBase, RenewAPIDefault
	APIBase = srv.URL + "/open-api"
	RenewAPIDefault = srv.URL + "/renewapi"
	defer func() { APIBase, RenewAPIDefault = oldBase, oldRenew }()

	// 预置一个"已过期"的 token，让首次请求走 401 分支
	tm := initTokenManager(&native.Pan123DriverConfig{
		RefreshToken: utils.StringPtr("initial-refresh"),
		ApiUrl:       utils.StringPtr(srv.URL + "/renewapi"),
	})
	tm.accessToken = "stale-token"
	tm.expiredAt = time.Now().Add(time.Hour)

	d := newTestDriver(&native.Pan123DriverConfig{
		RefreshToken: utils.StringPtr("initial-refresh"),
		ApiUrl:       utils.StringPtr(srv.URL + "/renewapi"),
	})
	d.tm = tm

	if _, err := d.getUserInfo(context.Background()); err != nil {
		t.Fatalf("getUserInfo should succeed after refresh, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if apiCalls != 2 {
		t.Errorf("user info called %d times, want 2 (401 then retry)", apiCalls)
	}
	if accessCalls != 1 {
		t.Errorf("renew api called %d times, want 1", accessCalls)
	}
	if tm.accessToken != "fresh-token" {
		t.Errorf("token after refresh = %q, want fresh-token", tm.accessToken)
	}
}

// TestPathHelpers pathJoin / splitPath 的路径语义
func TestPathHelpers(t *testing.T) {
	joinCases := []struct {
		base, name, want string
	}{
		{"/", "a.txt", "/a.txt"},
		{"/2026/09", "a.txt", "/2026/09/a.txt"},
		{"/2026/09/", "a.txt", "/2026/09/a.txt"},
	}
	for _, c := range joinCases {
		if got := pathJoin(c.base, c.name); got != c.want {
			t.Errorf("pathJoin(%q, %q) = %q, want %q", c.base, c.name, got, c.want)
		}
	}

	segCases := []struct {
		in   string
		want []string
	}{
		{"/2026/09", []string{"2026", "09"}},
		{"2026/09", []string{"2026", "09"}},
		{"/", nil},
		{"/./2026", []string{"2026"}},
	}
	for _, c := range segCases {
		got := splitPath(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitPath(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitPath(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

// TestUploadThreads upload_thread 解析边界
func TestUploadThreads(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"1", 1},
		{"32", 32},
		{"0", 3},   // 越界回落默认
		{"33", 3},  // 越界回落默认
		{"abc", 3}, // 非法回落默认
	}
	for _, c := range cases {
		d := &Pan123Driver{
			Config: &native.Pan123DriverConfig{UploadThread: utils.StringPtr(c.in)},
		}
		if got := d.uploadThreads(); got != c.want {
			t.Errorf("uploadThreads(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	// nil config 不应 panic
	d := &Pan123Driver{}
	if got := d.uploadThreads(); got != 3 {
		t.Errorf("uploadThreads(nil config) = %d, want 3", got)
	}
}

// TestComputeFileHashes MD5/SHA1 一次读取同时产出，且游标复位
func TestComputeFileHashes(t *testing.T) {
	content := []byte("123pan hash test payload")
	path := filepath.Join(t.TempDir(), "hash.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer f.Close()

	md5Hex, sha1Hex, err := computeFileHashes(f)
	if err != nil {
		t.Fatalf("computeFileHashes failed: %v", err)
	}

	wantMD5 := md5.Sum(content)
	wantSHA1 := sha1.Sum(content)
	if md5Hex != hex.EncodeToString(wantMD5[:]) {
		t.Errorf("md5 = %q, want %x", md5Hex, wantMD5)
	}
	if sha1Hex != hex.EncodeToString(wantSHA1[:]) {
		t.Errorf("sha1 = %q, want %x", sha1Hex, wantSHA1)
	}

	// 游标必须复位到开头
	buf := make([]byte, 4)
	if _, err := io.ReadFull(f, buf); err != nil {
		t.Fatalf("read after hash failed: %v", err)
	}
	if !bytes.Equal(buf, content[:4]) {
		t.Errorf("file cursor not reset: got %q, want %q", buf, content[:4])
	}
}

// TestCheckCredentials 三种认证方式的预检
func TestCheckCredentials(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *native.Pan123DriverConfig
		wantErr bool
	}{
		{"client credentials", &native.Pan123DriverConfig{
			ClientId:     utils.StringPtr("id"),
			ClientSecret: utils.StringPtr("secret"),
		}, false},
		{"refresh token", &native.Pan123DriverConfig{
			RefreshToken: utils.StringPtr("rt"),
		}, false},
		{"access token", &native.Pan123DriverConfig{
			AccessToken: utils.StringPtr("at"),
		}, false},
		{"client id without secret", &native.Pan123DriverConfig{
			ClientId: utils.StringPtr("id"),
		}, true},
		{"empty", &native.Pan123DriverConfig{}, true},
	}
	for _, c := range cases {
		err := checkCredentials(c.cfg)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", c.name, err, c.wantErr)
		}
	}
}

// TestRootFileID root_file_id 解析与非法值回落
func TestRootFileID(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"12345", 12345},
		{"abc", 0}, // 非法回落根目录
		{"-1", 0},  // 负数回落根目录
		{"", 0},    // 空回落根目录
	}
	for _, c := range cases {
		d := &Pan123Driver{
			Config: &native.Pan123DriverConfig{RootFileId: utils.StringPtr(c.in)},
		}
		if got := d.rootFileID(); got != c.want {
			t.Errorf("rootFileID(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	// nil config 不应 panic
	d := &Pan123Driver{}
	if got := d.rootFileID(); got != 0 {
		t.Errorf("rootFileID(nil config) = %d, want 0", got)
	}
}
