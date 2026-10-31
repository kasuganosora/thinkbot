package inbound

import (
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/secret"
)

// withTestVault 把全局 Vault 换成内存库，测试结束恢复。
func withTestVault(t *testing.T) *secret.Vault {
	t.Helper()
	v, err := secret.New("")
	if err != nil {
		t.Fatal(err)
	}
	prev := secret.Default()
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })
	return v
}

// registerSecretRequest 在全局注册表登记一个 pending 密钥请求，测试结束清理。
func registerSecretRequest(t *testing.T, id, bot, chat, name string) {
	t.Helper()
	_, err := interaction.Default().RegisterQuestion(interaction.Question{
		ID:          id,
		BotID:       bot,
		ChatID:      chat,
		Question:    "示例用途",
		Kind:        interaction.KindSecret,
		SecretName:  name,
		TimeoutSecs: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = interaction.Default().Cancel(id)
		interaction.Default().CleanupFinal(id)
	})
}

func TestInterceptSecretReply_RedactsBeforePipeline(t *testing.T) {
	v := withTestVault(t)
	registerSecretRequest(t, "si-test-1", "bot-test", "chat-test", "svc.token")

	const raw = "RAW_SECRET_VALUE_abcdefgh"
	msg := &core.Message{BotID: "bot-test", Channel: "chat-test", Text: raw}

	name, ok := InterceptSecretReply(msg)
	if !ok || name != "svc.token" {
		t.Fatalf("InterceptSecretReply = (%q,%v), want (svc.token,true)", name, ok)
	}
	// 明文已入库，且归属发起请求的 bot。
	if got, found := v.Get("bot-test", "svc.token"); !found || got != raw {
		t.Fatalf("vault value = (%q,%v), want (%q,true)", got, found, raw)
	}
	if _, found := v.Get("bot-other", "svc.token"); found {
		t.Fatal("secret registered globally instead of bot-scoped")
	}
	if strings.Contains(msg.Text, raw) {
		t.Fatalf("plaintext leaked into message text: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "{{secret:svc.token}}") {
		t.Fatalf("redacted message missing placeholder: %q", msg.Text)
	}
	// 一次性：再来一条不会被吞。
	next := &core.Message{BotID: "bot-test", Channel: "chat-test", Text: "ANOTHER_VALUE_abcdefgh"}
	if _, ok := InterceptSecretReply(next); ok {
		t.Fatal("secret request consumed twice")
	}
}

func TestInterceptSecretReply_NoPendingPassthrough(t *testing.T) {
	withTestVault(t)
	msg := &core.Message{BotID: "bot-none", Channel: "chat-none", Text: "just a normal message"}
	if name, ok := InterceptSecretReply(msg); ok || name != "" {
		t.Fatalf("InterceptSecretReply = (%q,%v), want (\"\",false)", name, ok)
	}
	if msg.Text != "just a normal message" {
		t.Fatalf("message text altered without pending request: %q", msg.Text)
	}
}

func TestInterceptSecretReply_UnicodeAndPassphrases(t *testing.T) {
	for _, raw := range []string{"虚构密码测试-123456", "仮のパスワード123456", "가상암호123456", "is this a fictional password?", "短？"} {
		t.Run(raw, func(t *testing.T) {
			v := withTestVault(t)
			registerSecretRequest(t, "si-unicode", "bot-test", "chat-unicode", "svc.token")
			msg := &core.Message{BotID: "bot-test", Channel: "chat-unicode", Text: raw}
			if _, ok := InterceptSecretReply(msg); !ok || strings.Contains(msg.Text, raw) {
				t.Fatalf("pending reply was not protected: %q", msg.Text)
			}
			if len(raw) >= secret.MinValueLen {
				if got, _ := v.Get("bot-test", "svc.token"); got != raw {
					t.Fatal("secret not registered")
				}
			}
		})
	}
}

func TestInterceptSecretReply_EmptyReplyKeepsRequest(t *testing.T) {
	withTestVault(t)
	registerSecretRequest(t, "si-empty", "bot-test", "chat-empty", "svc.token")
	msg := &core.Message{BotID: "bot-test", Channel: "chat-empty", Text: "  "}
	if _, ok := InterceptSecretReply(msg); ok {
		t.Fatal("empty reply consumed request")
	}
	interaction.Default().InterruptBySession("bot-test", "chat-empty")
	if !interaction.Default().HasPendingSecret("bot-test", "chat-empty") {
		t.Fatal("request lost")
	}
}

// TestInterceptSecretReply_ShortReplyNotLeaked 过短、登记失败的回复：
// 原文依然不能进 LLM（它可能就是一个短口令），只给模型一条说明。
func TestInterceptSecretReply_ShortReplyNotLeaked(t *testing.T) {
	withTestVault(t)
	registerSecretRequest(t, "si-test-3", "bot-test", "chat-short", "svc.pin")

	const raw = "pin-0042"
	msg := &core.Message{BotID: "bot-test", Channel: "chat-short", Text: raw}
	if _, ok := InterceptSecretReply(msg); !ok {
		t.Fatal("short reply should still be consumed")
	}
	if strings.Contains(msg.Text, raw) {
		t.Fatalf("short secret leaked: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "过短") {
		t.Fatalf("model should be told the reply was too short: %q", msg.Text)
	}
}

// TestInterceptSecretReply_UsesRawCandidate 渠道给正文加了装饰（回复引用等）时，
// 登记的必须是用户亲手输入的原文，且原文不能留在 Metadata 里流进 pipeline。
func TestInterceptSecretReply_UsesRawCandidate(t *testing.T) {
	v := withTestVault(t)
	registerSecretRequest(t, "si-test-4", "bot-test", "chat-raw", "svc.token")

	const raw = "RAW_SECRET_VALUE_xyz12345"
	msg := &core.Message{
		BotID:    "bot-test",
		Channel:  "chat-raw",
		Text:     "[Reply to bot: 请回复令牌]\n" + raw + "\n[note_id: n1]",
		Metadata: map[string]any{core.MetaSecretCandidate: raw},
	}
	if _, ok := InterceptSecretReply(msg); !ok {
		t.Fatal("decorated reply not intercepted")
	}
	if got, _ := v.Get("bot-test", "svc.token"); got != raw {
		t.Fatalf("registered %q, want raw input %q", got, raw)
	}
	if _, left := msg.Metadata[core.MetaSecretCandidate]; left {
		t.Fatal("raw candidate left in metadata")
	}
	if strings.Contains(msg.Text, raw) {
		t.Fatalf("plaintext leaked: %q", msg.Text)
	}
}

func TestInterceptSecretReply_CancelDoesNotRegisterValue(t *testing.T) {
	v := withTestVault(t)
	registerSecretRequest(t, "cancel-test", "bot-test", "chat-cancel", "svc.token")
	msg := &core.Message{BotID: "bot-test", Channel: "chat-cancel", Text: "/cancel-secret"}
	if _, ok := InterceptSecretReply(msg); !ok || !strings.Contains(msg.Text, "取消") {
		t.Fatal("cancellation not intercepted")
	}
	if _, found := v.Get("bot-test", "svc.token"); found {
		t.Fatal("cancel command registered as credential")
	}
	next := &core.Message{BotID: "bot-test", Channel: "chat-cancel", Text: "ordinary conversation"}
	if _, ok := InterceptSecretReply(next); ok {
		t.Fatal("cancelled request still captures messages")
	}
}
