package bot

import (
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestFailureReplyActions_AuthAndGenericStaySafe(t *testing.T) {
	const reflected = "dummy-token-reflected-zzzz"
	env := &core.Envelope{Message: core.Message{
		BotID:    "bot-synth",
		UserID:   "user-synth",
		Source:   "misskey",
		Channel:  "misskey",
		Metadata: map[string]any{"reply_target": "note-1"},
	}}
	auth := failureReplyActions(env, &llm.LLMError{Reason: llm.ErrorReasonAuthentication, Message: "provider rejected the credentials"})
	if len(auth) != 1 || auth[0].Channel != "note-1" || auth[0].Payload != "模型服务拒绝了当前密钥。" {
		t.Fatalf("auth action %+v", auth)
	}
	denied := failureReplyActions(env, &llm.LLMError{Reason: llm.ErrorReasonAuthentication, Message: "provider denied access"})
	if len(denied) != 1 || denied[0].Payload != "模型服务拒绝了访问。" {
		t.Fatalf("denied %+v", denied)
	}
	if failureReplyActions(env, errString("upstream body "+reflected)) != nil {
		t.Fatal("non-llm error produced a model reply")
	}
	rateLimited := failureReplyActions(env, &llm.LLMError{Reason: llm.ErrorReasonRateLimit, Message: "slow down " + reflected})
	payload, _ := rateLimited[0].Payload.(string)
	if len(rateLimited) != 1 || payload != llm.UserFailureText(&llm.LLMError{Reason: llm.ErrorReasonRateLimit}) || strings.Contains(payload, reflected) {
		t.Fatalf("rate limit llm %+v", rateLimited)
	}
	if rateLimited[0].Metadata["source_channel"] != "misskey" {
		t.Fatalf("source channel %+v", rateLimited[0].Metadata)
	}
	generic := failureReplyActions(env, &llm.LLMError{Reason: llm.ErrorReasonInvalidProviderOutput, Message: "bad output " + reflected})
	genPayload, _ := generic[0].Payload.(string)
	if len(generic) != 1 || genPayload != "模型服务暂时没有返回结果。" || strings.Contains(genPayload, reflected) {
		t.Fatalf("generic llm %+v", generic)
	}
	if failureReplyActions(env, nil) != nil {
		t.Fatal("nil error produced a reply")
	}
}
