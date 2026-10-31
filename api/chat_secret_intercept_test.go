package api

import (
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/secret"
	"go.uber.org/zap"
)

// TestInterceptWebSecretReply Web 聊天先落库再投递：拦截必须在 handler 里、
// 早于 SaveMessage 完成，且用的会话键要与 WebChannel.Inject 的 Channel 一致
// （否则 secret_input 登记的请求永远匹配不上，明文照样落库）。
func TestInterceptWebSecretReply(t *testing.T) {
	v, err := secret.New("")
	if err != nil {
		t.Fatal(err)
	}
	prev := secret.Default()
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })

	const bot, user, reqID = "bot-web-test", "42", "si-web-test"
	if _, err := interaction.Default().RegisterQuestion(interaction.Question{
		ID:          reqID,
		BotID:       bot,
		ChatID:      "web:" + user, // 与 WebChannel.InjectWithID 构造的 Channel 相同
		Question:    "示例用途",
		Kind:        interaction.KindSecret,
		SecretName:  "svc.token",
		TimeoutSecs: 300,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = interaction.Default().Cancel(reqID)
		interaction.Default().CleanupFinal(reqID)
	})

	s := &Server{logger: zap.NewNop().Sugar()}

	// 别的用户的会话不受影响。
	if got := s.interceptWebSecretReply(bot, "43", "RAW_SECRET_VALUE_other1234"); got != "RAW_SECRET_VALUE_other1234" {
		t.Fatalf("other user's message intercepted: %q", got)
	}

	const raw = "虚构密码 web12345?"
	got := s.interceptWebSecretReply(bot, user, raw)
	if strings.Contains(got, raw) {
		t.Fatalf("plaintext would be persisted: %q", got)
	}
	if !strings.Contains(got, "{{secret:svc.token}}") {
		t.Fatalf("persisted text missing placeholder: %q", got)
	}
	if val, ok := v.Get(bot, "svc.token"); !ok || val != raw {
		t.Fatalf("vault value = (%q,%v), want (%q,true)", val, ok, raw)
	}
}

func TestInterceptWebSecretReplyRedactsRegisteredRetries(t *testing.T) {
	v, _ := secret.New("")
	prev := secret.Default()
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })
	const raw = "fictional-retried-credential"
	if err := v.Register("bot", "service", raw, "test"); err != nil {
		t.Fatal(err)
	}
	s := &Server{logger: zap.NewNop().Sugar()}
	if got := s.interceptWebSecretReply("bot", "user", raw); strings.Contains(got, raw) {
		t.Fatal("retry after consumed request would persist plaintext")
	}
}
