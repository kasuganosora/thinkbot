package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// 分片上传把一个大文件拆成多段小请求，绕开边缘代理的单请求体上限。
//
//	POST   /files/uploads                         建会话
//	PUT    /files/uploads/:uploadId/chunks/:index 写入一段（可重试、可乱序）
//	POST   /files/uploads/:uploadId/complete      校验后合并，再放入工作区
//	DELETE /files/uploads/:uploadId               放弃并删掉暂存
//
// 会话与 Bot 各有一套相同的路径，最终都进同一个 bot 工作区。
// 旧的单次 POST /files/upload 保持不变，小文件和旧客户端继续用它。

const (
	chunkUploadDirName        = ".chunk-uploads"
	chunkUploadOpen           = "open"
	chunkUploadCommitting     = "committing"
	chunkUploadAborted        = "aborted"
	chunkUploadAbandonedAfter = 6 * time.Hour
	chunkUploadSweepEvery     = 15 * time.Minute
	maxActiveChunkUploads     = 8
	maxChunkUploadParts       = 4096
	chunkUploadMetaLimit      = 1 << 20
)

// chunkUpload 是一次尚未合并的分片上传。
// 目录布局：{workspace}/.chunk-uploads/{id}/meta.json 与 chunks/000000…
// 落盘只用 chunkUploadMeta，避免把锁交给 encoding/json。
type chunkUpload struct {
	mu          sync.Mutex
	ID          string
	OwnerID     uint
	BotID       string
	Path        string
	Name        string
	Size        int64
	ChunkSize   int64
	TotalChunks int
	State       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	dir         string
}

// chunkUploadMeta 是落在 meta.json 里的字段。不含锁，也不记录宿主目录。
type chunkUploadMeta struct {
	ID          string    `json:"id"`
	OwnerID     uint      `json:"ownerId"`
	BotID       string    `json:"botId"`
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ChunkSize   int64     `json:"chunkSize"`
	TotalChunks int       `json:"totalChunks"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// chunkUploadStore 只缓存本进程见过的上传。锁顺序是先 store.mu 再 upload.mu。
type chunkUploadStore struct {
	mu   sync.Mutex
	byID map[string]*chunkUpload
}

var chunkTempSeq atomic.Uint64

func (s *Server) uploads() *chunkUploadStore {
	s.chunkUploadOnce.Do(func() {
		s.chunkUploads = &chunkUploadStore{byID: map[string]*chunkUpload{}}
	})
	return s.chunkUploads
}

func (s *Server) chunkUploadChunkSize() int64 {
	n := DefaultChunkUploadBytes
	if s != nil && s.chunkUploadBytes > 0 {
		n = s.chunkUploadBytes
	}
	if n > MaxChunkUploadBytes {
		return MaxChunkUploadBytes
	}
	if n <= 0 {
		return DefaultChunkUploadBytes
	}
	return n
}

func (s *Server) chunkUploadActiveLimit() int {
	if s != nil && s.chunkUploadMaxActive > 0 {
		return s.chunkUploadMaxActive
	}
	return maxActiveChunkUploads
}

func (s *Server) chunkUploadRoot() (string, error) {
	base := "data/workspaces"
	if s != nil && s.store != nil {
		logger := s.logger
		base = config.NewBuilder(s.store, logger).GetWorkspaceDir()
	}
	if strings.TrimSpace(base) == "" {
		base = "data/workspaces"
	}
	return filepath.Abs(filepath.Join(base, chunkUploadDirName))
}

func (s *Server) runChunkUploadSweeper(ctx context.Context) {
	if n, err := s.sweepAbandonedChunkUploads(time.Now()); err != nil {
		if s.logger != nil {
			s.logger.Warnw("chunk upload sweep failed", "err", err)
		}
	} else if n > 0 && s.logger != nil {
		s.logger.Infow("swept abandoned chunk uploads", "removed", n)
	}
	ticker := time.NewTicker(chunkUploadSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			n, err := s.sweepAbandonedChunkUploads(now)
			if err != nil && s.logger != nil {
				s.logger.Warnw("chunk upload sweep failed", "err", err)
				continue
			}
			if n > 0 && s.logger != nil {
				s.logger.Infow("swept abandoned chunk uploads", "removed", n)
			}
		}
	}
}

func chunkUploadOwner(c *gin.Context) uint {
	if u := currentUser(c); u != nil {
		return u.ID
	}
	return 0
}

func (s *Server) handleSessionChunkUploadInit(c *gin.Context) {
	s.handleChunkUploadInit(c, c.Param("sid"))
}

func (s *Server) handleBotChunkUploadInit(c *gin.Context) {
	s.handleChunkUploadInit(c, c.Param("id"))
}

func (s *Server) handleSessionChunkUploadPut(c *gin.Context) {
	s.handleChunkUploadPut(c, c.Param("sid"))
}

func (s *Server) handleBotChunkUploadPut(c *gin.Context) {
	s.handleChunkUploadPut(c, c.Param("id"))
}

func (s *Server) handleSessionChunkUploadComplete(c *gin.Context) {
	s.handleChunkUploadComplete(c, c.Param("sid"))
}

func (s *Server) handleBotChunkUploadComplete(c *gin.Context) {
	s.handleChunkUploadComplete(c, c.Param("id"))
}

func (s *Server) handleSessionChunkUploadAbort(c *gin.Context) {
	s.handleChunkUploadAbort(c, c.Param("sid"))
}

func (s *Server) handleBotChunkUploadAbort(c *gin.Context) {
	s.handleChunkUploadAbort(c, c.Param("id"))
}

type chunkUploadInitRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func (s *Server) handleChunkUploadInit(c *gin.Context, botID string) {
	up, err := s.createChunkUpload(c, botID)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, gin.H{
		"uploadId":    up.ID,
		"chunkSize":   up.ChunkSize,
		"totalChunks": up.TotalChunks,
		"size":        up.Size,
	})
}

func (s *Server) createChunkUpload(c *gin.Context, botID string) (*chunkUpload, error) {
	if strings.TrimSpace(botID) == "" {
		return nil, errs.BadRequest("invalid bot id")
	}
	var req chunkUploadInitRequest
	if err := decodeJSONBody(c, &req, chunkUploadMetaLimit); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Path) == "" {
		req.Path = "/"
	}
	if strings.ContainsRune(req.Path, 0) {
		return nil, errs.BadRequest("invalid path")
	}
	name := req.Name
	if err := validateUploadFileName(name); err != nil {
		return nil, err
	}
	if req.Size <= 0 {
		return nil, errs.BadRequest("size is required")
	}
	if req.Size > MaxUserUploadBytes {
		return nil, uploadTooLargeError()
	}
	// 路径在真正落盘时还会再走 safeJoin。这里用一个不存在的根先拦住明显的坏名字。
	if _, err := safeJoin(filepath.Join(string(filepath.Separator), "upload-check"), req.Path, name); err != nil {
		return nil, err
	}

	chunkSize := s.chunkUploadChunkSize()
	parts := chunkCount(req.Size, chunkSize)
	if parts <= 0 || parts > maxChunkUploadParts {
		return nil, errs.BadRequest("file requires too many chunks")
	}

	root, err := s.chunkUploadRoot()
	if err != nil {
		return nil, errs.Internal("failed to prepare upload directory")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, errs.Internal("failed to prepare upload directory")
	}

	id, err := newChunkUploadID()
	if err != nil {
		return nil, errs.Internal("failed to allocate upload id")
	}
	now := time.Now().UTC()
	up := &chunkUpload{
		ID:          id,
		OwnerID:     chunkUploadOwner(c),
		BotID:       botID,
		Path:        req.Path,
		Name:        name,
		Size:        req.Size,
		ChunkSize:   chunkSize,
		TotalChunks: parts,
		State:       chunkUploadOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
		dir:         filepath.Join(root, id),
	}

	store := s.uploads()
	store.mu.Lock()
	if store.countOwner(root, up.OwnerID) >= s.chunkUploadActiveLimit() {
		store.mu.Unlock()
		return nil, errs.HTTPError(http.StatusTooManyRequests, "too many unfinished uploads")
	}
	store.byID[id] = up
	store.mu.Unlock()

	// 占位之后立刻持有上传锁再写 meta，避免清扫协程并发读到半初始化的字段。
	up.mu.Lock()
	mkErr := os.MkdirAll(filepath.Join(up.dir, "chunks"), 0o700)
	var metaErr error
	if mkErr == nil {
		metaErr = saveChunkMeta(up)
	}
	up.mu.Unlock()
	if mkErr != nil || metaErr != nil {
		store.forget(id)
		_ = os.RemoveAll(up.dir)
		return nil, errs.Internal("failed to prepare upload directory")
	}
	return up, nil
}

func (s *Server) handleChunkUploadPut(c *gin.Context, botID string) {
	up, err := s.lookupChunkUpload(botID, chunkUploadOwner(c), c.Param("uploadId"))
	if err != nil {
		Fail(c, err)
		return
	}
	index, err := parseChunkIndex(c.Param("index"))
	if err != nil {
		Fail(c, err)
		return
	}
	if err := s.writeChunk(c, up, index); err != nil {
		Fail(c, err)
		return
	}
	OK(c, gin.H{"ok": true, "index": index})
}

func (s *Server) writeChunk(c *gin.Context, up *chunkUpload, index int) error {
	up.mu.Lock()
	state := up.State
	total := up.TotalChunks
	chunkSize := up.ChunkSize
	size := up.Size
	dir := up.dir
	up.mu.Unlock()
	if state == chunkUploadAborted {
		return errs.NotFound("upload not found")
	}
	if state != chunkUploadOpen {
		return errs.Conflict("upload is already being finalized")
	}
	if index < 0 || index >= total {
		return errs.BadRequest("invalid chunk index")
	}
	expected := expectedChunkLength(size, chunkSize, index, total)
	if expected <= 0 || expected > MaxChunkUploadBytes {
		return errs.BadRequest("invalid chunk index")
	}
	if c.Request == nil || c.Request.Body == nil {
		return errs.BadRequest(fmt.Sprintf("chunk %d must be %d bytes", index, expected))
	}
	if c.Request.ContentLength > MaxChunkUploadBytes || (c.Request.ContentLength >= 0 && c.Request.ContentLength != expected) {
		return errs.BadRequest(fmt.Sprintf("chunk %d must be %d bytes", index, expected))
	}
	body := http.MaxBytesReader(c.Writer, c.Request.Body, expected)

	tmp, n, err := writeChunkTemp(filepath.Join(dir, "chunks"), index, body, expected)
	if tmp != "" {
		defer func() { _ = os.Remove(tmp) }()
	}
	if err != nil {
		if isRequestBodyTooLarge(err) || errors.Is(err, errChunkSize) {
			return errs.BadRequest(fmt.Sprintf("chunk %d must be %d bytes", index, expected))
		}
		return errs.Internal("failed to store chunk")
	}
	if n != expected {
		return errs.BadRequest(fmt.Sprintf("chunk %d must be %d bytes", index, expected))
	}

	up.mu.Lock()
	defer up.mu.Unlock()
	if up.State != chunkUploadOpen {
		return errs.Conflict("upload is already being finalized")
	}
	final := chunkFilePath(dir, index)
	if err := os.Rename(tmp, final); err != nil {
		return errs.Internal("failed to store chunk")
	}
	tmp = "" // rename 已取走，defer 不能再删最终文件
	up.UpdatedAt = time.Now().UTC()
	if err := saveChunkMeta(up); err != nil {
		return errs.Internal("failed to store chunk")
	}
	return nil
}

func (s *Server) handleChunkUploadComplete(c *gin.Context, botID string) {
	up, err := s.lookupChunkUpload(botID, chunkUploadOwner(c), c.Param("uploadId"))
	if err != nil {
		Fail(c, err)
		return
	}
	if err := s.completeChunkUpload(c, up); err != nil {
		Fail(c, err)
		return
	}
	if s.logger != nil {
		auditLog(c, s.logger, "workspace_file_upload", "bot", botID, "path", up.Path, "name", up.Name, "size", up.Size, "uploadId", up.ID, "chunked", true)
	}
	OK(c, gin.H{"ok": true})
}

func (s *Server) completeChunkUpload(c *gin.Context, up *chunkUpload) error {
	up.mu.Lock()
	if up.State == chunkUploadAborted {
		up.mu.Unlock()
		return errs.NotFound("upload not found")
	}
	if up.State != chunkUploadOpen {
		up.mu.Unlock()
		return errs.Conflict("upload is already being finalized")
	}
	if err := verifyChunkFiles(up); err != nil {
		up.mu.Unlock()
		if errors.Is(err, errChunkMissing) {
			return errs.BadRequest(err.Error())
		}
		return errs.Internal("failed to assemble upload")
	}
	up.State = chunkUploadCommitting
	up.UpdatedAt = time.Now().UTC()
	metaErr := saveChunkMeta(up)
	up.mu.Unlock()
	if metaErr != nil {
		s.reopenChunkUpload(up)
		return errs.Internal("failed to finalize upload")
	}

	assembled, err := assembleChunkUpload(up)
	if err != nil {
		s.reopenChunkUpload(up)
		if errors.Is(err, errChunkMissing) {
			return errs.BadRequest(err.Error())
		}
		return errs.Internal("failed to assemble upload")
	}
	if err := s.commitChunkedFile(c, up, assembled); err != nil {
		_ = os.Remove(assembled)
		s.reopenChunkUpload(up)
		return err
	}
	// 先删暂存再移出内存索引，避免完成窗口里被 getOrLoad 从磁盘复活。
	_ = os.RemoveAll(up.dir)
	s.uploads().forget(up.ID)
	return nil
}

func (s *Server) reopenChunkUpload(up *chunkUpload) {
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.State != chunkUploadCommitting {
		return
	}
	up.State = chunkUploadOpen
	up.UpdatedAt = time.Now().UTC()
	_ = saveChunkMeta(up)
}

func (s *Server) commitChunkedFile(c *gin.Context, up *chunkUpload, assembled string) error {
	// 容器卷在主进程里不可见，只能把已经拼好的文件再流进去。
	// 宿主工作区则把暂存文件 rename 到目标目录，避免先截断最终路径。
	if ws, ok := s.workspaceForUpload(up.BotID); ok {
		f, err := os.Open(assembled)
		if err != nil {
			return errs.Internal("failed to read assembled upload")
		}
		defer f.Close()
		target := filepath.ToSlash(filepath.Join(up.Path, up.Name))
		n, err := writeUploadedWorkspaceStream(requestContext(c), ws, target, f, up.Size)
		if err != nil {
			return mapUploadWriteError(err)
		}
		if n != up.Size {
			return errs.Internal("assembled upload size mismatch")
		}
		return nil
	}

	root, err := s.botWorkspaceRoot(up.BotID)
	if err != nil {
		return errs.Internal("failed to resolve workspace root: " + err.Error())
	}
	target, err := safeJoin(root, up.Path, up.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return errs.Internal("failed to prepare directory: " + err.Error())
	}
	if err := moveFileAtomic(assembled, target); err != nil {
		return errs.Internal("failed to write file: " + err.Error())
	}
	return nil
}

func (s *Server) handleChunkUploadAbort(c *gin.Context, botID string) {
	up, err := s.lookupChunkUpload(botID, chunkUploadOwner(c), c.Param("uploadId"))
	if err != nil {
		Fail(c, err)
		return
	}
	if err := s.abortChunkUpload(up); err != nil {
		Fail(c, err)
		return
	}
	OK(c, gin.H{"ok": true})
}

func (s *Server) abortChunkUpload(up *chunkUpload) error {
	up.mu.Lock()
	if up.State == chunkUploadCommitting {
		up.mu.Unlock()
		return errs.Conflict("upload is already being finalized")
	}
	if up.State == chunkUploadAborted {
		up.mu.Unlock()
		return errs.NotFound("upload not found")
	}
	up.State = chunkUploadAborted
	dir := up.dir
	up.mu.Unlock()
	_ = os.RemoveAll(dir)
	s.uploads().forget(up.ID)
	return nil
}

func (s *Server) lookupChunkUpload(botID string, owner uint, id string) (*chunkUpload, error) {
	if !validChunkUploadID(id) {
		return nil, errs.BadRequest("invalid upload id")
	}
	root, err := s.chunkUploadRoot()
	if err != nil {
		return nil, errs.NotFound("upload not found")
	}
	up, err := s.uploads().getOrLoad(root, id)
	if err != nil {
		return nil, errs.NotFound("upload not found")
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.BotID != botID || up.OwnerID != owner || up.State == chunkUploadAborted {
		return nil, errs.NotFound("upload not found")
	}
	return up, nil
}

func (st *chunkUploadStore) getOrLoad(root, id string) (*chunkUpload, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if up, ok := st.byID[id]; ok {
		return up, nil
	}
	up, err := loadChunkUpload(root, id)
	if err != nil {
		return nil, err
	}
	if up.State == chunkUploadCommitting {
		// 进程已经不在了，允许客户端重新合并。
		up.State = chunkUploadOpen
		_ = saveChunkMeta(up)
	}
	st.byID[id] = up
	return up, nil
}

func (st *chunkUploadStore) forget(id string) {
	st.mu.Lock()
	delete(st.byID, id)
	st.mu.Unlock()
}

func (st *chunkUploadStore) countOwner(root string, owner uint) int {
	seen := make(map[string]struct{})
	n := 0
	for id, up := range st.byID {
		up.mu.Lock()
		own, state := up.OwnerID, up.State
		up.mu.Unlock()
		if own == owner && state == chunkUploadOpen {
			seen[id] = struct{}{}
			n++
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return n
	}
	for _, e := range entries {
		id := e.Name()
		if _, ok := seen[id]; ok || !e.IsDir() || !validChunkUploadID(id) {
			continue
		}
		up, err := loadChunkUpload(root, id)
		if err != nil || up.OwnerID != owner || up.State != chunkUploadOpen {
			continue
		}
		n++
	}
	return n
}

func (s *Server) sweepAbandonedChunkUploads(now time.Time) (int, error) {
	root, err := s.chunkUploadRoot()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	store := s.uploads()
	removed := 0
	ttl := chunkUploadAbandonedAfter
	for _, e := range entries {
		id := e.Name()
		dir := filepath.Join(root, id)
		if !e.IsDir() || !validChunkUploadID(id) {
			info, statErr := e.Info()
			if statErr == nil && now.Sub(info.ModTime()) > ttl {
				_ = os.RemoveAll(dir)
				removed++
			}
			continue
		}
		store.mu.Lock()
		up := store.byID[id]
		store.mu.Unlock()
		if up != nil {
			up.mu.Lock()
			busy := up.State == chunkUploadCommitting
			updated := up.UpdatedAt
			up.mu.Unlock()
			if busy || now.Sub(updated) <= ttl {
				continue
			}
			up.mu.Lock()
			if up.State == chunkUploadCommitting || now.Sub(up.UpdatedAt) <= ttl {
				up.mu.Unlock()
				continue
			}
			up.State = chunkUploadAborted
			up.mu.Unlock()
			_ = os.RemoveAll(dir)
			store.forget(id)
			removed++
			continue
		}
		loaded, loadErr := loadChunkUpload(root, id)
		stale := loadErr != nil
		if loadErr == nil {
			stale = now.Sub(loaded.UpdatedAt) > ttl
		} else if info, statErr := os.Stat(dir); statErr == nil {
			stale = now.Sub(info.ModTime()) > ttl
		}
		if !stale {
			continue
		}
		_ = os.RemoveAll(dir)
		removed++
	}
	return removed, nil
}

func newChunkUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validChunkUploadID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func parseChunkIndex(raw string) (int, error) {
	if raw == "" || len(raw) > 6 {
		return 0, errs.BadRequest("invalid chunk index")
	}
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, errs.BadRequest("invalid chunk index")
		}
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errs.BadRequest("invalid chunk index")
	}
	return n, nil
}

func chunkCount(size, chunkSize int64) int {
	if size <= 0 || chunkSize <= 0 {
		return 0
	}
	n := size / chunkSize
	if size%chunkSize != 0 {
		n++
	}
	if n > int64(maxChunkUploadParts) {
		return maxChunkUploadParts + 1
	}
	return int(n)
}

func expectedChunkLength(size, chunkSize int64, index, total int) int64 {
	if index < 0 || index >= total || chunkSize <= 0 || size <= 0 {
		return 0
	}
	if index < total-1 {
		return chunkSize
	}
	rem := size % chunkSize
	if rem == 0 {
		return chunkSize
	}
	return rem
}

func chunkFilePath(dir string, index int) string {
	return filepath.Join(dir, "chunks", fmt.Sprintf("%06d", index))
}

var errChunkSize = errors.New("chunk size mismatch")
var errChunkMissing = errors.New("missing chunk")

func writeChunkTemp(dir string, index int, r io.Reader, expected int64) (string, int64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	tmp := filepath.Join(dir, fmt.Sprintf("%06d.%d.part", index, chunkTempSeq.Add(1)))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	n, copyErr := io.Copy(f, r)
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || n != expected {
		_ = os.Remove(tmp)
		if copyErr != nil {
			return "", n, copyErr
		}
		if n != expected {
			return "", n, errChunkSize
		}
		if syncErr != nil {
			return "", n, syncErr
		}
		return "", n, closeErr
	}
	return tmp, n, nil
}

func verifyChunkFiles(up *chunkUpload) error {
	var total int64
	for i := 0; i < up.TotalChunks; i++ {
		want := expectedChunkLength(up.Size, up.ChunkSize, i, up.TotalChunks)
		fi, err := os.Stat(chunkFilePath(up.dir, i))
		if err != nil || fi.Size() != want {
			return fmt.Errorf("%w %d", errChunkMissing, i)
		}
		total += fi.Size()
	}
	if total != up.Size {
		return fmt.Errorf("%w size", errChunkMissing)
	}
	return nil
}

func assembleChunkUpload(up *chunkUpload) (string, error) {
	final := filepath.Join(up.dir, "assembled")
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmp)
		}
	}()
	var total int64
	for i := 0; i < up.TotalChunks; i++ {
		want := expectedChunkLength(up.Size, up.ChunkSize, i, up.TotalChunks)
		cf, err := os.Open(chunkFilePath(up.dir, i))
		if err != nil {
			_ = f.Close()
			return "", fmt.Errorf("%w %d", errChunkMissing, i)
		}
		n, copyErr := io.Copy(f, cf)
		_ = cf.Close()
		if copyErr != nil {
			_ = f.Close()
			return "", copyErr
		}
		if n != want {
			_ = f.Close()
			return "", fmt.Errorf("%w %d", errChunkMissing, i)
		}
		total += n
	}
	if total != up.Size {
		_ = f.Close()
		return "", fmt.Errorf("%w size", errChunkMissing)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	success = true
	return final, nil
}

func moveFileAtomic(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return err
	}
	partial := dst + ".partial"
	if err := copyFileSync(src, partial); err != nil {
		_ = os.Remove(partial)
		return err
	}
	if err := os.Rename(partial, dst); err != nil {
		_ = os.Remove(partial)
		return err
	}
	_ = os.Remove(src)
	return nil
}

func copyFileSync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}

func saveChunkMeta(up *chunkUpload) error {
	if up == nil || !validChunkUploadID(up.ID) {
		return errors.New("invalid upload id")
	}
	// 单独拷贝可导出字段，避免 json.Marshal 复制 sync.Mutex。
	payload, err := json.Marshal(up.meta())
	if err != nil {
		return err
	}
	tmp := filepath.Join(up.dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(up.dir, "meta.json"))
}

func loadChunkUpload(root, id string) (*chunkUpload, error) {
	if !validChunkUploadID(id) {
		return nil, os.ErrNotExist
	}
	dir := filepath.Join(root, id)
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta chunkUploadMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	up := &chunkUpload{
		ID:          meta.ID,
		OwnerID:     meta.OwnerID,
		BotID:       meta.BotID,
		Path:        meta.Path,
		Name:        meta.Name,
		Size:        meta.Size,
		ChunkSize:   meta.ChunkSize,
		TotalChunks: meta.TotalChunks,
		State:       meta.State,
		CreatedAt:   meta.CreatedAt,
		UpdatedAt:   meta.UpdatedAt,
		dir:         dir,
	}
	if up.ID != id || !validChunkUploadMeta(up) {
		return nil, errors.New("invalid chunk upload metadata")
	}
	return up, nil
}

func (up *chunkUpload) meta() chunkUploadMeta {
	return chunkUploadMeta{
		ID:          up.ID,
		OwnerID:     up.OwnerID,
		BotID:       up.BotID,
		Path:        up.Path,
		Name:        up.Name,
		Size:        up.Size,
		ChunkSize:   up.ChunkSize,
		TotalChunks: up.TotalChunks,
		State:       up.State,
		CreatedAt:   up.CreatedAt,
		UpdatedAt:   up.UpdatedAt,
	}
}

func validChunkUploadMeta(up *chunkUpload) bool {
	if up.ChunkSize <= 0 || up.ChunkSize > MaxChunkUploadBytes {
		return false
	}
	if up.Size <= 0 || up.Size > MaxUserUploadBytes {
		return false
	}
	if up.TotalChunks != chunkCount(up.Size, up.ChunkSize) || up.TotalChunks <= 0 || up.TotalChunks > maxChunkUploadParts {
		return false
	}
	if validateUploadFileName(up.Name) != nil || strings.TrimSpace(up.BotID) == "" || strings.ContainsRune(up.Path, 0) {
		return false
	}
	return true
}

func decodeJSONBody(c *gin.Context, dst any, limit int64) error {
	if c.Request == nil || c.Request.Body == nil {
		return errs.BadRequest("invalid request body")
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		if isRequestBodyTooLarge(err) {
			return errs.BadRequest("invalid request body")
		}
		return errs.BadRequest("invalid request body")
	}
	return nil
}
