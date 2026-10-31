package wecom

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/secret"
)

// TestHandleMsgCallback_SecretReplyRedactedOnAppendPath bot 正忙时，
// 用户对密钥请求的回复走 AppendToMessage 直接注入编排、**不经过 Ingress**。
// 拦截必须发生在渠道入口：注入的只能是占位提示，明文只进保险库（且归属本 bot）。
func TestHandleMsgCallback_SecretReplyRedactedOnAppendPath(t *testing.T) {
	v, err := secret.New("")
	if err != nil {
		t.Fatal(err)
	}
	prev := secret.Default()
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })

	ctl := newFakeTurnCtl()
	c := newTurnTestChannel(ctl, "")
	c.botID = "bot-wecom-test"
	var mu sync.Mutex
	var got []core.Message
	c.ingress = newRecordingIngress(t, &mu, &got)

	chat := PrefixDM + "U1"
	const reqID = "si-wecom-append"
	if _, err := interaction.Default().RegisterQuestion(interaction.Question{
		ID:          reqID,
		BotID:       c.botID,
		ChatID:      chat,
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

	// 上一轮还在跑 → 这条回复会走 append。
	beginLiveTurn(c, ctl, chat, "T-live")
	t.Cleanup(func() { ctl.markLive("T-live", false) })

	const raw = "虚构密码 wecom1234?"
	c.handleMsgCallback(context.Background(), &callbackBody{
		MsgID:    "m-secret-1",
		ChatType: "single",
		MsgType:  MsgTypeText,
		From: struct {
			UserID string `json:"userid"`
		}{UserID: "U1"},
		Text: &textContent{Content: raw},
	})

	ctl.mu.Lock()
	appended := append([]string(nil), ctl.appended...)
	ctl.mu.Unlock()
	if len(appended) != 1 {
		t.Fatalf("expected exactly one append into the running turn, got %d: %q", len(appended), appended)
	}
	if strings.Contains(appended[0], raw) {
		t.Fatalf("plaintext secret injected into running turn: %q", appended[0])
	}
	if !strings.Contains(appended[0], "{{secret:svc.token}}") {
		t.Fatalf("appended text missing placeholder: %q", appended[0])
	}
	if val, ok := v.Get(c.botID, "svc.token"); !ok || val != raw {
		t.Fatalf("vault value = (%q,%v), want (%q,true)", val, ok, raw)
	}
	if _, ok := v.Get("bot-other", "svc.token"); ok {
		t.Fatal("secret registered globally instead of bot-scoped")
	}
	if interaction.Default().HasPendingSecret(c.botID, chat) {
		t.Fatal("secret request still pending after being consumed")
	}
}
