package tools

import (
	"context"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/secret"
	"testing"
)

func TestSecretInputWebRequestsMatchOnlyTheirOriginSession(t *testing.T) {
	v, _ := secret.New("")
	prev := secret.Default()
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })
	const bot, chat = "secret-test-bot", "web:42"
	requests := map[string]string{}
	for _, sid := range []string{"s1", "s2"} {
		ctx := agenttools.ContextWithMessageMeta(context.Background(), agenttools.MessageMeta{BotID: bot, ChatID: chat, ChannelType: "web"})
		ctx = agenttools.ContextWithCallOrigin(ctx, agenttools.CallOrigin{BotID: bot, SessionID: sid})
		result, err := execSecretInput(&llm.ToolExecContext{Context: ctx}, map[string]any{"name": sid, "purpose": "fictional test"})
		if err != nil {
			t.Fatal(err)
		}
		payload := result.(map[string]any)
		if payload["input_mode"] != "web_form" || payload["expires_at"].(int64) == 0 {
			t.Fatal("missing form contract")
		}
		requests[sid] = payload["question_id"].(string)
		t.Cleanup(func() { interaction.Default().CancelSecret(bot, interaction.SecretReplyChatID(chat, sid)) })
	}
	// Ordinary messages cannot submit a form request.
	msg := &core.Message{BotID: bot, Channel: chat, Text: "a clarification", Metadata: map[string]any{agenttools.ExtraKeyChatSessionID: "s1"}}
	if _, ok := inbound.InterceptSecretReply(msg); ok {
		t.Fatal("ordinary conversation consumed form request")
	}
	// Reverse form order: IDs and scoped session must both match.
	for _, sid := range []string{"s2", "s1"} {
		q, err := interaction.Default().ClaimSecretForm(requests[sid], bot, interaction.SecretReplyChatID(chat, sid), false)
		if err != nil || q.SecretName != sid {
			t.Fatalf("wrong request: %+v %v", q, err)
		}
	}

}

func TestSecretInputWebRequiresOriginSession(t *testing.T) {
	ctx := agenttools.ContextWithMessageMeta(context.Background(), agenttools.MessageMeta{BotID: "bot", ChatID: "web:42", ChannelType: "web"})
	if _, err := execSecretInput(&llm.ToolExecContext{Context: ctx}, map[string]any{"name": "test", "purpose": "test"}); err == nil {
		t.Fatal("unscoped web request accepted")
	}
}
