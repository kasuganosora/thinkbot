package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// newSendDocTestServer 起一个假的 Telegram Bot API，只响应 sendDocument。
// 回调收到解析后的 multipart 字段与文件内容，便于断言。
func newSendDocTestServer(t *testing.T) (*httptest.Server, *[]map[string]string) {
	t.Helper()
	got := make([]map[string]string, 0, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendDocument") {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 404, "description": "not found"})
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		entry := map[string]string{}
		for k, v := range r.MultipartForm.Value {
			if len(v) > 0 {
				entry[k] = v[0]
			}
		}
		if fileHeaders := r.MultipartForm.File["document"]; len(fileHeaders) > 0 {
			fh := fileHeaders[0]
			entry["__filename"] = fh.Filename
			rc, err := fh.Open()
			if err != nil {
				t.Errorf("open uploaded file: %v", err)
			} else {
				data, _ := io.ReadAll(rc)
				_ = rc.Close()
				entry["__content"] = string(data)
			}
		}
		got = append(got, entry)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// newSendDocTestChannel 构造接了假 API server 的 Channel。
func newSendDocTestChannel(srv *httptest.Server) *TelegramChannel {
	return &TelegramChannel{
		name:  "test-tg",
		botID: "test-bot",
		api:   newAPIClient("TESTTOKEN", 0, srv.URL),
	}
}

func TestSendDocumentTool_UploadsWorkspaceFile(t *testing.T) {
	srv, got := newSendDocTestServer(t)
	ch := newSendDocTestChannel(srv)
	ch.SetFileSource(func(ctx context.Context, botID, path string) ([]byte, error) {
		if botID != "test-bot" {
			return nil, errors.New("unexpected botID " + botID)
		}
		if path != "demo/hello.txt" {
			return nil, errors.New("unexpected path " + path)
		}
		return []byte("hello file"), nil
	})

	defs, err := ch.ChannelTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var docTool *llm.Tool
	for i := range defs {
		if defs[i].Name == "telegram_send_document" {
			docTool = &defs[i].Tool
		}
	}
	if docTool == nil {
		t.Fatal("telegram_send_document not registered in ChannelTools")
	}

	// MessageMeta 注入当前会话 chatId=777；chatId 参数缺省应回落到它
	ctx := agenttools.ContextWithMessageMeta(context.Background(), agenttools.MessageMeta{
		BotID:       "test-bot",
		ChatID:      "777",
		ChannelType: "telegram",
	})
	res, err := docTool.Execute(&llm.ToolExecContext{Context: ctx}, map[string]any{
		"filePath": "demo/hello.txt",
		"caption":  "a demo file",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m, _ := res.(map[string]any)
	if m["success"] != true || m["messageId"] != int64(42) {
		t.Fatalf("unexpected result: %#v", m)
	}
	if m["fileName"] != "hello.txt" {
		t.Errorf("fileName = %v, want hello.txt (path base)", m["fileName"])
	}

	if len(*got) != 1 {
		t.Fatalf("server received %d requests, want 1", len(*got))
	}
	req := (*got)[0]
	if req["chat_id"] != "777" {
		t.Errorf("chat_id = %q, want 777 (from MessageMeta)", req["chat_id"])
	}
	if req["caption"] != "a demo file" {
		t.Errorf("caption = %q", req["caption"])
	}
	if req["__filename"] != "hello.txt" {
		t.Errorf("uploaded filename = %q, want hello.txt", req["__filename"])
	}
	if req["__content"] != "hello file" {
		t.Errorf("uploaded content = %q", req["__content"])
	}
}

func TestSendDocumentTool_FilenameWithQuoteIsEscaped(t *testing.T) {
	srv, got := newSendDocTestServer(t)
	ch := newSendDocTestChannel(srv)
	ch.SetFileSource(func(ctx context.Context, botID, path string) ([]byte, error) {
		if path != `demo/re"port.txt` {
			return nil, errors.New("unexpected path " + path)
		}
		return []byte("quoted name"), nil
	})
	defs, _ := ch.ChannelTools(context.Background())
	var docTool *llm.Tool
	for i := range defs {
		if defs[i].Name == "telegram_send_document" {
			docTool = &defs[i].Tool
		}
	}

	ctx := agenttools.ContextWithMessageMeta(context.Background(), agenttools.MessageMeta{
		BotID:       "test-bot",
		ChatID:      "777",
		ChannelType: "telegram",
	})
	// 文件名含双引号：必须被 multipart 正确转义，否则 Content-Disposition 头部损坏。
	res, err := docTool.Execute(&llm.ToolExecContext{Context: ctx}, map[string]any{
		"filePath": `demo/re"port.txt`,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m, _ := res.(map[string]any)
	if m["fileName"] != `re"port.txt` {
		t.Errorf("fileName = %v, want re\"port.txt", m["fileName"])
	}
	if len(*got) != 1 {
		t.Fatalf("server received %d requests, want 1", len(*got))
	}
	if (*got)[0]["__filename"] != `re"port.txt` {
		t.Errorf("uploaded filename = %q, want re\"port.txt (quote must survive escaping)", (*got)[0]["__filename"])
	}
	if (*got)[0]["__content"] != "quoted name" {
		t.Errorf("uploaded content = %q", (*got)[0]["__content"])
	}
}

func TestSendDocumentTool_RequiresChatContext(t *testing.T) {
	srv, _ := newSendDocTestServer(t)
	ch := newSendDocTestChannel(srv)
	ch.SetFileSource(func(ctx context.Context, botID, path string) ([]byte, error) {
		return []byte("x"), nil
	})
	defs, _ := ch.ChannelTools(context.Background())
	var docTool *llm.Tool
	for i := range defs {
		if defs[i].Name == "telegram_send_document" {
			docTool = &defs[i].Tool
		}
	}

	// 无 MessageMeta 且无显式 chatId → 拒绝（避免发错对象）
	_, err := docTool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"filePath": "demo/hello.txt",
	})
	if err == nil {
		t.Fatal("expected error when chatId is missing and no conversation context")
	}
}

func TestSendDocumentTool_NoFileSourceFails(t *testing.T) {
	srv, _ := newSendDocTestServer(t)
	ch := newSendDocTestChannel(srv)
	// 不注入 fileSource
	defs, _ := ch.ChannelTools(context.Background())
	var docTool *llm.Tool
	for i := range defs {
		if defs[i].Name == "telegram_send_document" {
			docTool = &defs[i].Tool
		}
	}
	_, err := docTool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"filePath": "demo/hello.txt",
		"chatId":   777,
	})
	if err == nil || !strings.Contains(err.Error(), "file source") {
		t.Fatalf("expected file-source error, got %v", err)
	}
}

func TestSendDocumentTool_WorkspaceReadErrorPropagates(t *testing.T) {
	srv, _ := newSendDocTestServer(t)
	ch := newSendDocTestChannel(srv)
	ch.SetFileSource(func(ctx context.Context, botID, path string) ([]byte, error) {
		return nil, errors.New("sandbox: path contains '..' (directory traversal not allowed)")
	})
	defs, _ := ch.ChannelTools(context.Background())
	var docTool *llm.Tool
	for i := range defs {
		if defs[i].Name == "telegram_send_document" {
			docTool = &defs[i].Tool
		}
	}
	_, err := docTool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"filePath": "../../etc/passwd",
		"chatId":   777,
	})
	if err == nil || !strings.Contains(err.Error(), "read workspace file") {
		t.Fatalf("expected read error to propagate, got %v", err)
	}
}

// newSendPhotoTestServer 起一个假的 Telegram Bot API，只响应 sendPhoto。
func newSendPhotoTestServer(t *testing.T) (*httptest.Server, *[]map[string]string) {
	t.Helper()
	got := make([]map[string]string, 0, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 404, "description": "not found"})
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		entry := map[string]string{}
		for k, v := range r.MultipartForm.Value {
			if len(v) > 0 {
				entry[k] = v[0]
			}
		}
		if fileHeaders := r.MultipartForm.File["photo"]; len(fileHeaders) > 0 {
			fh := fileHeaders[0]
			entry["__filename"] = fh.Filename
			rc, err := fh.Open()
			if err != nil {
				t.Errorf("open uploaded photo: %v", err)
			} else {
				data, _ := io.ReadAll(rc)
				_ = rc.Close()
				entry["__content"] = string(data)
			}
		}
		got = append(got, entry)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func findTool(defs []agenttools.ToolDef, name string) *llm.Tool {
	for i := range defs {
		if defs[i].Name == name {
			return &defs[i].Tool
		}
	}
	return nil
}

func TestSendPhotoTool_UploadsWorkspaceImage(t *testing.T) {
	srv, got := newSendPhotoTestServer(t)
	ch := newSendDocTestChannel(srv)
	ch.SetFileSource(func(ctx context.Context, botID, path string) ([]byte, error) {
		if path != "demo/bird.png" {
			return nil, errors.New("unexpected path " + path)
		}
		return []byte("png-bytes"), nil
	})

	defs, err := ch.ChannelTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	photoTool := findTool(defs, "telegram_send_photo")
	if photoTool == nil {
		t.Fatal("telegram_send_photo not registered in ChannelTools")
	}

	ctx := agenttools.ContextWithMessageMeta(context.Background(), agenttools.MessageMeta{
		BotID:       "test-bot",
		ChatID:      "888",
		ChannelType: "telegram",
	})
	res, err := photoTool.Execute(&llm.ToolExecContext{Context: ctx}, map[string]any{
		"filePath": "demo/bird.png",
		"caption":  "a bird photo",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	m, _ := res.(map[string]any)
	if m["success"] != true || m["messageId"] != int64(77) {
		t.Fatalf("unexpected result: %#v", m)
	}
	if m["fileName"] != "bird.png" {
		t.Errorf("fileName = %v, want bird.png (path base)", m["fileName"])
	}

	if len(*got) != 1 {
		t.Fatalf("server received %d requests, want 1", len(*got))
	}
	req := (*got)[0]
	if req["chat_id"] != "888" {
		t.Errorf("chat_id = %q, want 888 (from MessageMeta)", req["chat_id"])
	}
	if req["caption"] != "a bird photo" {
		t.Errorf("caption = %q", req["caption"])
	}
	if req["__filename"] != "bird.png" {
		t.Errorf("uploaded filename = %q, want bird.png", req["__filename"])
	}
	if req["__content"] != "png-bytes" {
		t.Errorf("uploaded content = %q", req["__content"])
	}
}

func TestSendPhotoTool_RejectsOversizedImage(t *testing.T) {
	srv, got := newSendPhotoTestServer(t)
	ch := newSendDocTestChannel(srv)
	ch.SetFileSource(func(ctx context.Context, botID, path string) ([]byte, error) {
		return make([]byte, 9<<20+1), nil
	})

	defs, _ := ch.ChannelTools(context.Background())
	photoTool := findTool(defs, "telegram_send_photo")
	if photoTool == nil {
		t.Fatal("telegram_send_photo not registered")
	}
	ctx := agenttools.ContextWithMessageMeta(context.Background(), agenttools.MessageMeta{ChatID: "888"})
	_, err := photoTool.Execute(&llm.ToolExecContext{Context: ctx}, map[string]any{
		"filePath": "demo/huge.png",
	})
	if err == nil {
		t.Fatal("expected size-limit error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention size limit, got: %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("server received %d requests, want 0 (must not upload oversized image)", len(*got))
	}
}
