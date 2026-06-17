package outbound

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func TestEventEmitter_Nil_NoOp(t *testing.T) {
	// bus=nil 时不应 panic
	emitter := NewEventEmitter(nil, "bot-1")

	if emitter.Enabled() {
		t.Error("expected disabled emitter")
	}

	ctx := context.Background()

	// 所有方法都不应 panic
	emitter.Emit(ctx, EventStageEnter, "t1", nil)
	emitter.EmitMessageReceived(ctx, core.Message{TraceID: "t1"})
	emitter.EmitMessageDone(ctx, "t1", 0, 0)
	emitter.EmitMessageDropped(ctx, "t1", "test")
	emitter.EmitMessageError(ctx, "t1", errors.New("test"))
	emitter.EmitStageEnter(ctx, "t1", "filter")
	emitter.EmitStageExit(ctx, "t1", "filter", time.Second, nil)
	emitter.EmitLLMStart(ctx, "t1", "openai", "gpt-4")
	emitter.EmitLLMTextDelta(ctx, "t1", "hello")
	emitter.EmitLLMReasonDelta(ctx, "t1", "thinking")
	emitter.EmitLLMToolCall(ctx, "t1", "search", "tc-1")
	emitter.EmitLLMToolResult(ctx, "t1", "search", "tc-1")
	emitter.EmitLLMStepDone(ctx, "t1", 0, "stop")
	emitter.EmitLLMDone(ctx, "t1", 100, "stop")
	emitter.EmitLLMError(ctx, "t1", errors.New("llm error"))
	emitter.EmitDecision(ctx, "t1", "reply", 100, 0)
	emitter.EmitDispatchStart(ctx, "t1", 1)
	emitter.EmitDispatchDone(ctx, "t1", 1, time.Second)
	emitter.EmitDispatchError(ctx, "t1", errors.New("dispatch error"))
}

func TestEventEmitter_EmitMessageReceived(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	emitter := NewEventEmitter(bus, "bot-1")
	sub := bus.Subscribe("trace-123")
	defer bus.Unsubscribe(sub)

	if !emitter.Enabled() {
		t.Error("expected enabled emitter")
	}

	msg := core.Message{
		ID:       "msg-1",
		TraceID:  "trace-123",
		Source:   "telegram",
		Channel:  "chat-456",
		UserID:   "user-1",
		ChatType: "private",
		Text:     "hello world",
	}

	ctx := context.Background()
	emitter.EmitMessageReceived(ctx, msg)

	select {
	case event := <-sub.C():
		if event.Type != EventMessageReceived {
			t.Errorf("expected %s, got %s", EventMessageReceived, event.Type)
		}
		if event.TraceID != "trace-123" {
			t.Errorf("expected trace-123, got %s", event.TraceID)
		}
		if event.BotID != "bot-1" {
			t.Errorf("expected bot-1, got %s", event.BotID)
		}
		if event.Data["message_id"] != "msg-1" {
			t.Errorf("expected msg-1, got %v", event.Data["message_id"])
		}
		if event.Data["text_len"] != 11 {
			t.Errorf("expected text_len=11, got %v", event.Data["text_len"])
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestEventEmitter_EmitLLMTextDelta(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	emitter := NewEventEmitter(bus, "bot-1")
	sub := bus.Subscribe("t1")
	defer bus.Unsubscribe(sub)

	ctx := context.Background()
	emitter.EmitLLMTextDelta(ctx, "t1", "Hello ")
	emitter.EmitLLMTextDelta(ctx, "t1", "World!")

	// 收集两个事件
	events := make([]Event, 0, 2)
	for i := 0; i < 2; i++ {
		select {
		case e := <-sub.C():
			events = append(events, e)
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timeout: expected 2 events, got %d", len(events))
		}
	}

	if events[0].Data["text"] != "Hello " {
		t.Errorf("first delta: expected 'Hello ', got %v", events[0].Data["text"])
	}
	if events[1].Data["text"] != "World!" {
		t.Errorf("second delta: expected 'World!', got %v", events[1].Data["text"])
	}
}

func TestEventEmitter_EmitDecision(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	emitter := NewEventEmitter(bus, "bot-1")
	sub := bus.Subscribe("t1")
	defer bus.Unsubscribe(sub)

	ctx := context.Background()
	emitter.EmitDecision(ctx, "t1", "reply_with_note", 200, 50)

	select {
	case event := <-sub.C():
		if event.Type != EventDecision {
			t.Errorf("expected %s, got %s", EventDecision, event.Type)
		}
		if event.Data["decision"] != "reply_with_note" {
			t.Errorf("expected reply_with_note, got %v", event.Data["decision"])
		}
		if event.Data["reply_len"] != 200 {
			t.Errorf("expected 200, got %v", event.Data["reply_len"])
		}
		if event.Data["note_len"] != 50 {
			t.Errorf("expected 50, got %v", event.Data["note_len"])
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestEventEmitter_EmitStageExit_WithError(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	emitter := NewEventEmitter(bus, "bot-1")
	sub := bus.Subscribe("t1")
	defer bus.Unsubscribe(sub)

	ctx := context.Background()
	emitter.EmitStageExit(ctx, "t1", "llm", 500*time.Millisecond, errors.New("timeout"))

	select {
	case event := <-sub.C():
		if event.Type != EventStageExit {
			t.Errorf("expected %s, got %s", EventStageExit, event.Type)
		}
		if event.Stage != "llm" {
			t.Errorf("expected stage=llm, got %s", event.Stage)
		}
		if event.Data["duration_ms"] != int64(500) {
			t.Errorf("expected 500ms, got %v", event.Data["duration_ms"])
		}
		if event.Data["error"] != "timeout" {
			t.Errorf("expected error=timeout, got %v", event.Data["error"])
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestEventEmitter_EmitStageExit_NoError(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	emitter := NewEventEmitter(bus, "bot-1")
	sub := bus.Subscribe("t1")
	defer bus.Unsubscribe(sub)

	ctx := context.Background()
	emitter.EmitStageExit(ctx, "t1", "filter", 10*time.Millisecond, nil)

	select {
	case event := <-sub.C():
		if _, exists := event.Data["error"]; exists {
			t.Error("expected no error field")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestContextWithEmitter(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	emitter := NewEventEmitter(bus, "bot-1")

	ctx := context.Background()
	ctx = ContextWithEmitter(ctx, emitter)

	recovered := EmitterFromContext(ctx)
	if !recovered.Enabled() {
		t.Error("expected enabled emitter from context")
	}
}

func TestEmitterFromContext_NoOp(t *testing.T) {
	ctx := context.Background()
	emitter := EmitterFromContext(ctx)

	if emitter.Enabled() {
		t.Error("expected disabled emitter from empty context")
	}

	// 不应 panic
	emitter.EmitLLMStart(ctx, "t1", "openai", "gpt-4")
}

func TestEventEmitter_FullLifecycle(t *testing.T) {
	bus := NewMemoryEventBus(DefaultMemoryEventBusConfig(), testLogger())
	defer bus.Close()

	emitter := NewEventEmitter(bus, "bot-1")
	sub := bus.Subscribe("t1")
	defer bus.Unsubscribe(sub)

	ctx := context.Background()

	// 模拟完整生命周期
	emitter.EmitMessageReceived(ctx, core.Message{TraceID: "t1", ID: "m1"})
	emitter.EmitStageEnter(ctx, "t1", "filter")
	emitter.EmitStageExit(ctx, "t1", "filter", 5*time.Millisecond, nil)
	emitter.EmitStageEnter(ctx, "t1", "llm")
	emitter.EmitLLMStart(ctx, "t1", "openai", "gpt-4")
	emitter.EmitLLMTextDelta(ctx, "t1", "Hello")
	emitter.EmitLLMDone(ctx, "t1", 100, "stop")
	emitter.EmitStageExit(ctx, "t1", "llm", 2*time.Second, nil)
	emitter.EmitDecision(ctx, "t1", "reply", 5, 0)
	emitter.EmitDispatchStart(ctx, "t1", 1)
	emitter.EmitDispatchDone(ctx, "t1", 1, 50*time.Millisecond)
	emitter.EmitMessageDone(ctx, "t1", 1, 2100*time.Millisecond)

	// 收集所有事件
	expectedTypes := []EventType{
		EventMessageReceived,
		EventStageEnter,
		EventStageExit,
		EventStageEnter,
		EventLLMStart,
		EventLLMTextDelta,
		EventLLMDone,
		EventStageExit,
		EventDecision,
		EventDispatchStart,
		EventDispatchDone,
		EventMessageDone,
	}

	for i, expected := range expectedTypes {
		select {
		case event := <-sub.C():
			if event.Type != expected {
				t.Errorf("event[%d]: expected %s, got %s", i, expected, event.Type)
			}
			if event.BotID != "bot-1" {
				t.Errorf("event[%d]: expected bot-1, got %s", i, event.BotID)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timeout at event[%d]: expected %s", i, expected)
		}
	}
}
