package outbound

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"go.uber.org/zap"
)

// stubGuard 按渠道名决定是否放行。
type stubGuard struct {
	blocked map[string]bool
	calls   int
}

func (g *stubGuard) AllowOutbound(_ context.Context, channelName string, _ core.Action) bool {
	g.calls++
	return !g.blocked[channelName]
}

// TestChannelReplyHandler_GuardBlocksOutbound 验证只读守卫拦截对外动作：
// 动作被丢弃、Sender 完全不被调用，且**不返回错误**——
// 只读是管理员有意配置，不是故障，返回错误会被Dispatcher 记为发送失败。
func TestChannelReplyHandler_GuardBlocksOutbound(t *testing.T) {
	h := NewChannelReplyHandler(zap.NewNop().Sugar(), nil)
	sender := &mockSender{}
	h.Register("lurk-channel", sender)
	h.SetGuard(&stubGuard{blocked: map[string]bool{"lurk-channel": true}})

	err := h.Handle(context.Background(), core.Action{
		Type:     core.ActionReply,
		Channel:  "chat-1",
		Payload:  "should not be sent",
		Metadata: map[string]any{"source_channel": "lurk-channel"},
	})
	if err != nil {
		t.Fatalf("blocked action should not surface an error, got %v", err)
	}
	if got := len(sender.sent()); got != 0 {
		t.Fatalf("sender must not be called for read-only channel, got %d sends", got)
	}
}

// TestChannelReplyHandler_GuardAllowsOutbound 验证守卫放行时正常发送。
func TestChannelReplyHandler_GuardAllowsOutbound(t *testing.T) {
	h := NewChannelReplyHandler(zap.NewNop().Sugar(), nil)
	sender := &mockSender{}
	h.Register("open-channel", sender)
	guard := &stubGuard{blocked: map[string]bool{"other": true}}
	h.SetGuard(guard)

	err := h.Handle(context.Background(), core.Action{
		Type:     core.ActionReply,
		Channel:  "chat-1",
		Payload:  "hello",
		Metadata: map[string]any{"source_channel": "open-channel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(sender.sent()); got != 1 {
		t.Fatalf("expected 1 send, got %d", got)
	}
	if guard.calls != 1 {
		t.Fatalf("guard should be consulted exactly once, got %d", guard.calls)
	}
}

// TestChannelReplyHandler_NilGuardSendsNormally 验证未配置守卫时行为不变
// （向后兼容：绝大多数 bot 不设只读）。
func TestChannelReplyHandler_NilGuardSendsNormally(t *testing.T) {
	h := NewChannelReplyHandler(zap.NewNop().Sugar(), nil)
	sender := &mockSender{}
	h.Register("plain", sender)
	// 不调用 SetGuard

	err := h.Handle(context.Background(), core.Action{
		Type:     core.ActionReply,
		Metadata: map[string]any{"source_channel": "plain"},
		Payload:  "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(sender.sent()); got != 1 {
		t.Fatalf("expected 1 send without guard, got %d", got)
	}
}
