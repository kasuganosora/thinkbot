package llm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/log"
)

func TestSafeErrorText_WrappedCauseIsNotEchoed(t *testing.T) {
	const secret = "dummy-token-reflected-zzzz"
	inner := errs.New("upstream said " + secret)
	outer := errs.Wrap(errs.Wrap(inner, "decode response"), "provider call failed")
	got := SafeErrorText(outer)
	if strings.Contains(got, secret) || strings.Contains(got, "upstream said") || strings.Contains(got, "decode response") {
		t.Fatalf("wrapped cause leaked: %q", got)
	}
	if got != "provider call failed" {
		t.Fatalf("public text %q", got)
	}
	if !strings.Contains(outer.Error(), secret) {
		t.Fatal("structured Error() lost the cause")
	}
}

func TestSafeErrorText_UnregisteredPlaintextIsNotEchoed(t *testing.T) {
	err := fmt.Errorf("upstream said hello-secret-token")
	got := SafeErrorText(err)
	if strings.Contains(got, "hello-secret-token") || strings.Contains(got, "upstream said") {
		t.Fatalf("plaintext echoed: %q", got)
	}
	if got != "request failed" {
		t.Fatalf("public text %q", got)
	}
}

func TestSafeErrorText_LLMErrorDetailOmitsCause(t *testing.T) {
	const secret = "dummy-token-reflected-zzzz"
	le := NewLLMError(ErrorReasonInvalidRequest, "openai", "provider request failed with HTTP 400", WithCause(fmt.Errorf("body %s", secret)))
	le.SafeDetail = "code=unsupported_parameter param=max_tokens type=invalid_request_error"
	got := SafeErrorText(le)
	if strings.Contains(got, secret) || strings.Contains(got, "body ") {
		t.Fatalf("cause leaked: %q", got)
	}
	if !strings.Contains(got, "code=unsupported_parameter") || !strings.Contains(got, "param=max_tokens") || !strings.Contains(got, "type=invalid_request_error") {
		t.Fatalf("detail missing: %q", got)
	}
	if got != le.Error() {
		t.Fatalf("safe text %q error %q", got, le.Error())
	}
}

func TestSafeErrorText_UnknownErrorDoesNotEchoBody(t *testing.T) {
	const reflected = "dummy-token-reflected-zzzz"
	log.RegisterSecret(reflected)
	defer log.UnregisterSecret(reflected)
	err := fmt.Errorf("provider returned %s", reflected)
	got := SafeErrorText(err)
	if strings.Contains(got, reflected) {
		t.Fatalf("safe text echoed the secret: %q", got)
	}
	auth := &LLMError{Reason: ErrorReasonAuthentication, Message: "provider rejected the credentials"}
	if UserFailureText(auth) != "模型服务拒绝了当前密钥。" {
		t.Fatalf("auth text %q", UserFailureText(auth))
	}
	denied := &LLMError{Reason: ErrorReasonAuthentication, Message: "provider denied access"}
	if UserFailureText(denied) != "模型服务拒绝了访问。" {
		t.Fatalf("denied text %q", UserFailureText(denied))
	}
	if UserFailureText(err) != "" {
		t.Fatalf("non-llm error became a model message: %q", UserFailureText(err))
	}
	body := fmt.Errorf(`{"error":{"message":"rejected %s"}}`, reflected)
	if strings.Contains(SafeErrorText(body), reflected) || strings.Contains(SafeErrorText(body), "rejected") {
		t.Fatalf("json body echoed: %q", SafeErrorText(body))
	}
}

func TestUserFailureText_DistinctCategoriesWithoutLeakingSecrets(t *testing.T) {
	const leak = "DO_NOT_LEAK prompt=SECRET url=https://example.invalid/key token=abcd"
	categories := []struct {
		name   string
		err    *LLMError
		subStr string
	}{
		{
			name:   "invalid_request",
			err:    &LLMError{Reason: ErrorReasonInvalidRequest, Message: leak, SafeDetail: "code=invalid_argument param=temperature"},
			subStr: "请求参数",
		},
		{
			name:   "rate_limit",
			err:    &LLMError{Reason: ErrorReasonRateLimit, Message: leak, SafeDetail: "code=1302"},
			subStr: "频繁",
		},
		{
			name:   "quota_exceeded",
			err:    &LLMError{Reason: ErrorReasonQuotaExceeded, Message: leak, SafeDetail: "code=1113"},
			subStr: "额度",
		},
		{
			name:   "auth_key",
			err:    &LLMError{Reason: ErrorReasonAuthentication, Message: "provider rejected the credentials"},
			subStr: "密钥",
		},
		{
			name:   "auth_denied",
			err:    &LLMError{Reason: ErrorReasonAuthentication, Message: "provider denied access"},
			subStr: "访问",
		},
		{
			name:   "context_overflow",
			err:    &LLMError{Reason: ErrorReasonContextOverflow, Message: leak},
			subStr: "上下文",
		},
		{
			name:   "content_policy",
			err:    &LLMError{Reason: ErrorReasonContentPolicy, Message: leak},
			subStr: "内容安全",
		},
		{
			name:   "no_route",
			err:    &LLMError{Reason: ErrorReasonNoRoute, Message: leak},
			subStr: "模型",
		},
		{
			name:   "provider_internal",
			err:    &LLMError{Reason: ErrorReasonProviderInternal, Message: leak, SafeDetail: "code=1305"},
			subStr: "繁忙",
		},
		{
			name:   "transport",
			err:    &LLMError{Reason: ErrorReasonTransport, Message: leak},
			subStr: "连接",
		},
	}

	seen := map[string]string{}
	for _, tc := range categories {
		got := UserFailureText(tc.err)
		if got == "" {
			t.Fatalf("%s returned empty UserFailureText", tc.name)
		}
		if got == "（生成中断）" || got == "模型服务暂时没有返回结果。" {
			t.Fatalf("%s collapsed to generic fallback %q", tc.name, got)
		}
		if !strings.Contains(got, tc.subStr) {
			t.Fatalf("%s = %q, want substring %q", tc.name, got, tc.subStr)
		}
		for _, bad := range []string{"DO_NOT_LEAK", "SECRET", "https://", "token=abcd", "invalid_argument", "temperature", "1113", "1302", "1305"} {
			if strings.Contains(got, bad) {
				t.Fatalf("%s leaked %q in public text %q", tc.name, bad, got)
			}
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("%s and %s share identical public text %q", prev, tc.name, got)
		}
		seen[got] = tc.name
	}

	unknown := &LLMError{Reason: ErrorReasonInvalidProviderOutput, Message: leak}
	if got := UserFailureText(unknown); got != "模型服务暂时没有返回结果。" {
		t.Fatalf("unknown category fallback = %q", got)
	}
}
