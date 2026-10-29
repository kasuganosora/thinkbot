package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/sandbox"
)

func TestIsWorkspaceFileUploadSkipsChunkBodiesOnly(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/api/bots/b/files/upload", true},
		{http.MethodPost, "/api/sessions/s/files/upload", true},
		{http.MethodPut, "/api/bots/b/files/uploads/abc/chunks/0", true},
		{http.MethodPut, "/api/sessions/s/files/uploads/abc/chunks/3", true},
		{http.MethodPost, "/api/bots/b/files/uploads", false},
		{http.MethodPost, "/api/bots/b/files/uploads/abc/complete", false},
		{http.MethodDelete, "/api/bots/b/files/uploads/abc", false},
		{http.MethodGet, "/api/bots/b/files/upload", false},
		{http.MethodPut, "/api/bots/b/files/upload", false},
	}
	for _, tc := range cases {
		if got := isWorkspaceFileUpload(tc.method, tc.path); got != tc.want {
			t.Fatalf("isWorkspaceFileUpload(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestChunkUpload_AssemblesOnHostOutOfOrder(t *testing.T) {
	for _, route := range []string{"sessions", "bots"} {
		t.Run(route, func(t *testing.T) {
			s, root, r := newChunkTestServer(t)
			const botID = "bot-1"
			payload := []byte("0123456789abcdefghij") // 20 bytes, chunk size 8 → 8+8+4
			base := "/api/" + route + "/" + botID + "/files/uploads"
			init := chunkInit(t, r, "7", base, "/data/inbox", "my file.rar", len(payload))
			if init.ChunkSize != s.chunkUploadChunkSize() || init.TotalChunks != 3 || init.Size != int64(len(payload)) {
				t.Fatalf("init = %+v", init)
			}
			// 客户端如果自报段大小，服务端仍用自己的段大小。
			if init.ChunkSize != 8 {
				t.Fatalf("chunk size = %d, want test override 8", init.ChunkSize)
			}

			order := []int{2, 0, 1}
			for _, index := range order {
				start := index * int(init.ChunkSize)
				end := start + int(init.ChunkSize)
				if end > len(payload) {
					end = len(payload)
				}
				rec := chunkPut(t, r, "7", base, init.UploadID, index, payload[start:end])
				if rec.Code != http.StatusOK {
					t.Fatalf("put %d: %d %s", index, rec.Code, rec.Body.String())
				}
			}

			// 合并前工作区里还没有最终文件。
			final := filepath.Join(root, botID, "inbox", "my file.rar")
			if _, err := os.Stat(final); !os.IsNotExist(err) {
				t.Fatalf("final file exists before complete: %v", err)
			}

			rec := chunkJSON(t, r, http.MethodPost, "7", base+"/"+init.UploadID+"/complete", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
			}
			got, err := os.ReadFile(final)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("stored %q", got)
			}
			if _, err := os.Stat(final + ".partial"); !os.IsNotExist(err) {
				t.Fatalf("partial left behind: %v", err)
			}
			staging := filepath.Join(root, chunkUploadDirName, init.UploadID)
			if _, err := os.Stat(staging); !os.IsNotExist(err) {
				t.Fatalf("staging dir left behind: %v", err)
			}

			// 再次完成不再找到这次上传。
			again := chunkJSON(t, r, http.MethodPost, "7", base+"/"+init.UploadID+"/complete", nil)
			if again.Code != http.StatusNotFound {
				t.Fatalf("second complete: %d %s", again.Code, again.Body.String())
			}
		})
	}
}

func TestChunkUpload_ReplacesExistingFile(t *testing.T) {
	s, root, r := newChunkTestServer(t)
	_ = s
	const botID = "bot-1"
	final := filepath.Join(root, botID, "note.txt")
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := []byte("NEWFILE!")
	base := "/api/bots/" + botID + "/files/uploads"
	init := chunkInit(t, r, "1", base, "/", "note.txt", len(payload))
	rec := chunkPut(t, r, "1", base, init.UploadID, 0, payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	rec = chunkJSON(t, r, http.MethodPost, "1", base+"/"+init.UploadID+"/complete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("replaced content = %q", got)
	}
}

func TestChunkUpload_RewriteChunkUsesLastBytes(t *testing.T) {
	_, root, r := newChunkTestServer(t)
	payload := []byte("01234567XXXXXXXX")
	base := "/api/sessions/bot-1/files/uploads"
	init := chunkInit(t, r, "3", base, "/inbox", "a.bin", len(payload))
	if rec := chunkPut(t, r, "3", base, init.UploadID, 0, []byte("AAAAAAAA")); rec.Code != http.StatusOK {
		t.Fatalf("first put: %d %s", rec.Code, rec.Body.String())
	}
	if rec := chunkPut(t, r, "3", base, init.UploadID, 0, payload[:8]); rec.Code != http.StatusOK {
		t.Fatalf("rewrite: %d %s", rec.Code, rec.Body.String())
	}
	if rec := chunkPut(t, r, "3", base, init.UploadID, 1, payload[8:]); rec.Code != http.StatusOK {
		t.Fatalf("tail: %d %s", rec.Code, rec.Body.String())
	}
	rec := chunkJSON(t, r, http.MethodPost, "3", base+"/"+init.UploadID+"/complete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "bot-1", "inbox", "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q", got)
	}
}

func TestChunkUpload_RejectsBadChunksAndLeavesNoFile(t *testing.T) {
	_, root, r := newChunkTestServer(t)
	base := "/api/bots/bot-1/files/uploads"
	init := chunkInit(t, r, "1", base, "/inbox", "a.bin", 20)
	final := filepath.Join(root, "bot-1", "inbox", "a.bin")

	short := chunkPut(t, r, "1", base, init.UploadID, 0, []byte("1234567"))
	if short.Code != http.StatusBadRequest || !strings.Contains(short.Body.String(), "must be 8 bytes") {
		t.Fatalf("short: %d %s", short.Code, short.Body.String())
	}
	longBody := bytes.Repeat([]byte("x"), 9)
	longReq := httptest.NewRequest(http.MethodPut, base+"/"+init.UploadID+"/chunks/0", &unknownLen{r: bytes.NewReader(longBody)})
	longReq.ContentLength = -1
	longReq.Header.Set("X-Test-User", "1")
	longRec := httptest.NewRecorder()
	r.ServeHTTP(longRec, longReq)
	if longRec.Code != http.StatusBadRequest {
		t.Fatalf("long without content-length: %d %s", longRec.Code, longRec.Body.String())
	}
	over := chunkPut(t, r, "1", base, init.UploadID, 0, bytes.Repeat([]byte("y"), 9))
	if over.Code != http.StatusBadRequest {
		t.Fatalf("long: %d %s", over.Code, over.Body.String())
	}
	badIndex := chunkPut(t, r, "1", base, init.UploadID, 9, []byte("01234567"))
	if badIndex.Code != http.StatusBadRequest {
		t.Fatalf("index: %d %s", badIndex.Code, badIndex.Body.String())
	}
	badToken := chunkJSON(t, r, http.MethodPut, "1", base+"/"+init.UploadID+"/chunks/nope", bytes.NewReader([]byte("01234567")))
	if badToken.Code != http.StatusBadRequest {
		t.Fatalf("token: %d %s", badToken.Code, badToken.Body.String())
	}
	badID := chunkPut(t, r, "1", base, "not-a-valid-upload-id", 0, []byte("01234567"))
	if badID.Code != http.StatusBadRequest {
		t.Fatalf("id: %d %s", badID.Code, badID.Body.String())
	}

	// 只写入第 0 段和第 2 段，合并必须失败，且不能留下半截最终文件。
	if rec := chunkPut(t, r, "1", base, init.UploadID, 0, []byte("01234567")); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := chunkPut(t, r, "1", base, init.UploadID, 2, []byte("ijkl")); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	missing := chunkJSON(t, r, http.MethodPost, "1", base+"/"+init.UploadID+"/complete", nil)
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), "missing chunk") {
		t.Fatalf("missing: %d %s", missing.Code, missing.Body.String())
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("final file exists after failed complete: %v", err)
	}

	// 失败后仍可补上缺段再完成。
	if rec := chunkPut(t, r, "1", base, init.UploadID, 1, []byte("89abcdef")); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	ok := chunkJSON(t, r, http.MethodPost, "1", base+"/"+init.UploadID+"/complete", nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("retry complete: %d %s", ok.Code, ok.Body.String())
	}
}

func TestChunkUpload_OwnerAndBotIsolation(t *testing.T) {
	_, _, r := newChunkTestServer(t)
	base1 := "/api/bots/bot-1/files/uploads"
	init := chunkInit(t, r, "1", base1, "/", "a.bin", 8)
	otherUser := chunkPut(t, r, "2", base1, init.UploadID, 0, []byte("01234567"))
	if otherUser.Code != http.StatusNotFound {
		t.Fatalf("other user: %d %s", otherUser.Code, otherUser.Body.String())
	}
	otherBot := chunkPut(t, r, "1", "/api/bots/bot-2/files/uploads", init.UploadID, 0, []byte("01234567"))
	if otherBot.Code != http.StatusNotFound {
		t.Fatalf("other bot: %d %s", otherBot.Code, otherBot.Body.String())
	}
	// 原主人仍然可以写完。
	if rec := chunkPut(t, r, "1", base1, init.UploadID, 0, []byte("01234567")); rec.Code != http.StatusOK {
		t.Fatalf("owner put: %d %s", rec.Code, rec.Body.String())
	}
	rec := chunkJSON(t, r, http.MethodPost, "2", base1+"/"+init.UploadID+"/complete", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other user complete: %d %s", rec.Code, rec.Body.String())
	}
	rec = chunkJSON(t, r, http.MethodPost, "1", base1+"/"+init.UploadID+"/complete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner complete: %d %s", rec.Code, rec.Body.String())
	}
}

func TestChunkUpload_RejectsOversizeAndBadName(t *testing.T) {
	_, _, r := newChunkTestServer(t)
	base := "/api/bots/bot-1/files/uploads"
	over := chunkJSON(t, r, http.MethodPost, "1", base, bytes.NewReader(mustJSON(t, map[string]any{
		"path": "/", "name": "big.bin", "size": MaxUserUploadBytes + 1, "chunkSize": 1,
	})))
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), "1000 MB") {
		t.Fatalf("oversize: %d %s", over.Code, over.Body.String())
	}
	for _, name := range []string{"", ".", "..", "a/b", "a\\b", "nested/../x"} {
		rec := chunkJSON(t, r, http.MethodPost, "1", base, bytes.NewReader(mustJSON(t, map[string]any{
			"path": "/", "name": name, "size": 8,
		})))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("name %q: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// 客户端要求 1 字节一段时，服务端仍按自己的段大小切，不会接受海量分片。
	hint := chunkInit(t, r, "1", base, "/", "ok.bin", 8)
	if hint.ChunkSize != 8 || hint.TotalChunks != 1 {
		t.Fatalf("server chunk plan = %+v", hint)
	}
}

func TestChunkUpload_AbortRemovesStaging(t *testing.T) {
	_, root, r := newChunkTestServer(t)
	base := "/api/sessions/bot-1/files/uploads"
	init := chunkInit(t, r, "4", base, "/", "a.bin", 8)
	if rec := chunkPut(t, r, "4", base, init.UploadID, 0, []byte("01234567")); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	staging := filepath.Join(root, chunkUploadDirName, init.UploadID)
	if _, err := os.Stat(filepath.Join(staging, "chunks", "000000")); err != nil {
		t.Fatal(err)
	}
	rec := chunkJSON(t, r, http.MethodDelete, "4", base+"/"+init.UploadID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("abort: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging remains: %v", err)
	}
	rec = chunkPut(t, r, "4", base, init.UploadID, 0, []byte("01234567"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("put after abort: %d %s", rec.Code, rec.Body.String())
	}
}

func TestChunkUpload_SweepDropsAbandonedOnly(t *testing.T) {
	s, root, r := newChunkTestServer(t)
	base := "/api/bots/bot-1/files/uploads"
	oldInit := chunkInit(t, r, "9", base, "/", "old.bin", 8)
	fresh := chunkInit(t, r, "9", base, "/", "new.bin", 8)
	up := s.uploads().byID[oldInit.UploadID]
	up.mu.Lock()
	up.UpdatedAt = up.UpdatedAt.Add(-chunkUploadAbandonedAfter - time.Hour)
	if err := saveChunkMeta(up); err != nil {
		up.mu.Unlock()
		t.Fatal(err)
	}
	up.mu.Unlock()

	n, err := s.sweepAbandonedChunkUploads(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(root, chunkUploadDirName, oldInit.UploadID)); !os.IsNotExist(err) {
		t.Fatalf("old staging remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, chunkUploadDirName, fresh.UploadID, "meta.json")); err != nil {
		t.Fatalf("fresh upload was swept: %v", err)
	}
	// 被清掉的 id 不能再写入。
	rec := chunkPut(t, r, "9", base, oldInit.UploadID, 0, []byte("01234567"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("put swept upload: %d %s", rec.Code, rec.Body.String())
	}
}

func TestChunkUpload_TooManyUnfinished(t *testing.T) {
	s, _, r := newChunkTestServer(t)
	s.chunkUploadMaxActive = 2
	base := "/api/bots/bot-1/files/uploads"
	chunkInit(t, r, "1", base, "/", "a.bin", 8)
	chunkInit(t, r, "1", base, "/", "b.bin", 8)
	rec := chunkJSON(t, r, http.MethodPost, "1", base, bytes.NewReader(mustJSON(t, map[string]any{
		"path": "/", "name": "c.bin", "size": 8,
	})))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third init: %d %s", rec.Code, rec.Body.String())
	}
	// 另一个用户不受这一限额影响。
	other := chunkJSON(t, r, http.MethodPost, "2", base, bytes.NewReader(mustJSON(t, map[string]any{
		"path": "/", "name": "d.bin", "size": 8,
	})))
	if other.Code != http.StatusOK {
		t.Fatalf("other user: %d %s", other.Code, other.Body.String())
	}
}

func TestChunkUpload_ParallelChunks(t *testing.T) {
	_, root, r := newChunkTestServer(t)
	payload := []byte("0123456789abcdefghij")
	base := "/api/bots/bot-1/files/uploads"
	init := chunkInit(t, r, "1", base, "/", "p.bin", len(payload))
	var wg sync.WaitGroup
	errCh := make(chan string, init.TotalChunks)
	for index := 0; index < init.TotalChunks; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			start := index * int(init.ChunkSize)
			end := start + int(init.ChunkSize)
			if end > len(payload) {
				end = len(payload)
			}
			rec := chunkPut(t, r, "1", base, init.UploadID, index, payload[start:end])
			if rec.Code != http.StatusOK {
				errCh <- rec.Body.String()
			}
		}(index)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
	rec := chunkJSON(t, r, http.MethodPost, "1", base+"/"+init.UploadID+"/complete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "bot-1", "p.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q", got)
	}
}

func TestChunkUpload_StreamsAssembledFileIntoWorkspace(t *testing.T) {
	stub := &chunkUploadWS{}
	s, _, r := newChunkTestServer(t)
	s.uploadWorkspaceHook = func(string) (sandbox.Workspace, bool) { return stub, true }
	payload := []byte("0123456789abcdefghij")
	base := "/api/sessions/bot-1/files/uploads"
	init := chunkInit(t, r, "1", base, "/data/inbox", "photo.bin", len(payload))
	for index := 0; index < init.TotalChunks; index++ {
		start := index * int(init.ChunkSize)
		end := start + int(init.ChunkSize)
		if end > len(payload) {
			end = len(payload)
		}
		if rec := chunkPut(t, r, "1", base, init.UploadID, index, payload[start:end]); rec.Code != http.StatusOK {
			t.Fatal(rec.Body.String())
		}
	}
	rec := chunkJSON(t, r, http.MethodPost, "1", base+"/"+init.UploadID+"/complete", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body.String())
	}
	if stub.toolWrites != 0 {
		t.Fatalf("tool WriteFile called %d times", stub.toolWrites)
	}
	if stub.userPath != "/data/inbox/photo.bin" {
		t.Fatalf("path = %q", stub.userPath)
	}
	if !bytes.Equal(stub.buf.Bytes(), payload) {
		t.Fatalf("streamed %q", stub.buf.Bytes())
	}
}

func TestChunkUpload_SingleShotStillWorks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s, root, r := newChunkTestServer(t)
	_ = s
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("path", "/inbox"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", "small.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/bot-1/files/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("single shot: %d %s", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "bot-1", "inbox", "small.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestValidChunkUploadID(t *testing.T) {
	if !validChunkUploadID("0123456789abcdef0123456789abcdef") {
		t.Fatal("expected hex id")
	}
	for _, id := range []string{"", "..", "../x", "ABCDEF", "0123456789abcdef0123456789abcde", "0123456789abcdef0123456789abcdefg"} {
		if validChunkUploadID(id) {
			t.Fatalf("accepted %q", id)
		}
	}
}

type chunkInitData struct {
	UploadID    string `json:"uploadId"`
	ChunkSize   int64  `json:"chunkSize"`
	TotalChunks int    `json:"totalChunks"`
	Size        int64  `json:"size"`
}

type chunkUploadWS struct {
	uploadWS
	buf bytes.Buffer
}

func (w *chunkUploadWS) WriteUserUploadStream(_ context.Context, path string, r io.Reader, limit int64) (int64, error) {
	w.userPath = path
	n, err := sandbox.CopyUserUpload(&w.buf, r, limit)
	w.n = n
	if w.buf.Len() > 0 {
		w.first = w.buf.Bytes()[0]
		w.last = w.buf.Bytes()[w.buf.Len()-1]
		w.saw = true
	}
	return n, err
}

func newChunkTestServer(t *testing.T) (*Server, string, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tmp := t.TempDir()
	store := config.NewStore(nil)
	if err := store.Set(context.Background(), config.KeyWorkspaceDir, tmp); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		store:            store,
		logger:           zap.NewNop().Sugar(),
		chunkUploadBytes: 8,
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if h := c.GetHeader("X-Test-User"); h != "" {
			n, err := strconv.ParseUint(h, 10, 64)
			if err == nil {
				c.Set(ContextKeyUser, &dao.User{ID: uint(n)})
			}
		}
		c.Next()
	})
	r.POST("/api/bots/:id/files/upload", s.handleBotFileUpload)
	r.POST("/api/bots/:id/files/uploads", s.handleBotChunkUploadInit)
	r.PUT("/api/bots/:id/files/uploads/:uploadId/chunks/:index", s.handleBotChunkUploadPut)
	r.POST("/api/bots/:id/files/uploads/:uploadId/complete", s.handleBotChunkUploadComplete)
	r.DELETE("/api/bots/:id/files/uploads/:uploadId", s.handleBotChunkUploadAbort)
	r.POST("/api/sessions/:sid/files/upload", s.handleSessionFileUpload)
	r.POST("/api/sessions/:sid/files/uploads", s.handleSessionChunkUploadInit)
	r.PUT("/api/sessions/:sid/files/uploads/:uploadId/chunks/:index", s.handleSessionChunkUploadPut)
	r.POST("/api/sessions/:sid/files/uploads/:uploadId/complete", s.handleSessionChunkUploadComplete)
	r.DELETE("/api/sessions/:sid/files/uploads/:uploadId", s.handleSessionChunkUploadAbort)
	return s, tmp, r
}

func chunkInit(t *testing.T, r http.Handler, user, base, path, name string, size int) chunkInitData {
	t.Helper()
	rec := chunkJSON(t, r, http.MethodPost, user, base, bytes.NewReader(mustJSON(t, map[string]any{
		"path": path, "name": name, "size": size, "chunkSize": 1,
	})))
	if rec.Code != http.StatusOK {
		t.Fatalf("init %s: %d %s", base, rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data chunkInitData `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.UploadID == "" {
		t.Fatalf("missing upload id: %s", rec.Body.String())
	}
	return envelope.Data
}

func chunkPut(t *testing.T, r http.Handler, user, base, id string, index int, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	return chunkJSON(t, r, http.MethodPut, user, base+"/"+id+"/chunks/"+strconv.Itoa(index), bytes.NewReader(payload))
}

func chunkJSON(t *testing.T, r http.Handler, method, user, url string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, body)
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// unknownLen 隐藏 Len，让请求不带 Content-Length，覆盖只靠读字节数校验的路径。
type unknownLen struct{ r io.Reader }

func (u *unknownLen) Read(p []byte) (int, error) { return u.r.Read(p) }
