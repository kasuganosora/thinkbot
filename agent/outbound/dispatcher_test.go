package outbound

import (
	"context"
	"errors"
	"testing"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func TestLogDispatcher_Dispatch(t *testing.T) {
	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()
	d := NewLogDispatcher(logger, tp)

	actions := []core.Action{
		{Type: core.ActionReply, Channel: "ch-1", Payload: "hello"},
		{Type: core.ActionForward, Channel: "ch-2", UserID: "user-1"},
	}

	err := d.Dispatch(context.Background(), actions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLogDispatcher_EmptyActions(t *testing.T) {
	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()
	d := NewLogDispatcher(logger, tp)

	err := d.Dispatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMultiDispatcher_Route(t *testing.T) {
	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()
	d := NewMultiDispatcher(logger, tp)

	var replyCalled, forwardCalled bool

	d.Register(core.ActionReply, ActionHandlerFunc(func(ctx context.Context, a core.Action) error {
		replyCalled = true
		if a.Channel != "ch-1" {
			t.Errorf("expected ch-1, got %s", a.Channel)
		}
		return nil
	}))

	d.Register(core.ActionForward, ActionHandlerFunc(func(ctx context.Context, a core.Action) error {
		forwardCalled = true
		return nil
	}))

	actions := []core.Action{
		{Type: core.ActionReply, Channel: "ch-1"},
		{Type: core.ActionForward, Channel: "ch-2"},
	}

	err := d.Dispatch(context.Background(), actions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !replyCalled {
		t.Error("reply handler should be called")
	}
	if !forwardCalled {
		t.Error("forward handler should be called")
	}
}

func TestMultiDispatcher_Fallback(t *testing.T) {
	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()
	d := NewMultiDispatcher(logger, tp)

	var fallbackCalled bool
	d.SetFallback(ActionHandlerFunc(func(ctx context.Context, a core.Action) error {
		fallbackCalled = true
		return nil
	}))

	actions := []core.Action{
		{Type: core.ActionBroadcast, Channel: "ch-1"}, // 无注册 handler
	}

	err := d.Dispatch(context.Background(), actions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fallbackCalled {
		t.Error("fallback should be called for unregistered action type")
	}
}

func TestMultiDispatcher_Error(t *testing.T) {
	logger := zap.NewNop().Sugar()
	tp := noop_trace.NewTracerProvider()
	d := NewMultiDispatcher(logger, tp)

	d.Register(core.ActionReply, ActionHandlerFunc(func(ctx context.Context, a core.Action) error {
		return errors.New("send failed")
	}))

	actions := []core.Action{
		{Type: core.ActionReply, Channel: "ch-1"},
	}

	err := d.Dispatch(context.Background(), actions)
	if err == nil {
		t.Fatal("expected error from failed handler")
	}
}
