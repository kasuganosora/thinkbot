package pipeline

import (
	"context"
	"errors"
	"testing"

	noop_metric "go.opentelemetry.io/otel/metric/noop"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func testLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

func testPipeline(stages ...core.StageInfo) *Pipeline {
	p, err := New(stages, noop_trace.NewTracerProvider(), noop_metric.NewMeterProvider(), testLogger())
	if err != nil {
		panic(err)
	}
	return p
}

func testEnvelope(id string) *core.Envelope {
	return core.NewEnvelope(core.Message{ID: id, Source: "test", Text: "hello"})
}

func TestPipeline_Execute_Empty(t *testing.T) {
	p := testPipeline()
	env := testEnvelope("msg-1")

	result, err := p.Execute(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil envelope")
	}
	if result.Message.ID != "msg-1" {
		t.Errorf("expected msg-1, got %s", result.Message.ID)
	}
}

func TestPipeline_Execute_StageChain(t *testing.T) {
	s1 := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "enrich",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				env.Set("step1", true)
				return env, nil
			},
		},
		Order:   10,
		Enabled: true,
	}
	s2 := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "transform",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				v, _ := env.Get("step1")
				if v != true {
					t.Error("step1 should be true")
				}
				env.Set("step2", true)
				env.AddAction(core.Action{Type: core.ActionReply, Payload: "ok"})
				return env, nil
			},
		},
		Order:   20,
		Enabled: true,
	}

	p := testPipeline(s2, s1) // 故意乱序，Pipeline 应按 Order 排序
	result, err := p.Execute(context.Background(), testEnvelope("msg-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	v1, _ := result.Get("step1")
	v2, _ := result.Get("step2")
	if v1 != true || v2 != true {
		t.Error("both steps should be executed")
	}
	if len(result.Actions()) != 1 {
		t.Errorf("expected 1 action, got %d", len(result.Actions()))
	}
}

func TestPipeline_Execute_DisabledStage(t *testing.T) {
	s := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "disabled",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				t.Error("disabled stage should not execute")
				return env, nil
			},
		},
		Order:   10,
		Enabled: false,
	}

	p := testPipeline(s)
	_, err := p.Execute(context.Background(), testEnvelope("msg-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPipeline_Execute_AbortError(t *testing.T) {
	s1 := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "aborter",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				return env, &core.AbortError{Reason: "rate limit"}
			},
		},
		Order:   10,
		Enabled: true,
	}
	s2 := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "should-not-run",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				t.Error("should not run after abort")
				return env, nil
			},
		},
		Order:   20,
		Enabled: true,
	}

	p := testPipeline(s1, s2)
	_, err := p.Execute(context.Background(), testEnvelope("msg-1"))
	if err == nil {
		t.Fatal("expected abort error")
	}
	if !core.IsAbortError(err) {
		t.Errorf("expected AbortError, got %T: %v", err, err)
	}
}

func TestPipeline_Execute_SkipError(t *testing.T) {
	s1 := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "skipper",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				return env, &core.SkipError{Reason: "not applicable"}
			},
		},
		Order:   10,
		Enabled: true,
	}
	s2 := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "after-skip",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				env.Set("reached", true)
				return env, nil
			},
		},
		Order:   20,
		Enabled: true,
	}

	p := testPipeline(s1, s2)
	result, err := p.Execute(context.Background(), testEnvelope("msg-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := result.Get("reached")
	if v != true {
		t.Error("stage after skip should be reached")
	}
}

func TestPipeline_Execute_NonFatalError(t *testing.T) {
	s1 := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "error-stage",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				return env, errors.New("non-fatal error")
			},
		},
		Order:   10,
		Enabled: true,
	}
	s2 := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "after-error",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				env.Set("reached", true)
				return env, nil
			},
		},
		Order:   20,
		Enabled: true,
	}

	p := testPipeline(s1, s2)
	result, err := p.Execute(context.Background(), testEnvelope("msg-1"))
	if err != nil {
		t.Fatalf("non-fatal error should not stop pipeline: %v", err)
	}
	v, _ := result.Get("reached")
	if v != true {
		t.Error("should continue after non-fatal error")
	}
}

func TestPipeline_Execute_PanicRecovery(t *testing.T) {
	s := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "panicker",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				panic("unexpected!")
			},
		},
		Order:   10,
		Enabled: true,
	}

	p := testPipeline(s)
	result, err := p.Execute(context.Background(), testEnvelope("msg-1"))
	// panic 被恢复为非致命错误
	if err != nil && core.IsAbortError(err) {
		t.Error("panic should be recovered as non-fatal error")
	}
	if result == nil {
		t.Error("envelope should be preserved after panic recovery")
	}
}

func TestPipeline_Execute_EnvelopeAborted(t *testing.T) {
	s1 := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "aborter",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				env.Abort(errors.New("manual abort"))
				return env, nil
			},
		},
		Order:   10,
		Enabled: true,
	}
	s2 := core.StageInfo{
		Stage: &core.StageFunc{
			StageName: "should-not-run",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				t.Error("should not run after envelope abort")
				return env, nil
			},
		},
		Order:   20,
		Enabled: true,
	}

	p := testPipeline(s1, s2)
	result, err := p.Execute(context.Background(), testEnvelope("msg-1"))
	if err != nil {
		t.Fatalf("envelope abort should not return error: %v", err)
	}
	if !result.Aborted() {
		t.Error("envelope should be aborted")
	}
}

func TestPipeline_StageNames(t *testing.T) {
	p := testPipeline(
		core.StageInfo{Stage: &core.StageFunc{StageName: "b"}, Order: 20, Enabled: true},
		core.StageInfo{Stage: &core.StageFunc{StageName: "a"}, Order: 10, Enabled: true},
		core.StageInfo{Stage: &core.StageFunc{StageName: "c"}, Order: 30, Enabled: false},
	)

	names := p.StageNames()
	if len(names) != 2 {
		t.Fatalf("expected 2 enabled stages, got %d", len(names))
	}
	if names[0] != "a" || names[1] != "b" {
		t.Errorf("expected [a, b], got %v", names)
	}
}
