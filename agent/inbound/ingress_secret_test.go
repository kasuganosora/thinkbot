package inbound

import (
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/secret"
)

// 测试替身：把全局 Vault 换成内存库，注册一个待回复的 secret 请求，
// 再模拟用户回复走 consumeSecretReply，验证明文被存库且消息被改写为脱敏文本。
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

func TestConsumeSecretReply_RedactsBeforePipeline(t *testing.T) {
	v := withTestVault(t)
	_, err := interaction.Default().RegisterQuestion(interaction.Question{
		ID:          "si-test-1",
		BotID:       "bot-test",
		ChatID:      "chat-test",
		Question:    "示例用途",
		Kind:        interaction.KindSecret,
		SecretName:  "svc.token",
		TimeoutSecs: 300,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { interaction.Default().CleanupFinal("si-test-1") })

	const raw = "RAW_SECRET_VALUE_abcdefgh"
	msg := &core.Message{BotID: "bot-test", Channel: "chat-test", Text: raw}

	name, ok := consumeSecretReply(msg)
	if !ok || name != "svc.token" {
		t.Fatalf("consumeSecretReply = (%q,%v), want (svc.token,true)", name, ok)
	}
	// 明文已入库。
	if got, found := v.Get("svc.token"); !found || got != raw {
		t.Fatalf("vault value = (%q,%v), want (%q,true)", got, found, raw)
	}
	// 消息正文被改写：不含明文，含占位符。
	if strings.Contains(msg.Text, raw) {
		t.Fatalf("plaintext leaked into message text: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "{{secret:svc.token}}") {
		t.Fatalf("redacted message missing placeholder: %q", msg.Text)
	}
}

func TestConsumeSecretReply_NoPendingPassthrough(t *testing.T) {
	withTestVault(t)
	msg := &core.Message{BotID: "bot-none", Channel: "chat-none", Text: "just a normal message"}
	name, ok := consumeSecretReply(msg)
	if ok || name != "" {
		t.Fatalf("consumeSecretReply = (%q,%v), want (\"\",false)", name, ok)
	}
	// 没有 pending 请求时，正文必须原样保留。
	if msg.Text != "just a normal message" {
		t.Fatalf("message text altered without pending request: %q", msg.Text)
	}
}
