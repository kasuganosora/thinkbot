package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/retry"
)

func TestChat401IsAuthAndDoesNotRetryOrLeakBody(t *testing.T) {
	const reflected = "dummy-token-reflected-zzzz"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"rejected `+reflected+`","type":"invalid_request_error","code":"invalid_api_key"}}`)
	}))
	defer srv.Close()
	c := New(
		WithAPIKey("dummy-token-not-logged"),
		WithBaseURL(srv.URL+"/v1"),
		WithChatMode(),
		WithChatPath("/chat/completions"),
		WithTimeout(2*time.Second),
		WithRetry(retry.Config{MaxRetries: 3}),
	)
	_, err := c.DoGenerate(context.Background(), llm.GenerateParams{
		Model:    llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{llm.UserMessage("ping")},
	})
	if hits.Load() != 1 {
		t.Fatalf("attempts %d", hits.Load())
	}
	var le *llm.LLMError
	if !errors.As(err, &le) {
		t.Fatalf("type %T %v", err, err)
	}
	if le.Reason != llm.ErrorReasonAuthentication || le.Retryable {
		t.Fatalf("reason %s retryable %v", le.Reason, le.Retryable)
	}
	if strings.Contains(err.Error(), reflected) || strings.Contains(le.Message, reflected) {
		t.Fatal("credential reflected in error text")
	}
	if le.Message != MsgCredentialsRejected {
		t.Fatalf("message %s", le.Message)
	}
}

func TestChat403IsAccessDeniedNotMalformedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"nope","type":"permission_error","code":"insufficient_permissions"}}`)
	}))
	defer srv.Close()
	c := New(WithAPIKey("dummy-token-not-logged"), WithBaseURL(srv.URL+"/v1"), WithChatMode(), WithChatPath("/chat/completions"), WithTimeout(2*time.Second))
	_, err := c.DoGenerate(context.Background(), llm.GenerateParams{
		Model:    llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{llm.UserMessage("ping")},
	})
	var le *llm.LLMError
	if !errors.As(err, &le) || le.Reason != llm.ErrorReasonAuthentication || le.Retryable {
		t.Fatalf("err %v", err)
	}
	if le.Message != MsgAccessDenied {
		t.Fatalf("message %s", le.Message)
	}
}

func TestStream401DropsBody(t *testing.T) {
	const reflected = "dummy-token-reflected-zzzz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"rejected `+reflected+`","type":"invalid_request_error","code":"invalid_api_key"}}`)
	}))
	defer srv.Close()
	c := New(WithAPIKey("dummy-token-not-logged"), WithBaseURL(srv.URL+"/v1"), WithChatMode(), WithChatPath("/chat/completions"), WithTimeout(2*time.Second), WithRetry(retry.Config{MaxRetries: 0}))
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
	if streamErr == nil || strings.Contains(streamErr.Error(), reflected) {
		t.Fatalf("stream err leaked or missing: %v", streamErr)
	}
	if llm.PublicFailureText(streamErr) != "模型服务拒绝了当前密钥。" {
		t.Fatalf("public %q", llm.PublicFailureText(streamErr))
	}
}
