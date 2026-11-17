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

func TestAlternateProviderErrorEnvelopesAndRedaction(t *testing.T) {
	const apiKey = "dummy-token-not-logged"
	forbidden := []string{
		"DO_NOT_LEAK",
		"SECRET",
		"https://example.invalid/key",
		"token=abcd",
		"Client specified",
		apiKey,
	}

	cases := []struct {
		name       string
		body       string
		wantDetail []string
		wantEmpty  bool
	}{
		{
			name:       "openai_structured_envelope",
			body:       `{"error":{"code":"invalid_argument","type":"invalid_request_error","param":"temperature","message":"DO_NOT_LEAK prompt=SECRET url=https://example.invalid/key"}}`,
			wantDetail: []string{"code=invalid_argument", "param=temperature", "type=invalid_request_error"},
		},
		{
			name:       "xai_top_level_code_with_string_error",
			body:       `{"code":"unrecognized_field","error":"DO_NOT_LEAK token=abcd"}`,
			wantDetail: []string{"code=unrecognized_field"},
		},
		{
			name:       "xai_single_token_string_error_never_copied",
			body:       `{"code":"unrecognized_field","error":"DO_NOT_LEAK"}`,
			wantDetail: []string{"code=unrecognized_field"},
		},
		{
			name:       "xai_grpc_canonical_phrase_mapped_to_safe_token",
			body:       `{"code":"Client specified an invalid argument","error":"DO_NOT_LEAK prompt=SECRET url=https://example.invalid/key"}`,
			wantDetail: []string{"code=invalid_argument"},
		},
		{
			name:       "grpc_details_reason_and_field_violation",
			body:       `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"DO_NOT_LEAK prompt=SECRET","details":[{"reason":"UNSUPPORTED_PARAMETER","fieldViolations":[{"field":"temperature","description":"DO_NOT_LEAK"}]}]}}`,
			wantDetail: []string{"code=UNSUPPORTED_PARAMETER", "param=temperature", "type=INVALID_ARGUMENT"},
		},
		{
			name:      "unknown_free_text_falls_back_safely",
			body:      `{"code":"free text DO_NOT_LEAK","error":"DO_NOT_LEAK token=abcd url=https://example.invalid/key"}`,
			wantEmpty: true,
		},
		{
			name:      "non_json_body_falls_back_safely",
			body:      `DO_NOT_LEAK prompt=SECRET url=https://example.invalid/key`,
			wantEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := New(
				WithAPIKey(apiKey),
				WithBaseURL(srv.URL+"/v1"),
				WithChatMode(),
				WithChatPath("/chat/completions"),
				WithTimeout(2*time.Second),
			)

			// 1. DoGenerate
			_, genErr := c.DoGenerate(context.Background(), llm.GenerateParams{
				Model:    llm.ChatModel("model-sol-main"),
				Messages: []llm.Message{llm.UserMessage("ping")},
			})
			le, ok := llm.AsLLMError(genErr)
			if !ok || le.Reason != llm.ErrorReasonInvalidRequest {
				t.Fatalf("expected invalid_request LLMError, got %T %v", genErr, genErr)
			}

			// 2. DoStream
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
			sle, ok := llm.AsLLMError(streamErr)
			if !ok || sle.Reason != llm.ErrorReasonInvalidRequest {
				t.Fatalf("expected stream invalid_request LLMError, got %T %v", streamErr, streamErr)
			}

			for _, item := range []*llm.LLMError{le, sle} {
				if tc.wantEmpty && item.SafeDetail != "" {
					t.Fatalf("expected empty SafeDetail on free-text fallback, got %q", item.SafeDetail)
				}
				for _, want := range tc.wantDetail {
					if !strings.Contains(item.SafeDetail, want) {
						t.Fatalf("SafeDetail %q missing %q", item.SafeDetail, want)
					}
				}
				surfaces := []string{
					item.SafeDetail,
					item.Message,
					item.Error(),
					llm.SafeErrorText(item),
					llm.UserFailureText(item),
				}
				for _, s := range surfaces {
					for _, bad := range forbidden {
						if strings.Contains(s, bad) {
							t.Fatalf("surface %q leaked %q", s, bad)
						}
					}
				}
			}
		})
	}
}
