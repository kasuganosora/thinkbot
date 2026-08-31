package outbound

import (
	"context"
	"fmt"
	"sync"
	"testing"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// 测试辅助
// ============================================================================

// mockSender 是一个记录所有发送动作的 mock Sender。
type mockSender struct {
	mu      sync.Mutex
	actions []core.Action
	err     error // 如果设置了，Send 会返回此错误
}

func (s *mockSender) Send(_ context.Context, action core.Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = append(s.actions, action)
	if s.err != nil {
		return s.err
	}
	return nil
}

func (s *mockSender) sent() []core.Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.Action, len(s.actions))
	copy(out, s.actions)
	return out
}

// ============================================================================
// ChannelReplyHandler 测试
// ============================================================================

func TestChannelReplyHandler_RegisterAndUnregister(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	h := NewChannelReplyHandler(logger, tp)

	if h.RegisteredCount() != 0 {
		t.Fatalf("expected 0 registered, got %d", h.RegisteredCount())
	}

	h.Register("ch-a", &mockSender{})
	h.Register("ch-b", &mockSender{})

	if h.RegisteredCount() != 2 {
		t.Fatalf("expected 2 registered, got %d", h.RegisteredCount())
	}

	h.Unregister("ch-a")
	if h.RegisteredCount() != 1 {
		t.Fatalf("expected 1 registered after unregister, got %d", h.RegisteredCount())
	}

	h.Unregister("ch-b")
	if h.RegisteredCount() != 0 {
		t.Fatalf("expected 0 registered after unregister all, got %d", h.RegisteredCount())
	}
}

func TestChannelReplyHandler_RouteToCorrectSender(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	h := NewChannelReplyHandler(logger, tp)

	senderA := &mockSender{}
	senderB := &mockSender{}
	h.Register("telegram-bot", senderA)
	h.Register("misskey-bot", senderB)

	// 路由到 telegram-bot
	err := h.Handle(context.Background(), core.Action{
		Type:    core.ActionReply,
		Channel: "12345",
		Payload: "hello telegram",
		Metadata: map[string]any{
			"source_channel": "telegram-bot",
		},
	})
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}

	// 路由到 misskey-bot
	err = h.Handle(context.Background(), core.Action{
		Type:    core.ActionReply,
		Channel: "note-abc",
		Payload: "hello misskey",
		Metadata: map[string]any{
			"source_channel": "misskey-bot",
		},
	})
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}

	// 验证各 sender 收到了正确的 action
	aActions := senderA.sent()
	if len(aActions) != 1 {
		t.Fatalf("senderA: expected 1 action, got %d", len(aActions))
	}
	if aActions[0].Payload != "hello telegram" {
		t.Errorf("senderA payload: got %v, want hello telegram", aActions[0].Payload)
	}

	bActions := senderB.sent()
	if len(bActions) != 1 {
		t.Fatalf("senderB: expected 1 action, got %d", len(bActions))
	}
	if bActions[0].Payload != "hello misskey" {
		t.Errorf("senderB payload: got %v, want hello misskey", bActions[0].Payload)
	}
}

func TestChannelReplyHandler_NoSourceChannel(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	h := NewChannelReplyHandler(logger, tp)
	h.Register("ch-a", &mockSender{})

	// Action 没有 source_channel metadata
	err := h.Handle(context.Background(), core.Action{
		Type:    core.ActionReply,
		Channel: "12345",
		Payload: "hello",
	})
	if err == nil {
		t.Fatal("expected error for missing source_channel, got nil")
	}
}

func TestChannelReplyHandler_UnknownChannel(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	h := NewChannelReplyHandler(logger, tp)

	err := h.Handle(context.Background(), core.Action{
		Type:    core.ActionReply,
		Channel: "12345",
		Payload: "hello",
		Metadata: map[string]any{
			"source_channel": "nonexistent-channel",
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown channel, got nil")
	}
}

func TestChannelReplyHandler_SenderError(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	h := NewChannelReplyHandler(logger, tp)

	failSender := &mockSender{err: fmt.Errorf("API rate limited")}
	h.Register("failing-channel", failSender)

	err := h.Handle(context.Background(), core.Action{
		Type:    core.ActionReply,
		Channel: "12345",
		Payload: "hello",
		Metadata: map[string]any{
			"source_channel": "failing-channel",
		},
	})
	if err == nil {
		t.Fatal("expected error from failing sender, got nil")
	}

	// Sender 应该仍然收到了 action（只是返回了错误）
	if len(failSender.sent()) != 1 {
		t.Errorf("expected sender to receive 1 action, got %d", len(failSender.sent()))
	}
}

func TestChannelReplyHandler_ConcurrentAccess(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	h := NewChannelReplyHandler(logger, tp)

	sender := &mockSender{}
	h.Register("concurrent-ch", sender)

	// 并发发送 100 个 action
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = h.Handle(context.Background(), core.Action{
				Type:    core.ActionReply,
				Channel: fmt.Sprintf("chat-%d", idx),
				Payload: fmt.Sprintf("msg-%d", idx),
				Metadata: map[string]any{
					"source_channel": "concurrent-ch",
				},
			})
		}(i)
	}
	wg.Wait()

	if len(sender.sent()) != 100 {
		t.Errorf("expected 100 actions, got %d", len(sender.sent()))
	}
}

// ============================================================================
// MultiDispatcher + ChannelReplyHandler 集成测试
// ============================================================================

func TestMultiDispatcher_WithChannelReplyHandler(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	// 创建 MultiDispatcher
	disp := NewMultiDispatcher(logger, tp)

	// 创建 ChannelReplyHandler
	h := NewChannelReplyHandler(logger, tp)
	sender := &mockSender{}
	h.Register("my-tg-bot", sender)

	// 注册到 MultiDispatcher
	disp.Register(core.ActionReply, h)

	// 通过 MultiDispatcher 分发 Action
	err := disp.Dispatch(context.Background(), []core.Action{
		{
			Type:    core.ActionReply,
			Channel: "12345",
			Payload: "hi from pipeline",
			Metadata: map[string]any{
				"source_channel":      "my-tg-bot",
				"reply_to_message_id": int64(42),
			},
		},
	})
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}

	actions := sender.sent()
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Channel != "12345" {
		t.Errorf("action channel: got %q, want 12345", actions[0].Channel)
	}
	if actions[0].Payload != "hi from pipeline" {
		t.Errorf("action payload: got %v, want hi from pipeline", actions[0].Payload)
	}
}

func TestMultiDispatcher_MixedActionTypes(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	disp := NewMultiDispatcher(logger, tp)

	replyHandler := NewChannelReplyHandler(logger, tp)
	replySender := &mockSender{}
	replyHandler.Register("tg-bot", replySender)
	disp.Register(core.ActionReply, replyHandler)

	// 还注册一个自定义 handler 给 forward
	forwardHandler := NewChannelReplyHandler(logger, tp)
	forwardSender := &mockSender{}
	forwardHandler.Register("mk-bot", forwardSender)
	disp.Register(core.ActionForward, forwardHandler)

	// 混合 dispatch
	err := disp.Dispatch(context.Background(), []core.Action{
		{
			Type:    core.ActionReply,
			Channel: "chat-1",
			Payload: "reply msg",
			Metadata: map[string]any{
				"source_channel": "tg-bot",
			},
		},
		{
			Type:    core.ActionForward,
			Channel: "note-1",
			Payload: "forward msg",
			Metadata: map[string]any{
				"source_channel": "mk-bot",
			},
		},
		{
			Type:    core.ActionDrop, // 无 handler
			Channel: "ignored",
		},
	})
	// ActionDrop 无 handler 不算错误（只是 warn）
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}

	if len(replySender.sent()) != 1 {
		t.Errorf("reply sender: expected 1, got %d", len(replySender.sent()))
	}
	if len(forwardSender.sent()) != 1 {
		t.Errorf("forward sender: expected 1, got %d", len(forwardSender.sent()))
	}
}
