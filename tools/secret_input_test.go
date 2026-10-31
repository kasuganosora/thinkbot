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
	for _, sid := range []string{"s1", "s2"} {
		ctx := agenttools.ContextWithMessageMeta(context.Background(), agenttools.MessageMeta{BotID: bot, ChatID: chat, ChannelType: "web"})
		ctx = agenttools.ContextWithCallOrigin(ctx, agenttools.CallOrigin{BotID: bot, SessionID: sid})
		if _, err := execSecretInput(&llm.ToolExecContext{Context: ctx}, map[string]any{"name": sid, "purpose": "fictional test"}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { interaction.Default().CancelSecret(bot, interaction.SecretReplyChatID(chat, sid)) })
	}
	// Reverse response order: FIFO matching would put s2's value into s1.
	for _, sid := range []string{"s2", "s1"} {
		value := "fictional-credential-for-" + sid
		msg := &core.Message{BotID: bot, Channel: chat, Text: value, Metadata: map[string]any{agenttools.ExtraKeyChatSessionID: sid}}
		if name, ok := inbound.InterceptSecretReply(msg); !ok || name != sid {
			t.Fatalf("wrong request: %s %v", name, ok)
		}
		if got, _ := v.Get(bot, sid); got != value {
			t.Fatal("credential stored under another session's name")
		}
	}
}

func TestSecretInputWebRequiresOriginSession(t *testing.T) {
	ctx := agenttools.ContextWithMessageMeta(context.Background(), agenttools.MessageMeta{BotID: "bot", ChatID: "web:42", ChannelType: "web"})
	if _, err := execSecretInput(&llm.ToolExecContext{Context: ctx}, map[string]any{"name": "test", "purpose": "test"}); err == nil {
		t.Fatal("unscoped web request accepted")
	}
}
