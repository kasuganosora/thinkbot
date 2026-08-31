package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// testStage 是一个简单的测试 Stage。
type testStage struct {
	name      string
	fn        func(ctx context.Context, env *core.Envelope) (*core.Envelope, error)
	callCount int
}

func (s *testStage) Name() string { return s.name }
func (s *testStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	s.callCount++
	if s.fn != nil {
		return s.fn(ctx, env)
	}
	return env, nil
}

func testBusLogger() *zap.SugaredLogger {
	l, _ := zap.NewDevelopment()
	return l.Sugar()
}

func TestObservableStage_EmitsEnterExit(t *testing.T) {
	bus := outbound.NewMemoryEventBus(outbound.DefaultMemoryEventBusConfig(), testBusLogger())
	defer bus.Close()

	inner := &testStage{name: "test-stage"}
	obs := NewObservableStage(inner)

	if obs.Name() != "test-stage" {
		t.Errorf("expected name=test-stage, got %s", obs.Name())
	}

	// 设置 context
	emitter := outbound.NewEventEmitter(bus, "bot-1")
	ctx := context.Background()
	ctx = traceid.WithTraceID(ctx, "trace-001")
	ctx = outbound.ContextWithEmitter(ctx, emitter)

	sub := bus.Subscribe("trace-001")
	defer bus.Unsubscribe(sub)

	// 执行
	env := core.NewEnvelope(core.Message{ID: "m1", TraceID: "trace-001"})
	result, err := obs.Process(ctx, env)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != env {
		t.Error("expected same envelope back")
	}
	if inner.callCount != 1 {
		t.Errorf("expected inner called once, got %d", inner.callCount)
	}

	// 验证收到 stage.enter 和 stage.exit
	var events []outbound.Event
	for i := 0; i < 2; i++ {
		select {
		case e := <-sub.C():
			events = append(events, e)
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timeout: got %d events, expected 2", len(events))
		}
	}

	if events[0].Type != outbound.EventStageEnter {
		t.Errorf("event[0]: expected %s, got %s", outbound.EventStageEnter, events[0].Type)
	}
	if events[0].Stage != "test-stage" {
		t.Errorf("event[0]: expected stage=test-stage, got %s", events[0].Stage)
	}
	if events[1].Type != outbound.EventStageExit {
		t.Errorf("event[1]: expected %s, got %s", outbound.EventStageExit, events[1].Type)
	}
	if events[1].Stage != "test-stage" {
		t.Errorf("event[1]: expected stage=test-stage, got %s", events[1].Stage)
	}
	// duration_ms 应该 >= 0
	durationMs, ok := events[1].Data["duration_ms"].(int64)
	if !ok {
		t.Errorf("expected int64 duration_ms, got %T", events[1].Data["duration_ms"])
	}
	if durationMs < 0 {
		t.Errorf("expected non-negative duration, got %d", durationMs)
	}
}

func TestObservableStage_EmitsSkipEvent(t *testing.T) {
	bus := outbound.NewMemoryEventBus(outbound.DefaultMemoryEventBusConfig(), testBusLogger())
	defer bus.Close()

	inner := &testStage{
		name: "skip-stage",
		fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			return env, &core.SkipError{Reason: "not relevant"}
		},
	}
	obs := NewObservableStage(inner)

	emitter := outbound.NewEventEmitter(bus, "bot-1")
	ctx := context.Background()
	ctx = traceid.WithTraceID(ctx, "trace-002")
	ctx = outbound.ContextWithEmitter(ctx, emitter)

	sub := bus.Subscribe("trace-002")
	defer bus.Unsubscribe(sub)

	env := core.NewEnvelope(core.Message{ID: "m2", TraceID: "trace-002"})
	_, err := obs.Process(ctx, env)

	if !core.IsSkipError(err) {
		t.Errorf("expected SkipError, got %v", err)
	}

	// 应该收到 3 个事件：enter, exit, skip
	var events []outbound.Event
	for i := 0; i < 3; i++ {
		select {
		case e := <-sub.C():
			events = append(events, e)
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timeout: got %d events, expected 3", len(events))
		}
	}

	if events[0].Type != outbound.EventStageEnter {
		t.Errorf("event[0]: expected enter, got %s", events[0].Type)
	}
	if events[1].Type != outbound.EventStageExit {
		t.Errorf("event[1]: expected exit, got %s", events[1].Type)
	}
	if events[2].Type != outbound.EventStageSkip {
		t.Errorf("event[2]: expected skip, got %s", events[2].Type)
	}
}

func TestObservableStage_EmitsErrorInExit(t *testing.T) {
	bus := outbound.NewMemoryEventBus(outbound.DefaultMemoryEventBusConfig(), testBusLogger())
	defer bus.Close()

	inner := &testStage{
		name: "error-stage",
		fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			return env, errors.New("something went wrong")
		},
	}
	obs := NewObservableStage(inner)

	emitter := outbound.NewEventEmitter(bus, "bot-1")
	ctx := context.Background()
	ctx = traceid.WithTraceID(ctx, "trace-003")
	ctx = outbound.ContextWithEmitter(ctx, emitter)

	sub := bus.Subscribe("trace-003")
	defer bus.Unsubscribe(sub)

	env := core.NewEnvelope(core.Message{ID: "m3", TraceID: "trace-003"})
	_, _ = obs.Process(ctx, env)

	// 收 enter + exit
	var events []outbound.Event
	for i := 0; i < 2; i++ {
		select {
		case e := <-sub.C():
			events = append(events, e)
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timeout: got %d events, expected 2", len(events))
		}
	}

	// exit 事件应该包含 error
	exitEvent := events[1]
	if exitEvent.Data["error"] != "something went wrong" {
		t.Errorf("expected error in exit event, got %v", exitEvent.Data["error"])
	}
}

func TestObservableStage_NoEmitter_NoOp(t *testing.T) {
	inner := &testStage{name: "no-emitter"}
	obs := NewObservableStage(inner)

	// context 中没有 emitter，应该正常执行不 panic
	ctx := context.Background()
	env := core.NewEnvelope(core.Message{ID: "m4", TraceID: "trace-004"})

	result, err := obs.Process(ctx, env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != env {
		t.Error("expected same envelope")
	}
	if inner.callCount != 1 {
		t.Error("expected inner called")
	}
}

func TestObservableStage_Inner(t *testing.T) {
	inner := &testStage{name: "original"}
	obs := NewObservableStage(inner)

	if obs.Inner() != inner {
		t.Error("Inner() should return the original stage")
	}
}

func TestWrapWithObservability(t *testing.T) {
	stages := []core.StageInfo{
		{Stage: &testStage{name: "s1"}, Order: 1, Enabled: true},
		{Stage: &testStage{name: "s2"}, Order: 2, Enabled: true},
	}

	wrapped := WrapWithObservability(stages)

	if len(wrapped) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(wrapped))
	}

	for i, w := range wrapped {
		obs, ok := w.Stage.(*ObservableStage)
		if !ok {
			t.Errorf("stage[%d]: expected ObservableStage, got %T", i, w.Stage)
			continue
		}
		if obs.Name() != stages[i].Stage.Name() {
			t.Errorf("stage[%d]: name mismatch", i)
		}
	}

	// 不应重复包装
	doubleWrapped := WrapWithObservability(wrapped)
	for i, w := range doubleWrapped {
		obs := w.Stage.(*ObservableStage)
		if _, nested := obs.Inner().(*ObservableStage); nested {
			t.Errorf("stage[%d]: should not double-wrap", i)
		}
	}
}
