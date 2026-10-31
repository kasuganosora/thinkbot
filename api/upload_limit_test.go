package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/util/errs"
)

func TestBotFileUpload_StreamsAboveToolCap(t *testing.T) {
	tmp := t.TempDir()
	store := config.NewStore(nil)
	if err := store.Set(context.Background(), config.KeyWorkspaceDir, tmp); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, logger: zap.NewNop().Sugar()}

	const size = (10 << 20) + 1
	n, err := s.streamWorkspaceFile(nil, "bot-local", "/", "eleven.bin", io.LimitReader(repeatByte('Q'), size), MaxUserUploadBytes)
	if err != nil {
		t.Fatalf("stream above old 10MB cap: %v", err)
	}
	if n != size {
		t.Fatalf("wrote %d, want %d", n, size)
	}
	fi, err := os.Stat(filepath.Join(tmp, "bot-local", "eleven.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != size {
		t.Fatalf("on-disk size = %d", fi.Size())
	}
}

func TestStreamWorkspaceFile_OverLimitRemovesPartial(t *testing.T) {
	tmp := t.TempDir()
	store := config.NewStore(nil)
	if err := store.Set(context.Background(), config.KeyWorkspaceDir, tmp); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, logger: zap.NewNop().Sugar()}

	_, err := s.streamWorkspaceFile(nil, "bot-local", "/", "too-big.bin", strings.NewReader("0123456789abcdef"), 8)
	if err == nil || errs.GetCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "1000 MB") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(tmp, "bot-local", "too-big.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("over-limit upload was left on disk: %v", statErr)
	}
}

type uploadWS struct {
	toolWrites int
	userPath   string
	n          int64
	first      byte
	last       byte
	saw        bool
}

func (w *uploadWS) ID() string      { return "stub" }
func (w *uploadWS) WorkDir() string { return "/data" }
func (w *uploadWS) Exec(context.Context, sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	return nil, errors.New("unused")
}
func (w *uploadWS) ReadFile(context.Context, string) ([]byte, error) {
	return nil, errors.New("unused")
}
func (w *uploadWS) WriteFile(context.Context, string, []byte) error {
	w.toolWrites++
	return errors.New("tool WriteFile must not receive user uploads")
}
func (w *uploadWS) WriteUserUploadStream(_ context.Context, path string, r io.Reader, limit int64) (int64, error) {
	w.userPath = path
	n, err := sandbox.CopyUserUpload(countingWriter{w}, r, limit)
	w.n = n
	return n, err
}
func (w *uploadWS) ListDir(context.Context, string) ([]sandbox.FileEntry, error) {
	return nil, errors.New("unused")
}
func (w *uploadWS) HealthCheck(context.Context) sandbox.HealthStatus {
	return sandbox.HealthStatus{}
}
func (w *uploadWS) Close() error { return nil }

type countingWriter struct{ ws *uploadWS }

func (c countingWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !c.ws.saw {
		c.ws.first = p[0]
		c.ws.saw = true
	}
	c.ws.last = p[len(p)-1]
	return len(p), nil
}

func TestSessionUpload_StreamsAboveToolCap(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &uploadWS{}
	s := &Server{
		logger: zap.NewNop().Sugar(),
		uploadWorkspaceHook: func(string) (sandbox.Workspace, bool) {
			return stub, true
		},
	}
	const size = (10 << 20) + 1
	body, contentType := multipartStream(t, "/inbox", "photo.bin", io.LimitReader(repeatByte('U'), size))

	router := gin.New()
	router.POST("/api/sessions/:sid/files/upload", s.handleSessionFileUpload)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/sess-1/files/upload", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if stub.toolWrites != 0 {
		t.Fatalf("tool WriteFile called %d times; user upload must stream", stub.toolWrites)
	}
	if stub.userPath != "/inbox/photo.bin" {
		t.Fatalf("user path = %q", stub.userPath)
	}
	if stub.n != size || stub.first != 'U' || stub.last != 'U' {
		t.Fatalf("streamed n=%d first=%q last=%q", stub.n, stub.first, stub.last)
	}
}

func TestSessionUpload_FileFieldBeforePathRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &uploadWS{}
	s := &Server{
		logger: zap.NewNop().Sugar(),
		uploadWorkspaceHook: func(string) (sandbox.Workspace, bool) {
			return stub, true
		},
	}
	body, contentType := multipartFileThenPath(t, "/notes", "a.txt", []byte("hello"))
	router := gin.New()
	router.POST("/api/sessions/:sid/files/upload", s.handleSessionFileUpload)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/sess-1/files/upload", body)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "path field must precede file") {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if stub.n != 0 || stub.toolWrites != 0 {
		t.Fatalf("upload wrote despite missing leading path: n=%d toolWrites=%d", stub.n, stub.toolWrites)
	}
}

func TestBotUpload_ContentLengthOverLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{logger: zap.NewNop().Sugar()}
	router := gin.New()
	router.POST("/api/bots/:id/files/upload", s.handleBotFileUpload)

	req := httptest.NewRequest(http.MethodPost, "/api/bots/bot-1/files/upload", strings.NewReader("nope"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.ContentLength = maxUploadBodyBytes() + 1
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "1000 MB") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestChatAttachmentLimits(t *testing.T) {
	// 聊天附件不走工作区流式上传，也不受 sandbox MaxFileWrite 约束。10MB+1 仍在单文件 20MB 以内。
	if err := validateChatAttachments([]ChatAttachment{{
		Name: "mid.bin",
		Size: (10 << 20) + 1,
	}}); err != nil {
		t.Fatalf("11MB chat attachment: %v", err)
	}
	if err := validateChatAttachments([]ChatAttachment{{
		Name: "edge.bin",
		Size: MaxChatAttachmentBytes,
	}}); err != nil {
		t.Fatalf("at chat limit: %v", err)
	}

	err := validateChatAttachments([]ChatAttachment{{
		Name: "big.bin",
		Size: MaxChatAttachmentBytes + 1,
	}})
	if err == nil || errs.GetCode(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "20 MB") {
		t.Fatalf("over per-file limit: %v", err)
	}

	err = validateChatAttachments([]ChatAttachment{
		{Name: "a", Size: 20 << 20},
		{Name: "b", Size: 20 << 20},
		{Name: "c", Size: 20 << 20},
		{Name: "d", Size: 1},
	})
	if err == nil || !strings.Contains(err.Error(), "60 MB") {
		t.Fatalf("over total limit: %v", err)
	}

	many := make([]ChatAttachment, MaxChatAttachmentCount+1)
	for i := range many {
		many[i] = ChatAttachment{Name: "f", Size: 1}
	}
	if err := validateChatAttachments(many); err == nil || !strings.Contains(err.Error(), "10") {
		t.Fatalf("over count: %v", err)
	}

	raw := bytes.Repeat([]byte("a"), 32)
	encoded := base64.StdEncoding.EncodeToString(raw)
	if got := attachmentBytes(ChatAttachment{
		Size:    1,
		DataUrl: "data:text/plain;base64," + encoded,
	}); got != int64(len(raw)) {
		t.Fatalf("decoded size = %d, want %d", got, len(raw))
	}
	if err := validateChatAttachments([]ChatAttachment{{
		Name:    "lie.bin",
		Size:    1,
		DataUrl: "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(make([]byte, int(MaxChatAttachmentBytes)+1)),
	}}); err == nil {
		t.Fatal("data URL larger than the declared size was accepted")
	}
}

type repeatByte byte

func (b repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func multipartStream(t *testing.T, path, name string, r io.Reader) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("path", path); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(part, r); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func multipartFileThenPath(t *testing.T, path, name string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("path", path); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}
