package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/retry"
)

func TestGrokSafeErrorParsing_NeverCopiesRawMessageOrBody(t *testing.T) {
	const apiKey = "dummy-grok-key-not-logged"
	const secret = "sk-test-LEAK-9999-secret"
	forbidden := []string{
		"DO_NOT_LEAK",
		secret,
		"LEAK",
		"https://example.invalid/secret",
		apiKey,
	}

	cases := []struct {
		name       string
		statusCode int
		body       string
		wantReason llm.ErrorReason
		wantDetail []string
		wantEmpty  bool
		retryable  bool
	}{
		{
			name:       "structured_400_keeps_safe_tokens_and_drops_message",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"temperature","message":"DO_NOT_LEAK ` + secret + ` https://example.invalid/secret"}}`,
			wantReason: llm.ErrorReasonInvalidRequest,
			wantDetail: []string{"code=unsupported_parameter", "param=temperature", "type=invalid_request_error"},
			retryable:  false,
		},
		{
			name:       "xai_grpc_canonical_400_envelope",
			statusCode: http.StatusBadRequest,
			body:       `{"code":"Client specified an invalid argument","error":"DO_NOT_LEAK ` + secret + `"}`,
			wantReason: llm.ErrorReasonInvalidRequest,
			wantDetail: []string{"code=invalid_argument"},
			retryable:  false,
		},
		{
			name:       "secret_shaped_code_dropped_from_safe_detail",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"type":"invalid_request_error","code":"` + secret + `","message":"DO_NOT_LEAK"}}`,
			wantReason: llm.ErrorReasonInvalidRequest,
			wantDetail: []string{"type=invalid_request_error"},
			retryable:  false,
		},
		{
			name:       "non_json_body_never_copied_into_message",
			statusCode: http.StatusInternalServerError,
			body:       `<html>DO_NOT_LEAK ` + secret + ` https://example.invalid/secret</html>`,
			wantReason: llm.ErrorReasonProviderInternal,
			wantEmpty:  true,
			retryable:  true,
		},
		{
			name:       "429_billing_1113_classified_as_non_retryable_quota",
			statusCode: http.StatusTooManyRequests,
			body:       `{"error":{"code":"1113","message":"DO_NOT_LEAK ` + secret + `"}}`,
			wantReason: llm.ErrorReasonQuotaExceeded,
			wantDetail: []string{"code=1113"},
			retryable:  false,
		},
		{
			name:       "401_unauthorized_uses_fixed_safe_message",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":{"code":"invalid_api_key","message":"DO_NOT_LEAK ` + secret + `"}}`,
			wantReason: llm.ErrorReasonAuthentication,
			wantDetail: []string{"code=invalid_api_key"},
			retryable:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			noRetry := retry.Config{MaxRetries: 0, ShouldRetry: func(int, error) bool { return false }}
			c := New(
				WithAPIKey(apiKey),
				WithBaseURL(srv.URL),
				WithTimeout(2*time.Second),
				WithRetry(noRetry),
			)

			// 1. Synchronous DoGenerate
			_, genErr := c.DoGenerate(context.Background(), llm.GenerateParams{
				Model:    llm.ChatModel(ModelGrok43),
				Messages: []llm.Message{llm.UserMessage("ping")},
			})
			le, ok := llm.AsLLMError(genErr)
			if !ok {
				t.Fatalf("DoGenerate expected *llm.LLMError, got %T (%v)", genErr, genErr)
			}

			// 2. Streaming DoStream
			sr, err := c.DoStream(context.Background(), llm.GenerateParams{
				Model:    llm.ChatModel(ModelGrok43),
				Messages: []llm.Message{llm.UserMessage("ping")},
			})
			if err != nil {
				t.Fatalf("DoStream setup error: %v", err)
			}
			var streamErr error
			for part := range sr.Stream {
				if p, ok := part.(*llm.ErrorPart); ok {
					streamErr = p.Error
				}
			}
			sle, ok := llm.AsLLMError(streamErr)
			if !ok {
				t.Fatalf("DoStream expected *llm.LLMError in ErrorPart, got %T (%v)", streamErr, streamErr)
			}

			for label, item := range map[string]*llm.LLMError{"DoGenerate": le, "DoStream": sle} {
				if item.ProviderName != "grok" {
					t.Fatalf("%s ProviderName = %q, want grok", label, item.ProviderName)
				}
				if item.Reason != tc.wantReason {
					t.Fatalf("%s Reason = %q, want %q", label, item.Reason, tc.wantReason)
				}
				if item.Retryable != tc.retryable {
					t.Fatalf("%s Retryable = %v, want %v", label, item.Retryable, tc.retryable)
				}
				if tc.wantEmpty && item.SafeDetail != "" {
					t.Fatalf("%s SafeDetail = %q, want empty", label, item.SafeDetail)
				}
				for _, want := range tc.wantDetail {
					if !strings.Contains(item.SafeDetail, want) {
						t.Fatalf("%s SafeDetail %q missing %q", label, item.SafeDetail, want)
					}
				}
				surfaces := []string{
					item.SafeDetail,
					item.Message,
					item.Error(),
					llm.SafeErrorText(item),
					llm.UserFailureText(item),
				}
				if item.Unwrap() != nil {
					surfaces = append(surfaces, item.Unwrap().Error())
				}
				for _, s := range surfaces {
					for _, bad := range forbidden {
						if strings.Contains(s, bad) {
							t.Fatalf("%s surface %q leaked %q", label, s, bad)
						}
					}
				}
			}
		})
	}
}
