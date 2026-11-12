package bot

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/llm"
)

type captureSender struct {
	name string
	got  []core.Action
}

func (s *captureSender) Send(_ context.Context, action core.Action) error {
	s.got = append(s.got, action)
	return nil
}

func dispatchFailure(t *testing.T, source, replyTarget string, err error, prior []core.Action) []core.Action {
	t.Helper()
	env := &core.Envelope{Message: core.Message{
		ID:       "m1",
		BotID:    "bot-synth",
		UserID:   "user-synth",
		Source:   source,
		Channel:  replyTarget,
		Metadata: map[string]any{"reply_target": replyTarget},
	}}
	for _, action := range prior {
		env.AddAction(action)
	}
	actions := failureReplyActions(env, err)
	sender := &captureSender{name: source}
	handler := outbound.NewChannelReplyHandler(zap.NewNop().Sugar(), nil)
	handler.Register(source, sender)
	disp := outbound.NewMultiDispatcher(zap.NewNop().Sugar(), nil)
	disp.Register(core.ActionReply, handler)
	if err := disp.Dispatch(context.Background(), actions); err != nil && len(actions) > 0 {
		t.Fatalf("dispatch %s: %v", source, err)
	}
	return sender.got
}

func TestFailureDelivery_AuthReachesChannelOnce(t *testing.T) {
	auth := &llm.LLMError{Reason: llm.ErrorReasonAuthentication, Message: "provider rejected the credentials"}
	for _, source := range []string{"telegram", "wecom", "misskey", "mastodon"} {
		got := dispatchFailure(t, source, "chat-42", auth, nil)
		if len(got) != 1 {
			t.Fatalf("%s deliveries %d", source, len(got))
		}
		text, _ := got[0].Payload.(string)
		if text != "模型服务拒绝了当前密钥。" || strings.Contains(text, "dummy-token") {
			t.Fatalf("%s payload %q", source, text)
		}
	}
}

func TestFailureDelivery_NonLLMAndExistingReplyStaySilent(t *testing.T) {
	plain := errString("disk full on dummy-token-reflected-zzzz")
	got := dispatchFailure(t, "telegram", "chat-42", plain, nil)
	if len(got) != 0 {
		t.Fatalf("non-llm deliveries %d %+v", len(got), got)
	}
	prior := []core.Action{{
		Type:     core.ActionReply,
		Channel:  "chat-42",
		Payload:  "partial answer",
		Metadata: map[string]any{"source_channel": "telegram"},
	}}
	auth := &llm.LLMError{Reason: llm.ErrorReasonAuthentication, Message: "provider rejected the credentials"}
	got = dispatchFailure(t, "telegram", "chat-42", auth, prior)
	if len(got) != 0 {
		t.Fatalf("duplicate after text %+v", got)
	}
}
