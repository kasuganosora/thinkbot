package workflow

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
	"github.com/kasuganosora/thinkbot/llm/openai"
	"github.com/kasuganosora/thinkbot/subagent"
	utilhttp "github.com/kasuganosora/thinkbot/util/http"
)

func classified1113() error {
	le := llm.NewLLMError(
		llm.ErrorReasonQuotaExceeded,
		"openai",
		"provider account has insufficient balance",
	)
	le.SafeDetail = "code=1113"
	return le
}

func TestIsNonRetryable_Classified1113WithoutHTTPStatus(t *testing.T) {
	err := classified1113()
	if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "403") {
		t.Fatalf("fixture must not embed 429/403, got %q", err.Error())
	}
	if utilhttp.IsQuotaExhaustedLoose(err) {
		t.Fatalf("IsQuotaExhaustedLoose unexpectedly true for classified 1113 without 429/403: %q", err.Error())
	}
	if !isNonRetryable(err) {
		t.Fatalf("isNonRetryable = false for already-classified 1113; node layer would retry. err=%q", err.Error())
	}
}

func TestIsNonRetryable_ZeroAndGenericLLMErrorsStayRetryable(t *testing.T) {
	retryableCases := []struct {
		name string
		err  error
	}{
		{
			name: "zero_value_llm_error",
			err:  &llm.LLMError{Message: "transient upstream hiccup"},
		},
		{
			name: "unknown_provider_generic_llm_error",
			err:  llm.NewLLMError(llm.ErrorReasonUnknownProvider, "custom", "temporary failure"),
		},
		{
			name: "invalid_provider_output_generic_llm_error",
			err:  llm.NewLLMError(llm.ErrorReasonInvalidProviderOutput, "openai", "empty chunk"),
		},
		{
			name: "rate_limit_1302",
			err: func() error {
				e := llm.NewLLMError(llm.ErrorReasonRateLimit, "openai", "provider rate limited the request")
				e.SafeDetail = "code=1302"
				return e
			}(),
		},
		{
			name: "overload_1305",
			err: func() error {
				e := llm.NewLLMError(llm.ErrorReasonProviderInternal, "openai", "provider model is overloaded")
				e.SafeDetail = "code=1305"
				return e
			}(),
		},
		{
			name: "transport_error_struct_literal",
			err:  &llm.LLMError{Reason: llm.ErrorReasonTransport, ProviderName: "openai", Message: "provider connection failed"},
		},
		{
			name: "explicit_retryable_override_true",
			err:  llm.NewLLMError(llm.ErrorReasonInvalidRequest, "openai", "transient validation race", llm.WithRetryable(true)),
		},
	}

	for _, tc := range retryableCases {
		t.Run(tc.name, func(t *testing.T) {
			if isNonRetryable(tc.err) {
				t.Fatalf("isNonRetryable(%s) = true, want false (must not block retries for zero/generic/retryable LLMError)", tc.name)
			}
		})
	}

	nonRetryableCases := []struct {
		name string
		err  error
	}{
		{
			name: "classified_quota_1113",
			err:  classified1113(),
		},
		{
			name: "classified_auth",
			err:  llm.NewLLMError(llm.ErrorReasonAuthentication, "openai", "provider rejected the credentials"),
		},
		{
			name: "classified_invalid_request",
			err:  llm.NewLLMError(llm.ErrorReasonInvalidRequest, "openai", "provider request failed with HTTP 400"),
		},
	}

	for _, tc := range nonRetryableCases {
		t.Run(tc.name, func(t *testing.T) {
			if !isNonRetryable(tc.err) {
				t.Fatalf("isNonRetryable(%s) = false, want true", tc.name)
			}
		})
	}
}

func TestSchedulerNode_EndToEnd1113SingleAttemptWhile1302And1305Retry(t *testing.T) {
	// 1. Structured 1113 on default MaxRetries path (MaxRetries=0 -> default 2 retries)
	// must make exactly 1 request end-to-end through Scheduler -> Executor -> SubAgent -> OpenAI.
	var hits1113 atomic.Int32
	srv1113 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits1113.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"1113","message":"DO_NOT_LEAK"}}`)
	}))
	defer srv1113.Close()

	client1113 := openai.New(
		openai.WithAPIKey("dummy-token-not-logged"),
		openai.WithBaseURL(srv1113.URL+"/v1"),
		openai.WithChatMode(),
		openai.WithChatPath("/chat/completions"),
		openai.WithTimeout(2*time.Second),
	)
	saMgr1113 := subagent.NewSubAgentManager(client1113, "model-sol-main")
	exec1113 := NewExecutor(saMgr1113, nil, nil)
	wf1113 := NewWorkflow("wf-e2e-1113", "req", []*DAGNode{
		{ID: "n1", Name: "task1", Task: "run once"}, // MaxRetries left 0 -> default MaxRetries=2
	})
	wf1113.RebuildIndex()
	sched1113 := newMockScheduler(wf1113, exec1113)
	sched1113.Run(context.Background())

	if got := hits1113.Load(); got != 1 {
		t.Fatalf("1113 end-to-end HTTP requests = %d, want 1 (node layer must not retry structured quota/billing error)", got)
	}

	// 2. 1302 (rate limit) and 1305 (overload) on the exact same default MaxRetries=0 path
	// must still retry at the node layer and succeed on the 3rd attempt.
	for _, code := range []string{"1302", "1305"} {
		t.Run("retryable_"+code, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) <= 2 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":"DO_NOT_LEAK"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"}}]}\n\n")
				_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				flusher.Flush()
			}))
			defer srv.Close()

			client := openai.New(
				openai.WithAPIKey("dummy-token-not-logged"),
				openai.WithBaseURL(srv.URL+"/v1"),
				openai.WithChatMode(),
				openai.WithChatPath("/chat/completions"),
				openai.WithTimeout(2*time.Second),
			)
			saMgr := subagent.NewSubAgentManager(client, "model-sol-main")
			exec := NewExecutor(saMgr, nil, nil)
			wf := NewWorkflow("wf-e2e-"+code, "req", []*DAGNode{
				{ID: "n1", Name: "task1", Task: "retry until ok"}, // default MaxRetries=2
			})
			wf.RebuildIndex()
			sched := newMockScheduler(wf, exec)
			status := sched.Run(context.Background())

			if status != WorkflowCompleted {
				t.Fatalf("code %s workflow status = %s, want completed", code, status)
			}
			if got := hits.Load(); got != 3 {
				t.Fatalf("code %s HTTP requests = %d, want 3 (2 failures + 1 success)", code, got)
			}
		})
	}
}
