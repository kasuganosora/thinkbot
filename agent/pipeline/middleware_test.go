package pipeline

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Middleware 测试
// ============================================================================

func TestRecoveryMiddleware(t *testing.T) {
	panicker := &core.StageFunc{
		StageName: "panicker",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			panic("boom!")
		},
	}

	wrapped := RecoveryMiddleware()(panicker)

	env := core.NewEnvelope(core.Message{ID: "test"})
	result, err := wrapped.Process(context.Background(), env)
	if err == nil {
		t.Fatal("expected error from panic recovery")
	}
	if result == nil {
		t.Fatal("expected envelope to be preserved after recovery")
	}
	// 验证错误是 PipelineError
	var pe *core.PipelineError
	if ok := isPipelineError(err, &pe); !ok {
		t.Errorf("expected PipelineError, got %T: %v", err, err)
	}
}

func TestRecoveryMiddleware_NoPanic(t *testing.T) {
	normal := &core.StageFunc{
		StageName: "normal",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			env.Set("ok", true)
			return env, nil
		},
	}

	wrapped := RecoveryMiddleware()(normal)

	env := core.NewEnvelope(core.Message{ID: "test"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := result.Get("ok")
	if v != true {
		t.Error("expected ok=true")
	}
}

func TestTimeoutMiddleware(t *testing.T) {
	slow := &core.StageFunc{
		StageName: "slow",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			select {
			case <-time.After(5 * time.Second):
				return env, nil
			case <-ctx.Done():
				return env, ctx.Err()
			}
		},
	}

	wrapped := TimeoutMiddleware(50 * time.Millisecond)(slow)

	env := core.NewEnvelope(core.Message{ID: "test"})
	_, err := wrapped.Process(context.Background(), env)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestTimeoutMiddleware_Fast(t *testing.T) {
	fast := &core.StageFunc{
		StageName: "fast",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			env.Set("done", true)
			return env, nil
		},
	}

	wrapped := TimeoutMiddleware(5 * time.Second)(fast)

	env := core.NewEnvelope(core.Message{ID: "test"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := result.Get("done")
	if v != true {
		t.Error("expected done=true")
	}
}

func TestLoggingMiddleware(t *testing.T) {
	stage := &core.StageFunc{
		StageName: "logged",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			return env, nil
		},
	}

	logger := zap.NewNop().Sugar()
	wrapped := LoggingMiddleware(logger)(stage)

	env := core.NewEnvelope(core.Message{ID: "test"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Error("expected non-nil result")
	}
}

func TestWithMiddleware(t *testing.T) {
	var order []string

	mw1 := func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name(),
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				order = append(order, "mw1-before")
				result, err := next.Process(ctx, env)
				order = append(order, "mw1-after")
				return result, err
			},
		}
	}

	mw2 := func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name(),
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				order = append(order, "mw2-before")
				result, err := next.Process(ctx, env)
				order = append(order, "mw2-after")
				return result, err
			},
		}
	}

	inner := &core.StageFunc{
		StageName: "inner",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			order = append(order, "inner")
			return env, nil
		},
	}

	wrapped := WithMiddleware(inner, mw1, mw2)
	env := core.NewEnvelope(core.Message{ID: "test"})
	_, _ = wrapped.Process(context.Background(), env)

	expected := []string{"mw1-before", "mw2-before", "inner", "mw2-after", "mw1-after"}
	if len(order) != len(expected) {
		t.Fatalf("expected %d calls, got %d: %v", len(expected), len(order), order)
	}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("step %d: expected %s, got %s", i, v, order[i])
		}
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

func isPipelineError(err error, target **core.PipelineError) bool {
	if pe, ok := err.(*core.PipelineError); ok {
		*target = pe
		return true
	}
	return false
}
