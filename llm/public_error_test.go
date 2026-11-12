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
