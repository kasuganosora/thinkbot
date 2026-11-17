package openai

import (
	"context"
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

func TestBigModel1113_NonRetryableBillingOnWrappedAndStreamingPaths(t *testing.T) {
	bodies := []string{
		`{"error":{"code":"1113","message":"DO_NOT_LEAK"}}`,
		`{"error":{"code":1113,"message":"DO_NOT_LEAK"}}`,
	}

	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			var genAttempts atomic.Int32
			genSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				genAttempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, body)
			}))
			defer genSrv.Close()

			cfg := retry.LLMRetryConfig(3)
			cfg.Backoff.Initial = 5 * time.Millisecond
			cfg.Backoff.Max = 20 * time.Millisecond

			rawClient := New(
				WithAPIKey("dummy-token-not-logged"),
				WithBaseURL(genSrv.URL+"/v1"),
				WithChatMode(),
				WithChatPath("/chat/completions"),
				WithTimeout(2*time.Second),
				WithRetry(cfg),
			)
			wrapped := llm.NewQuotaRecordingProvider(rawClient, func(string, int64) int64 { return 0 })

			_, err := wrapped.DoGenerate(context.Background(), llm.GenerateParams{
				Model:    llm.ChatModel("model-sol-main"),
				Messages: []llm.Message{llm.UserMessage("ping")},
			})
			if genAttempts.Load() != 1 {
				t.Fatalf("DoGenerate retried 1113: attempts=%d, want 1", genAttempts.Load())
			}
			assertBigModel1113Error(t, err)

			// Also verify default ShouldRetry (when retry.Config{MaxRetries: 3} has nil ShouldRetry)
			var defaultAttempts atomic.Int32
			defSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defaultAttempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, body)
			}))
			defer defSrv.Close()

			defClient := New(
				WithAPIKey("dummy-token-not-logged"),
				WithBaseURL(defSrv.URL+"/v1"),
				WithChatMode(),
				WithChatPath("/chat/completions"),
				WithTimeout(2*time.Second),
				WithRetry(retry.Config{MaxRetries: 3}),
			)
			_, defErr := defClient.DoGenerate(context.Background(), llm.GenerateParams{
				Model:    llm.ChatModel("model-sol-main"),
				Messages: []llm.Message{llm.UserMessage("ping")},
			})
			if defaultAttempts.Load() != 1 {
				t.Fatalf("default ShouldRetry retried 1113: attempts=%d, want 1", defaultAttempts.Load())
			}
			assertBigModel1113Error(t, defErr)

			// Streaming path wrapped in QuotaRecordingProvider
			var streamAttempts atomic.Int32
			streamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				streamAttempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, body)
			}))
			defer streamSrv.Close()

			streamClient := llm.NewQuotaRecordingProvider(
				newTestClient(streamSrv.URL, WithRetry(cfg)),
				func(string, int64) int64 { return 0 },
			)
			sr, sErr := streamClient.DoStream(context.Background(), llm.GenerateParams{
				Model:    &llm.Model{ID: "model-sol-main"},
				Messages: []llm.Message{llm.UserMessage("ping")},
			})
			if sErr != nil {
				t.Fatalf("DoStream setup failed: %v", sErr)
			}
			var gotStreamErr error
			for part := range sr.Stream {
				if p, ok := part.(*llm.ErrorPart); ok {
					gotStreamErr = p.Error
				}
			}
			if streamAttempts.Load() != 1 {
				t.Fatalf("DoStream retried 1113: attempts=%d, want 1", streamAttempts.Load())
			}
			assertBigModel1113Error(t, gotStreamErr)
		})
	}
}

func assertBigModel1113Error(t *testing.T, err error) {
	t.Helper()
	le, ok := llm.AsLLMError(err)
	if !ok {
		t.Fatalf("expected *llm.LLMError, got %T %v", err, err)
	}
	if le.Reason != llm.ErrorReasonQuotaExceeded {
		t.Fatalf("reason = %q, want %q", le.Reason, llm.ErrorReasonQuotaExceeded)
	}
	if le.Retryable || llm.IsRetryableLLMError(err) {
		t.Fatalf("expected non-retryable error for 1113, got retryable=true")
	}
	if le.SafeDetail != "code=1113" {
		t.Fatalf("SafeDetail = %q, want code=1113", le.SafeDetail)
	}
	if strings.Contains(strings.ToLower(le.Message), "rate limit") {
		t.Fatalf("1113 message must not claim rate limit: %q", le.Message)
	}
	if !strings.Contains(strings.ToLower(le.Message), "balance") && !strings.Contains(strings.ToLower(le.Message), "quota") {
		t.Fatalf("expected safe balance/quota message, got %q", le.Message)
	}
	for _, s := range []string{le.Message, le.SafeDetail, err.Error(), llm.SafeErrorText(err), llm.UserFailureText(err)} {
		if strings.Contains(s, "DO_NOT_LEAK") {
			t.Fatalf("surface %q leaked provider message", s)
		}
	}
}

func TestBigModel1302And1305_RemainRetryableAndDistinctFrom1113(t *testing.T) {
	cases := []struct {
		code       string
		wantReason llm.ErrorReason
		wantMsgSub string
	}{
		{
			code:       "1302",
			wantReason: llm.ErrorReasonRateLimit,
			wantMsgSub: "rate limited",
		},
		{
			code:       "1305",
			wantReason: llm.ErrorReasonProviderInternal,
			wantMsgSub: "overloaded",
		},
	}

	for _, tc := range cases {
		t.Run("code_"+tc.code, func(t *testing.T) {
			body := `{"error":{"code":"` + tc.code + `","message":"DO_NOT_LEAK"}}`

			// 1. Classifier check when retries are disabled
			classifySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, body)
			}))
			defer classifySrv.Close()

			noRetryClient := newTestClient(classifySrv.URL)
			_, err := noRetryClient.DoGenerate(context.Background(), llm.GenerateParams{
				Model:    &llm.Model{ID: "model-sol-main"},
				Messages: []llm.Message{llm.UserMessage("ping")},
			})
			le, ok := llm.AsLLMError(err)
			if !ok {
				t.Fatalf("expected *llm.LLMError, got %T %v", err, err)
			}
			if le.Reason != tc.wantReason || !le.Retryable || !llm.IsRetryableLLMError(err) {
				t.Fatalf("code %s: reason=%q retryable=%v, want %q retryable=true", tc.code, le.Reason, le.Retryable, tc.wantReason)
			}
			if le.Reason == llm.ErrorReasonQuotaExceeded {
				t.Fatalf("code %s regressed into quota_exceeded", tc.code)
			}
			if le.SafeDetail != "code="+tc.code {
				t.Fatalf("SafeDetail = %q, want code=%s", le.SafeDetail, tc.code)
			}
			if !strings.Contains(strings.ToLower(le.Message), tc.wantMsgSub) {
				t.Fatalf("message = %q, want substring %q", le.Message, tc.wantMsgSub)
			}
			if strings.Contains(err.Error(), "DO_NOT_LEAK") || strings.Contains(llm.UserFailureText(err), "DO_NOT_LEAK") {
				t.Fatalf("leaked raw message for %s", tc.code)
			}

			// 2. Wrapped DoGenerate retry recovery (2x429 + 1x200 = 3 attempts)
			var genAttempts atomic.Int32
			genRetrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if genAttempts.Add(1) <= 2 {
					w.Header().Set("Retry-After-MS", "5")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, body)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"model-sol-main","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer genRetrySrv.Close()

			cfg := retry.LLMRetryConfig(3)
			cfg.Backoff.Initial = 5 * time.Millisecond
			cfg.Backoff.Max = 20 * time.Millisecond
			genWrapped := llm.NewQuotaRecordingProvider(newTestClient(genRetrySrv.URL, WithRetry(cfg)), func(string, int64) int64 { return 0 })
			res, gErr := genWrapped.DoGenerate(context.Background(), llm.GenerateParams{
				Model:    &llm.Model{ID: "model-sol-main"},
				Messages: []llm.Message{llm.UserMessage("ping")},
			})
			if gErr != nil || res.Text != "ok" || genAttempts.Load() != 3 {
				t.Fatalf("code %s DoGenerate retry failed: err=%v text=%q attempts=%d", tc.code, gErr, res.Text, genAttempts.Load())
			}

			// 3. Wrapped DoStream retry recovery (2x429 + 1x200 = 3 attempts)
			var streamAttempts atomic.Int32
			streamRetrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if streamAttempts.Add(1) <= 2 {
					w.Header().Set("Retry-After-MS", "5")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, body)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher := w.(http.Flusher)
				writeSSE(w, flusher, mockChatChunk("ok", ""))
				writeSSE(w, flusher, mockChatChunk("", "stop"))
				writeSSE(w, flusher, "[DONE]")
			}))
			defer streamRetrySrv.Close()

			streamWrapped := llm.NewQuotaRecordingProvider(newTestClient(streamRetrySrv.URL, WithRetry(cfg)), func(string, int64) int64 { return 0 })
			sr, sErr := streamWrapped.DoStream(context.Background(), llm.GenerateParams{
				Model:    &llm.Model{ID: "model-sol-main"},
				Messages: []llm.Message{llm.UserMessage("ping")},
			})
			if sErr != nil {
				t.Fatal(sErr)
			}
			var gotText string
			var streamPartErr error
			for part := range sr.Stream {
				switch p := part.(type) {
				case *llm.TextDeltaPart:
					gotText += p.Text
				case *llm.ErrorPart:
					streamPartErr = p.Error
				}
			}
			if streamPartErr != nil || gotText != "ok" || streamAttempts.Load() != 3 {
				t.Fatalf("code %s DoStream retry failed: err=%v text=%q attempts=%d", tc.code, streamPartErr, gotText, streamAttempts.Load())
			}
		})
	}
}
