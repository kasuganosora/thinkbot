package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	httputil "github.com/kasuganosora/thinkbot/util/http"
)

func TestIsTextualContent(t *testing.T) {
	cases := []struct {
		mime     string
		filename string
		want     bool
	}{
		{"text/plain", "note.txt", true},
		{"text/markdown", "readme.md", true},
		{"application/json", "x.json", true},
		{"application/x-yaml", "a.yaml", true},
		{"", "main.go", true},
		{"", "style.css", true},
		{"", "data.csv", true},
		{"image/png", "pic.png", false},
		{"application/pdf", "doc.pdf", false},
		{"application/zip", "a.zip", false},
		{"", "a.bin", false},
		{"", "noext", false},
	}
	for _, c := range cases {
		if got := isTextualContent(c.mime, c.filename); got != c.want {
			t.Errorf("isTextualContent(%q,%q)=%v want %v", c.mime, c.filename, got, c.want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{20 * 1024 * 1024, "20.0MB"},
	}
	for _, c := range cases {
		if got := humanSize(c.n); got != c.want {
			t.Errorf("humanSize(%d)=%q want %q", c.n, got, c.want)
		}
	}
}

func TestFormatFileText(t *testing.T) {
	if got := formatFileText("", "a.txt", "hello"); got != "[文件: a.txt]\nhello" {
		t.Errorf("formatFileText empty caption = %q", got)
	}
	if got := formatFileText("看这个", "a.txt", "hello"); got != "看这个\n\n[文件: a.txt]\nhello" {
		t.Errorf("formatFileText with caption = %q", got)
	}
}

func TestFormatFileNote(t *testing.T) {
	if got := formatFileNote("", "a.pdf", 2048, "application/pdf"); got != "[文件: a.pdf (2.0KB application/pdf)]" {
		t.Errorf("formatFileNote = %q", got)
	}
	if got := formatFileNote("处理下", "a.pdf", 2048, "application/pdf"); got != "处理下\n\n[文件: a.pdf (2.0KB application/pdf)]" {
		t.Errorf("formatFileNote with caption = %q", got)
	}
}

// newTestAPIClient 用两个 httptest server 构造 apiClient：
// apiServer 响应 getFile，fileServer 返回实际文件字节。
func newTestAPIClient(t *testing.T, getFileBody, fileBody string) *apiClient {
	t.Helper()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(getFileBody))
	}))
	t.Cleanup(apiSrv.Close)
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(fileBody))
	}))
	t.Cleanup(fileSrv.Close)

	return &apiClient{
		client:     httputil.New(httputil.WithBaseURL(apiSrv.URL + "/botTEST")),
		fileClient: httputil.New(httputil.WithBaseURL(fileSrv.URL+"/file/botTEST"), httputil.WithMaxBodySize(maxTelegramDownloadBytes)),
		token:      "TEST",
	}
}

func TestAcquireInboundFile_TextInline(t *testing.T) {
	api := newTestAPIClient(t,
		`{"ok":true,"result":{"file_id":"F","file_path":"documents/f.txt"}}`,
		"package main\nfunc main(){}",
	)
	ch := &TelegramChannel{name: "tg-test", botID: "b", api: api}

	msg := &Message{Document: &Document{FileID: "F", FileName: "main.go", MimeType: "text/plain"}}
	text, atts := ch.acquireInboundFile(context.Background(), msg, "", "[文件: main.go]")

	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(atts))
	}
	if atts[0].Type != core.AttachmentTypeFile {
		t.Errorf("attachment type = %q", atts[0].Type)
	}
	if string(atts[0].Data) != "package main\nfunc main(){}" {
		t.Errorf("attachment data = %q", string(atts[0].Data))
	}
	// 文本类应内联到消息文本（修复「没文字」）
	want := "[文件: main.go]\npackage main\nfunc main(){}"
	if text != want {
		t.Errorf("inlined text = %q want %q", text, want)
	}
}

func TestAcquireInboundFile_BinaryNote(t *testing.T) {
	api := newTestAPIClient(t,
		`{"ok":true,"result":{"file_id":"F","file_path":"documents/f.bin"}}`,
		"\x00\x01\x02 binaries",
	)
	ch := &TelegramChannel{name: "tg-test", botID: "b", api: api}

	msg := &Message{Document: &Document{FileID: "F", FileName: "a.bin", MimeType: "application/octet-stream"}}
	text, atts := ch.acquireInboundFile(context.Background(), msg, "", "[文件: a.bin]")

	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(atts))
	}
	// 二进制不应内联内容，仅记带大小的占位
	if text == "" || text == "[文件: a.bin]" {
		t.Errorf("binary note text = %q (should include size)", text)
	}
	if !strings.Contains(text, "application/octet-stream") {
		t.Errorf("binary note missing mime: %q", text)
	}
}

func TestAcquireInboundFile_DownloadFailFallback(t *testing.T) {
	// getFile 返回失败：应回退占位文本、不挂附件。
	api := newTestAPIClient(t, `{"ok":false,"error_code":400,"description":"bad"}`, "")
	ch := &TelegramChannel{name: "tg-test", botID: "b", api: api}

	msg := &Message{Document: &Document{FileID: "F", FileName: "x.txt"}}
	text, atts := ch.acquireInboundFile(context.Background(), msg, "", "[文件: x.txt]")

	if len(atts) != 0 {
		t.Errorf("expected no attachment on download failure, got %d", len(atts))
	}
	if text != "[文件: x.txt]" {
		t.Errorf("fallback text = %q", text)
	}
}
