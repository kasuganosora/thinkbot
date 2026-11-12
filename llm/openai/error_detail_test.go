package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestChatErrorDetailKeepsCodeParamAndDropsMessage(t *testing.T) {
	const leaked = "sk-test-LEAK-9999-secret"
	const apiKey = "dummy-token-not-logged"
	body := `{"error":{"message":"rejected ` + leaked + ` because the user said hello","type":"invalid_request_error","code":"unsupported_parameter","param":"max_tokens","hint":"` + leaked + `"}}`
	assertSafeUpstreamDetail(t, false, apiKey, leaked, body, "code=unsupported_parameter", "param=max_tokens", "type=invalid_request_error")
}

func TestChatErrorDetailDropsUnsafeParam(t *testing.T) {
	const leaked = "sk-test-LEAK-9999-secret"
	body := `{"error":{"message":"` + leaked + `","type":"invalid_request_error","code":"unsupported_parameter","param":"max_tokens ` + leaked + `"}}`
	assertSafeUpstreamDetail(t, true, "dummy-token-not-logged", leaked, body, "code=unsupported_parameter", "type=invalid_request_error")
}

func TestStreamErrorDetailKeepsCodeParamAndDropsMessage(t *testing.T) {
	const leaked = "sk-test-LEAK-9999-secret"
	body := `{"error":{"message":"echo ` + leaked + `","type":"invalid_request_error","code":"unsupported_parameter","param":"max_completion_tokens"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Debug-Token", leaked)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	c := New(
		WithAPIKey("dummy-token-not-logged"),
		WithBaseURL(srv.URL+"/v1"),
		WithChatMode(),
		WithChatPath("/chat/completions"),
		WithTimeout(2*time.Second),
	)
	sr, err := c.DoStream(context.Background(), llm.GenerateParams{
		Model:    llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{llm.UserMessage("ping")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for part := range sr.Stream {
		if p, ok := part.(*llm.ErrorPart); ok {
			streamErr = p.Error
		}
	}
	assertDetailText(t, llm.SafeErrorText(streamErr), "dummy-token-not-logged", leaked, "code=unsupported_parameter", "param=max_completion_tokens", "type=invalid_request_error")
	if strings.Contains(llm.SafeErrorText(streamErr), "param=") && !strings.Contains(llm.SafeErrorText(streamErr), "param=max_completion_tokens") {
		t.Fatalf("param leaked: %s", llm.SafeErrorText(streamErr))
	}
}

func assertSafeUpstreamDetail(t *testing.T, dropParam bool, apiKey, leaked, body string, want ...string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Errorf("auth header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Debug-Token", leaked)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	c := New(
		WithAPIKey(apiKey),
		WithBaseURL(srv.URL+"/v1"),
		WithChatMode(),
		WithChatPath("/chat/completions"),
		WithTimeout(2*time.Second),
	)
	_, err := c.DoGenerate(context.Background(), llm.GenerateParams{
		Model:    llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{llm.UserMessage("ping " + leaked)},
	})
	text := llm.SafeErrorText(err)
	assertDetailText(t, text, apiKey, leaked, want...)
	if dropParam && strings.Contains(text, "param=") {
		t.Fatalf("unsafe param kept: %s", text)
	}
	if !dropParam && !strings.Contains(text, "param=") {
		t.Fatalf("param missing: %s", text)
	}
}

func assertDetailText(t *testing.T, text, apiKey, leaked string, want ...string) {
	t.Helper()
	if text == "" {
		t.Fatal("empty safe error")
	}
	for _, part := range want {
		if !strings.Contains(text, part) {
			t.Fatalf("safe text %q missing %q", text, part)
		}
	}
	for _, secret := range []string{leaked, apiKey, "rejected", "hello", "echo", "Bearer", "X-Debug-Token"} {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("safe text leaked %q: %s", secret, text)
		}
	}
}
