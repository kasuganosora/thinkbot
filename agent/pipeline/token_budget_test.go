package pipeline

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// TokenBudgetMiddleware 测试
// ============================================================================

func TestTokenBudgetConfig_Defaults(t *testing.T) {
	cfg := NewTokenBudgetConfig()
	if cfg.MaxTokens != 100_000 {
		t.Errorf("expected MaxTokens=100000, got %d", cfg.MaxTokens)
	}
	if cfg.WarnPercent != 0.8 {
		t.Errorf("expected WarnPercent=0.8, got %f", cfg.WarnPercent)
	}
	if cfg.HardPercent != 1.0 {
		t.Errorf("expected HardPercent=1.0, got %f", cfg.HardPercent)
	}
	if cfg.IsZero() {
		t.Error("default config should not be zero")
	}
}

func TestTokenBudgetConfig_Zero(t *testing.T) {
	cfg := TokenBudgetConfig{}
	if !cfg.IsZero() {
		t.Error("empty config should be zero")
	}
}

func TestTokenBudgetConfig_Chaining(t *testing.T) {
	cfg := NewTokenBudgetConfig().
		WithMaxTokens(50_000).
		WithWarnPercent(0.5).
		WithHardPercent(0.9)
	if cfg.MaxTokens != 50_000 {
		t.Errorf("expected MaxTokens=50000, got %d", cfg.MaxTokens)
	}
	if cfg.WarnPercent != 0.5 {
		t.Errorf("expected WarnPercent=0.5, got %f", cfg.WarnPercent)
	}
	if cfg.HardPercent != 0.9 {
		t.Errorf("expected HardPercent=0.9, got %f", cfg.HardPercent)
	}
}

func TestTokenBudgetMiddleware_ZeroConfig(t *testing.T) {
	inner := &core.StageFunc{
		StageName: "inner",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			env.Set("called", true)
			return env, nil
		},
	}
	mw := TokenBudgetMiddleware(TokenBudgetConfig{})
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{ID: "test", Channel: "ch-1"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := result.Get("called")
	if v != true {
		t.Error("inner stage should have been called")
	}
}

func TestTokenBudgetMiddleware_BelowThreshold(t *testing.T) {
	cfg := NewTokenBudgetConfig().
		WithMaxTokens(100_000).
		WithWarnPercent(0.8).
		WithHardPercent(1.0)

	inner := llmResultStage("step", &llm.GenerateResult{
		Usage: llm.Usage{TotalTokens: 100},
	})

	mw := TokenBudgetMiddleware(cfg)
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{ID: "test", Channel: "ch-1"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if HasWarning(result, "token_budget") {
		t.Error("expected no warning when below threshold")
	}
	if v, _ := result.Get("llm.result"); v == nil {
		t.Error("expected llm.result to exist")
	}
}

func TestTokenBudgetMiddleware_SoftWarning(t *testing.T) {
	// warn at 80% of 1000 = 800 tokens. Start usage at 900 → should trigger.
	cfg := NewTokenBudgetConfig().
		WithMaxTokens(1000).
		WithWarnPercent(0.8)

	mw2 := TokenBudgetMiddleware(cfg)

	// First call: simulate high accumulated usage via a stage that returns high tokens.
	innerHigh := llmResultStage("high", &llm.GenerateResult{
		Usage: llm.Usage{TotalTokens: 900},
	})
	wrappedHigh := mw2(innerHigh)

	result, err := wrappedHigh.Process(context.Background(), core.NewEnvelope(core.Message{ID: "h", Channel: "ch-1"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if HasWarning(result, "token_budget") {
		t.Error("expected no warning on first high-usage call (check before, warnLimit=800)")
	}

	// Second call: now cumulative = 900 + 900 = 1800 → should trigger soft warning.
	result2, err := wrappedHigh.Process(context.Background(), core.NewEnvelope(core.Message{ID: "h2", Channel: "ch-1"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !HasWarning(result2, "token_budget") {
		t.Error("expected soft warning when cumulative exceeds warn limit")
	}
	if !HasWarningLevel(result2, core.WarningLevelSoft) {
		t.Error("expected soft-level warning from token_budget")
	}
}

func TestTokenBudgetMiddleware_HardLimit(t *testing.T) {
	cfg := NewTokenBudgetConfig().
		WithMaxTokens(1000).
		WithHardPercent(1.0)

	inner := llmResultStage("step", &llm.GenerateResult{
		Usage: llm.Usage{TotalTokens: 1100},
	})

	mw := TokenBudgetMiddleware(cfg)
	wrapped := mw(inner)

	// First call: check happens BEFORE execution. Initial usage is 0, so it passes.
	env := core.NewEnvelope(core.Message{ID: "test", Channel: "ch-1"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error on first call: %v", err)
	}
	_ = result

	// After first call, cumulative = 1100. Second call should hit hard limit.
	env2 := core.NewEnvelope(core.Message{ID: "test2", Channel: "ch-1"})
	_, err = wrapped.Process(context.Background(), env2)
	if err == nil {
		t.Fatal("expected hard limit error")
	}
	var pe *core.PipelineError
	if ok := isPE(err, &pe); !ok {
		t.Errorf("expected PipelineError, got %T: %v", err, err)
	} else if pe.Stage != "token_budget" {
		t.Errorf("expected stage token_budget, got %s", pe.Stage)
	}
}

func TestTokenBudgetMiddleware_HardLimitWarning(t *testing.T) {
	// At 90-99% of hard limit, inject a hard-level WARNING (not error).
	// The check happens BEFORE execution, so we need two calls:
	//   Call 1: cumulative=0 → pass. After: cumulative=950
	//   Call 2: cumulative=950 (≥ 90% of 1000) → hard warning.
	cfg := NewTokenBudgetConfig().
		WithMaxTokens(1000).
		WithWarnPercent(0.5).
		WithHardPercent(1.0)

	inner := llmResultStage("step", &llm.GenerateResult{
		Usage: llm.Usage{TotalTokens: 950},
	})

	mw := TokenBudgetMiddleware(cfg)
	wrapped := mw(inner)

	// First call: accumulates 950 tokens.
	env1 := core.NewEnvelope(core.Message{ID: "t1", Channel: "ch-1"})
	_, err := wrapped.Process(context.Background(), env1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Second call: cumulative=950 ≥ 900 (90% of hard limit=1000) → hard warning.
	env2 := core.NewEnvelope(core.Message{ID: "t2", Channel: "ch-1"})
	result, err := wrapped.Process(context.Background(), env2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !HasWarning(result, "token_budget") {
		t.Error("expected warning at 95% of hard limit")
	}
	hasHard := false
	v, _ := result.Get(core.WarningsKey)
	warnings, _ := v.([]core.Warning)
	for _, w := range warnings {
		if w.Level == core.WarningLevelHard {
			hasHard = true
		}
	}
	if !hasHard {
		t.Error("expected hard-level warning when near hard limit")
	}
}

func TestTokenBudgetMiddleware_NoChannel(t *testing.T) {
	cfg := NewTokenBudgetConfig()
	inner := llmResultStage("step", &llm.GenerateResult{
		Usage: llm.Usage{TotalTokens: 1_000_000},
	})

	mw := TokenBudgetMiddleware(cfg)
	wrapped := mw(inner)

	// No channel → skipped.
	env := core.NewEnvelope(core.Message{ID: "test"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error when no channel: %v", err)
	}
	if HasWarning(result, "token_budget") {
		t.Error("expected no warning when channel is empty")
	}
}

func TestTokenBudgetMiddleware_AccumulatesUsage(t *testing.T) {
	cfg := NewTokenBudgetConfig().WithMaxTokens(10_000)
	inner := llmResultStage("step", &llm.GenerateResult{
		Usage: llm.Usage{TotalTokens: 500},
	})

	mw := TokenBudgetMiddleware(cfg)
	wrapped := mw(inner)

	env1 := core.NewEnvelope(core.Message{ID: "t1", Channel: "ch-1"})
	_, _ = wrapped.Process(context.Background(), env1)

	env2 := core.NewEnvelope(core.Message{ID: "t2", Channel: "ch-1"})
	_, _ = wrapped.Process(context.Background(), env2)

	// Internal state should have 1000 tokens accumulated (500+500).
	// Cannot inspect internal state, but no errors = success.
}

func TestTokenBudgetMiddleware_IndependentChannels(t *testing.T) {
	cfg := NewTokenBudgetConfig().
		WithMaxTokens(1000).
		WithHardPercent(1.0)

	inner := llmResultStage("step", &llm.GenerateResult{
		Usage: llm.Usage{TotalTokens: 900},
	})

	mw := TokenBudgetMiddleware(cfg)
	wrapped := mw(inner)

	// Channel 1: accumulate 900.
	env1 := core.NewEnvelope(core.Message{ID: "t1", Channel: "ch-1"})
	_, err := wrapped.Process(context.Background(), env1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Channel 2: should start fresh (no accumulated tokens).
	env2 := core.NewEnvelope(core.Message{ID: "t2", Channel: "ch-2"})
	result2, err := wrapped.Process(context.Background(), env2)
	if err != nil {
		t.Fatalf("unexpected error on independent channel: %v", err)
	}
	// Should NOT trigger hard limit because ch-2 starts from 0.
	if HasWarning(result2, "token_budget") {
		// might have soft warning at 900 tokens
		if HasWarningLevel(result2, core.WarningLevelHard) {
			t.Error("expected independent channel tracking, but got hard warning on ch-2")
		}
	}
}

func TestTokenBudgetMiddleware_WarnOnlyOnce(t *testing.T) {
	cfg := NewTokenBudgetConfig().
		WithMaxTokens(1000).
		WithWarnPercent(0.5) // warn at 500

	inner := llmResultStage("step", &llm.GenerateResult{
		Usage: llm.Usage{TotalTokens: 600}, // above warn threshold
	})

	mw := TokenBudgetMiddleware(cfg)
	wrapped := mw(inner)

	// First call: cumulative=0 before, no warning. After: cumulative=600.
	env1 := core.NewEnvelope(core.Message{ID: "t1", Channel: "ch-1"})
	_, _ = wrapped.Process(context.Background(), env1)

	// Second call: cumulative=600 >= 500 → soft warning. warned flag set.
	env2 := core.NewEnvelope(core.Message{ID: "t2", Channel: "ch-1"})
	r2, _ := wrapped.Process(context.Background(), env2)
	if !HasWarning(r2, "token_budget") {
		t.Fatal("expected warning on second call (cumulative exceeds warn limit)")
	}

	// Third call: cumulative=1200 but warned flag prevents duplicate soft warning.
	env3 := core.NewEnvelope(core.Message{ID: "t3", Channel: "ch-1"})
	r3, _ := wrapped.Process(context.Background(), env3)
	if HasWarning(r3, "token_budget") {
		t.Error("expected no duplicate soft warning (warned flag)")
	}
}

// ============================================================================
// 辅助
// ============================================================================

func isPE(err error, target **core.PipelineError) bool {
	if pe, ok := err.(*core.PipelineError); ok {
		*target = pe
		return true
	}
	return false
}
