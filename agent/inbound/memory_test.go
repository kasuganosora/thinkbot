package inbound

import (
	"context"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func TestMemorySource_SendReceive(t *testing.T) {
	src := NewMemorySource("test", 10)
	ch := make(chan *core.Envelope, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := src.Start(ctx, ch); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	// 发送消息
	src.Send(core.Message{
		ID:   "msg-1",
		Text: "hello",
	})

	// 接收
	select {
	case env := <-ch:
		if env.Message.ID != "msg-1" {
			t.Errorf("expected msg-1, got %s", env.Message.ID)
		}
		if env.Message.Text != "hello" {
			t.Errorf("expected hello, got %s", env.Message.Text)
		}
		if env.Message.Source != "test" {
			t.Errorf("expected source test, got %s", env.Message.Source)
		}
		if env.Message.CreatedAt.IsZero() {
			t.Error("expected non-zero CreatedAt")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for message")
	}
}

func TestMemorySource_MultipleMessages(t *testing.T) {
	src := NewMemorySource("test", 10)
	ch := make(chan *core.Envelope, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := src.Start(ctx, ch); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	for i := 0; i < 5; i++ {
		src.Send(core.Message{ID: "msg-" + string(rune('0'+i)), Text: "hello"})
	}

	received := 0
	timeout := time.After(2 * time.Second)
	for received < 5 {
		select {
		case <-ch:
			received++
		case <-timeout:
			t.Fatalf("timeout: received %d/5 messages", received)
		}
	}
}

func TestMemorySource_Stop(t *testing.T) {
	src := NewMemorySource("test", 10)
	ch := make(chan *core.Envelope, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := src.Start(ctx, ch); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()

	if err := src.Stop(stopCtx); err != nil {
		t.Fatalf("failed to stop: %v", err)
	}
}

func TestMemorySource_TrySend(t *testing.T) {
	// 使用无缓冲通道测试 TrySend
	src := NewMemorySource("test", 0)

	// 未启动时，TrySend 到无缓冲 channel 应该失败
	ok := src.TrySend(core.Message{ID: "msg-1"})
	if ok {
		t.Error("TrySend should fail on unbuffered channel when not reading")
	}
}

func TestMemorySource_Name(t *testing.T) {
	src1 := NewMemorySource("custom", 0)
	if src1.Name() != "custom" {
		t.Errorf("expected custom, got %s", src1.Name())
	}

	src2 := NewMemorySource("", 0)
	if src2.Name() != "memory" {
		t.Errorf("expected memory, got %s", src2.Name())
	}
}

func TestMemorySource_DoubleStart(t *testing.T) {
	src := NewMemorySource("test", 10)
	ch := make(chan *core.Envelope, 10)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := src.Start(ctx, ch); err != nil {
		t.Fatalf("first start failed: %v", err)
	}
	// Second start should be no-op
	if err := src.Start(ctx, ch); err != nil {
		t.Fatalf("second start should be no-op: %v", err)
	}
}
