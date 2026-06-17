package outbound

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// MemoryCallbackRegistry 测试
// ============================================================================

func TestMemoryCallbackRegistry_RegisterAndInvoke(t *testing.T) {
	reg := NewMemoryCallbackRegistry()

	var received CallbackResult
	id := reg.Register("task-1", func(ctx context.Context, result CallbackResult) error {
		received = result
		return nil
	})

	if id != "task-1" {
		t.Fatalf("expected id=task-1, got %q", id)
	}
	if !reg.Has("task-1") {
		t.Fatal("expected Has to return true")
	}
	if reg.Count() != 1 {
		t.Fatalf("expected count=1, got %d", reg.Count())
	}

	err := reg.Invoke(context.Background(), "task-1", CallbackResult{
		CallbackID: "task-1",
		Status:     "success",
		Payload:    "result data",
	})
	if err != nil {
		t.Fatalf("Invoke failed: %v", err)
	}
	if received.Payload != "result data" {
		t.Errorf("received payload = %v", received.Payload)
	}
	if received.Status != "success" {
		t.Errorf("received status = %q", received.Status)
	}
}

func TestMemoryCallbackRegistry_AutoID(t *testing.T) {
	reg := NewMemoryCallbackRegistry()
	id1 := reg.Register("", func(ctx context.Context, result CallbackResult) error { return nil })
	id2 := reg.Register("", func(ctx context.Context, result CallbackResult) error { return nil })
	if id1 == id2 {
		t.Fatalf("auto IDs should be unique: %q == %q", id1, id2)
	}
	if reg.Count() != 2 {
		t.Fatalf("expected count=2, got %d", reg.Count())
	}
}

func TestMemoryCallbackRegistry_InvokeNotFound(t *testing.T) {
	reg := NewMemoryCallbackRegistry()
	err := reg.Invoke(context.Background(), "nonexistent", CallbackResult{})
	if !errors.Is(err, ErrCallbackNotFound) {
		t.Fatalf("expected ErrCallbackNotFound, got %v", err)
	}
}

func TestMemoryCallbackRegistry_Unregister(t *testing.T) {
	reg := NewMemoryCallbackRegistry()
	reg.Register("x", func(ctx context.Context, result CallbackResult) error { return nil })
	reg.Unregister("x")
	if reg.Has("x") {
		t.Fatal("expected Has to return false after Unregister")
	}
	if reg.Count() != 0 {
		t.Fatalf("expected count=0, got %d", reg.Count())
	}
}

func TestMemoryCallbackRegistry_Concurrent(t *testing.T) {
	reg := NewMemoryCallbackRegistry()
	var count int64
	var wg sync.WaitGroup

	// Register concurrently
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.Register("", func(ctx context.Context, result CallbackResult) error {
				atomic.AddInt64(&count, 1)
				return nil
			})
		}()
	}
	wg.Wait()

	if reg.Count() != 50 {
		t.Fatalf("expected 50, got %d", reg.Count())
	}
}

// ============================================================================
// CallbackHandler 测试
// ============================================================================

func TestCallbackHandler_Handle_Success(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	reg := NewMemoryCallbackRegistry()

	var received CallbackResult
	reg.Register("cb-123", func(ctx context.Context, result CallbackResult) error {
		received = result
		return nil
	})

	handler := NewCallbackHandler(reg, logger, tp)

	err := handler.Handle(context.Background(), core.Action{
		Type:    core.ActionCallback,
		Channel: "chat-1",
		UserID:  "user-1",
		Payload: "task completed successfully",
		Metadata: map[string]any{
			"callback_id": "cb-123",
			"status":      "success",
		},
	})
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}

	if received.CallbackID != "cb-123" {
		t.Errorf("callback_id = %q", received.CallbackID)
	}
	if received.Payload != "task completed successfully" {
		t.Errorf("payload = %v", received.Payload)
	}
	if received.Status != "success" {
		t.Errorf("status = %q", received.Status)
	}
}

func TestCallbackHandler_Handle_MissingCallbackID(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	reg := NewMemoryCallbackRegistry()

	handler := NewCallbackHandler(reg, logger, tp)

	err := handler.Handle(context.Background(), core.Action{
		Type:    core.ActionCallback,
		Payload: "data",
		// No Metadata["callback_id"]
	})
	if err == nil {
		t.Fatal("expected error for missing callback_id")
	}
}

func TestCallbackHandler_Handle_CallbackNotFound(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	reg := NewMemoryCallbackRegistry()

	handler := NewCallbackHandler(reg, logger, tp)

	err := handler.Handle(context.Background(), core.Action{
		Type: core.ActionCallback,
		Metadata: map[string]any{
			"callback_id": "nonexistent",
		},
	})
	if !errors.Is(err, ErrCallbackNotFound) {
		t.Fatalf("expected ErrCallbackNotFound, got %v", err)
	}
}

func TestCallbackHandler_Handle_CallbackError(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	reg := NewMemoryCallbackRegistry()

	callbackErr := errors.New("processing failed")
	reg.Register("cb-err", func(ctx context.Context, result CallbackResult) error {
		return callbackErr
	})

	handler := NewCallbackHandler(reg, logger, tp)

	err := handler.Handle(context.Background(), core.Action{
		Type: core.ActionCallback,
		Metadata: map[string]any{
			"callback_id": "cb-err",
			"status":      "error",
			"error":       "something went wrong",
		},
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("expected callbackErr, got %v", err)
	}
}

func TestCallbackHandler_Handle_DefaultStatus(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	reg := NewMemoryCallbackRegistry()

	var received CallbackResult
	reg.Register("cb-default", func(ctx context.Context, result CallbackResult) error {
		received = result
		return nil
	})

	handler := NewCallbackHandler(reg, logger, tp)

	// No "status" in metadata → should default to "success"
	err := handler.Handle(context.Background(), core.Action{
		Type:    core.ActionCallback,
		Payload: "done",
		Metadata: map[string]any{
			"callback_id": "cb-default",
		},
	})
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}
	if received.Status != "success" {
		t.Errorf("expected default status=success, got %q", received.Status)
	}
}

func TestCallbackHandler_Registry(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	reg := NewMemoryCallbackRegistry()

	handler := NewCallbackHandler(reg, logger, tp)

	// Registry() should return the same registry
	if handler.Registry() != reg {
		t.Fatal("Registry() should return the injected registry")
	}
}

// ============================================================================
// SilentHandler 测试
// ============================================================================

func TestSilentHandler_Handle(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	handler := NewSilentHandler(logger, tp)

	err := handler.Handle(context.Background(), core.Action{
		Type:    core.ActionSilent,
		Channel: "chat-1",
		UserID:  "user-1",
		Metadata: map[string]any{
			"reason": "irrelevant chatter",
		},
	})
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}
}

func TestSilentHandler_Handle_NoReason(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	handler := NewSilentHandler(logger, tp)

	// No metadata/reason → should not error
	err := handler.Handle(context.Background(), core.Action{
		Type:    core.ActionSilent,
		Channel: "chat-2",
	})
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}
}

func TestSilentHandler_Handle_NilMetadata(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	handler := NewSilentHandler(logger, tp)

	err := handler.Handle(context.Background(), core.Action{
		Type: core.ActionSilent,
	})
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}
}

// ============================================================================
// MultiDispatcher 集成测试
// ============================================================================

func TestMultiDispatcher_CallbackAndSilent(t *testing.T) {
	tp := noop_trace.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	reg := NewMemoryCallbackRegistry()

	multiDisp := NewMultiDispatcher(logger, tp)

	callbackHandler := NewCallbackHandler(reg, logger, tp)
	silentHandler := NewSilentHandler(logger, tp)

	multiDisp.Register(core.ActionCallback, callbackHandler)
	multiDisp.Register(core.ActionSilent, silentHandler)

	var callbackReceived bool
	reg.Register("integration-cb", func(ctx context.Context, result CallbackResult) error {
		callbackReceived = true
		return nil
	})

	// Dispatch both action types
	err := multiDisp.Dispatch(context.Background(), []core.Action{
		{
			Type:    core.ActionCallback,
			Payload: "done",
			Metadata: map[string]any{
				"callback_id": "integration-cb",
			},
		},
		{
			Type:    core.ActionSilent,
			Channel: "chat-1",
			Metadata: map[string]any{
				"reason": "test",
			},
		},
	})
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}
	if !callbackReceived {
		t.Error("callback should have been invoked")
	}
}
