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
	other := failureReplyActions(env, &llm.LLMError{Reason: llm.ErrorReasonRateLimit, Message: "slow down " + reflected})
	payload, _ := other[0].Payload.(string)
	if len(other) != 1 || payload != "模型服务暂时没有返回结果。" || strings.Contains(payload, reflected) {
		t.Fatalf("other llm %+v", other)
	}
	if other[0].Metadata["source_channel"] != "misskey" {
		t.Fatalf("source channel %+v", other[0].Metadata)
	}
	if failureReplyActions(env, nil) != nil {
		t.Fatal("nil error produced a reply")
	}
}
