package inbound

import (
	"context"
	"testing"
	"time"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// testIngress 创建测试用 Ingress。
func testIngress(bufSize int) *Ingress {
	return NewIngress(
		IngressConfig{BufferSize: bufSize},
		zap.NewNop().Sugar(),
		noop_trace.NewTracerProvider(),
	)
}

// ============================================================================
// Ingress 测试
// ============================================================================

func TestIngress_ReceiveAndConsume(t *testing.T) {
	g := testIngress(16)

	ctx := context.Background()
	err := g.Receive(ctx, core.Message{
		ID:     "msg-1",
		Source: "test",
		Text:   "hello",
	})
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}

	select {
	case env := <-g.C():
		if env.Message.ID != "msg-1" {
			t.Errorf("expected msg-1, got %s", env.Message.ID)
		}
		if env.Message.Text != "hello" {
			t.Errorf("expected hello, got %s", env.Message.Text)
		}
		if env.Message.CreatedAt.IsZero() {
			t.Error("expected non-zero CreatedAt")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestIngress_TryReceive(t *testing.T) {
	// 缓冲区大小 1
	g := testIngress(1)

	// 第一条应该成功
	ok := g.TryReceive(core.Message{ID: "msg-1"})
	if !ok {
		t.Fatal("first TryReceive should succeed")
	}

	// 第二条缓冲区满，应该失败
	ok = g.TryReceive(core.Message{ID: "msg-2"})
	if ok {
		t.Fatal("second TryReceive should fail when buffer full")
	}
}

func TestIngress_ReceiveCancelled(t *testing.T) {
	// 缓冲区满 + ctx 取消
	g := testIngress(1)
	g.TryReceive(core.Message{ID: "fill"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	err := g.Receive(ctx, core.Message{ID: "msg-blocked"})
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestIngress_Close(t *testing.T) {
	g := testIngress(16)

	// 先放一条消息
	g.TryReceive(core.Message{ID: "msg-before-close"})

	g.Close()

	// 关闭后 Receive 应返回错误
	err := g.Receive(context.Background(), core.Message{ID: "msg-after-close"})
	if err == nil {
		t.Fatal("expected error after close")
	}

	// 关闭后 TryReceive 应返回 false
	ok := g.TryReceive(core.Message{ID: "msg-after-close-2"})
	if ok {
		t.Fatal("TryReceive should fail after close")
	}

	// 已缓冲的消息仍可消费
	env, ok := <-g.C()
	if !ok || env.Message.ID != "msg-before-close" {
		t.Fatalf("expected buffered message, got ok=%v env=%v", ok, env)
	}

	// channel 排空后应关闭
	_, ok = <-g.C()
	if ok {
		t.Fatal("channel should be closed after drain")
	}

	// 二次 Close 不应 panic
	g.Close()
}

func TestIngress_Len(t *testing.T) {
	g := testIngress(16)

	if g.Len() != 0 {
		t.Errorf("expected 0 length, got %d", g.Len())
	}

	g.TryReceive(core.Message{ID: "1"})
	g.TryReceive(core.Message{ID: "2"})

	if g.Len() != 2 {
		t.Errorf("expected 2 length, got %d", g.Len())
	}
}

func TestIngress_DefaultConfig(t *testing.T) {
	cfg := DefaultIngressConfig()
	if cfg.BufferSize != 256 {
		t.Errorf("expected 256, got %d", cfg.BufferSize)
	}
}

func TestIngress_MultipleMessages(t *testing.T) {
	g := testIngress(32)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if err := g.Receive(ctx, core.Message{
			ID:   "msg-" + string(rune('0'+i)),
			Text: "hello",
		}); err != nil {
			t.Fatalf("Receive %d failed: %v", i, err)
		}
	}

	received := 0
	timeout := time.After(time.Second)
	for received < 10 {
		select {
		case <-g.C():
			received++
		case <-timeout:
			t.Fatalf("timeout: received %d/10", received)
		}
	}
}

// ============================================================================
// MemoryChannel 测试
// ============================================================================

func TestMemoryChannel_SendReceive(t *testing.T) {
	g := testIngress(16)
	mem := NewMemoryChannel("test", g)

	ctx := context.Background()
	if err := mem.Send(ctx, core.Message{ID: "msg-1", Text: "hello"}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	select {
	case env := <-g.C():
		if env.Message.ID != "msg-1" {
			t.Errorf("expected msg-1, got %s", env.Message.ID)
		}
		if env.Message.Source != "test" {
			t.Errorf("expected source test, got %s", env.Message.Source)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestMemoryChannel_TrySend(t *testing.T) {
	g := testIngress(1)
	mem := NewMemoryChannel("test", g)

	ok := mem.TrySend(core.Message{ID: "1"})
	if !ok {
		t.Fatal("first TrySend should succeed")
	}

	ok = mem.TrySend(core.Message{ID: "2"})
	if ok {
		t.Fatal("second TrySend should fail when buffer full")
	}
}

func TestMemoryChannel_Name(t *testing.T) {
	g := testIngress(1)

	m1 := NewMemoryChannel("custom", g)
	if m1.Name() != "custom" {
		t.Errorf("expected custom, got %s", m1.Name())
	}
	if m1.Type() != "memory" {
		t.Errorf("expected memory, got %s", m1.Type())
	}

	m2 := NewMemoryChannel("", g)
	if m2.Name() != "memory" {
		t.Errorf("expected default memory, got %s", m2.Name())
	}
}

func TestMemoryChannel_DefaultSource(t *testing.T) {
	g := testIngress(16)
	mem := NewMemoryChannel("my-source", g)

	ctx := context.Background()
	// 不设置 Source，Send 应自动填充
	if err := mem.Send(ctx, core.Message{ID: "msg-1", Text: "hi"}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	env := <-g.C()
	if env.Message.Source != "my-source" {
		t.Errorf("expected source my-source, got %s", env.Message.Source)
	}

	// 设置了 Source，应该保留
	if err := mem.Send(ctx, core.Message{ID: "msg-2", Source: "override", Text: "hi"}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	env = <-g.C()
	if env.Message.Source != "override" {
		t.Errorf("expected source override, got %s", env.Message.Source)
	}
}
