package pipeline

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// LoopDetectionMiddleware 测试
// ============================================================================

func TestLoopDetectionConfig_Defaults(t *testing.T) {
	cfg := NewLoopDetectionConfig()
	if cfg.WarnThreshold != 3 {
		t.Errorf("expected WarnThreshold=3, got %d", cfg.WarnThreshold)
	}
	if cfg.HardLimit != 5 {
		t.Errorf("expected HardLimit=5, got %d", cfg.HardLimit)
	}
	if cfg.WindowSize != 20 {
		t.Errorf("expected WindowSize=20, got %d", cfg.WindowSize)
	}
	if cfg.IsZero() {
		t.Error("default config should not be zero")
	}
}

func TestLoopDetectionConfig_Zero(t *testing.T) {
	cfg := LoopDetectionConfig{}
	if !cfg.IsZero() {
		t.Error("empty config should be zero")
	}
}

func TestLoopDetectionConfig_Chaining(t *testing.T) {
	cfg := NewLoopDetectionConfig().
		WithWarnThreshold(5).
		WithHardLimit(10).
		WithWindowSize(30)
	if cfg.WarnThreshold != 5 {
		t.Errorf("expected WarnThreshold=5, got %d", cfg.WarnThreshold)
	}
	if cfg.HardLimit != 10 {
		t.Errorf("expected HardLimit=10, got %d", cfg.HardLimit)
	}
	if cfg.WindowSize != 30 {
		t.Errorf("expected WindowSize=30, got %d", cfg.WindowSize)
	}
}

func TestLoopDetectionMiddleware_ZeroConfig(t *testing.T) {
	// Zero config → pass-through (returns next unchanged)
	inner := &core.StageFunc{
		StageName: "inner",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			env.Set("called", true)
			return env, nil
		},
	}
	mw := LoopDetectionMiddleware(LoopDetectionConfig{})
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{ID: "test"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := result.Get("called")
	if v != true {
		t.Error("inner stage should have been called")
	}
}

func TestLoopDetectionMiddleware_NoToolCalls(t *testing.T) {
	// LLM result with no tool calls → no loop detection triggered.
	inner := llmResultStage("llm_result_no_tools", &llm.GenerateResult{
		Text:  "Hello!",
		Steps: []llm.StepResult{{Text: "Hello!"}},
	})

	mw := LoopDetectionMiddleware(NewLoopDetectionConfig())
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{ID: "test", Channel: "ch-1"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := result.Get("llm.result"); v == nil {
		t.Error("expected llm.result to exist")
	}
	// No warnings should be queued (no tool calls = no digest).
	if HasWarning(result, "loop_detection") {
		t.Error("expected no loop warning when no tool calls")
	}
}

func TestLoopDetectionMiddleware_FirstToolCall(t *testing.T) {
	// First occurrence of a tool call pattern → count=1, below thresholds.
	inner := llmResultStage("step1", &llm.GenerateResult{
		Steps: []llm.StepResult{{
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "tc1", ToolName: "search", Input: "query"},
			},
		}},
	})

	mw := LoopDetectionMiddleware(NewLoopDetectionConfig().WithWarnThreshold(3).WithHardLimit(5))
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{ID: "test", Channel: "ch-1"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if HasWarning(result, "loop_detection") {
		t.Error("expected no warning on first tool call")
	}
}

func TestLoopDetectionMiddleware_SoftWarning(t *testing.T) {
	cfg := NewLoopDetectionConfig().WithWarnThreshold(2).WithHardLimit(5)
	inner := llmResultStage("step", &llm.GenerateResult{
		Steps: []llm.StepResult{{
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "tc1", ToolName: "search", Input: "query"},
			},
		}},
	})

	mw := LoopDetectionMiddleware(cfg)
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{ID: "test", Channel: "ch-1"})

	// First call: count=1, no warning.
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if HasWarning(result, "loop_detection") {
		t.Error("expected no warning on first call")
	}

	// Second call: count=2, soft warning triggered.
	env2 := core.NewEnvelope(core.Message{ID: "test2", Channel: "ch-1"})
	result2, err := wrapped.Process(context.Background(), env2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !HasWarning(result2, "loop_detection") {
		t.Error("expected soft loop warning on second occurrence")
	}
	if !HasWarningLevel(result2, core.WarningLevelSoft) {
		t.Error("expected soft-level warning")
	}
}

func TestLoopDetectionMiddleware_HardWarning(t *testing.T) {
	cfg := NewLoopDetectionConfig().WithWarnThreshold(1).WithHardLimit(2)
	inner := llmResultStage("step", &llm.GenerateResult{
		Steps: []llm.StepResult{{
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "tc1", ToolName: "search", Input: "x"},
			},
		}},
	})

	mw := LoopDetectionMiddleware(cfg)
	wrapped := mw(inner)

	// 1st: warn
	env1 := core.NewEnvelope(core.Message{ID: "t1", Channel: "ch-1"})
	r1, err := wrapped.Process(context.Background(), env1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !HasWarningLevel(r1, core.WarningLevelSoft) {
		t.Error("expected soft warning at count=1 (warn threshold)")
	}

	// 2nd: hard
	env2 := core.NewEnvelope(core.Message{ID: "t2", Channel: "ch-1"})
	r2, err := wrapped.Process(context.Background(), env2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !HasWarningLevel(r2, core.WarningLevelHard) {
		t.Error("expected hard warning at count=2 (hard limit)")
	}

	// 3rd: already hardWarned, should be a no-op (no new warnings)
	env3 := core.NewEnvelope(core.Message{ID: "t3", Channel: "ch-1"})
	r3, err := wrapped.Process(context.Background(), env3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// After hard-warned, the middleware skips detection entirely.
	// So no new warnings should be queued.
	ClearWarnings(r3) // just ensure no panic
}

func TestLoopDetectionMiddleware_NoChannel(t *testing.T) {
	inner := llmResultStage("step", &llm.GenerateResult{
		Steps: []llm.StepResult{{
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "tc1", ToolName: "search", Input: "x"},
			},
		}},
	})

	mw := LoopDetectionMiddleware(NewLoopDetectionConfig().WithWarnThreshold(1).WithHardLimit(2))
	wrapped := mw(inner)

	// No channel → skipped by middleware.
	env := core.NewEnvelope(core.Message{ID: "test"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if HasWarning(result, "loop_detection") {
		t.Error("expected no warning when channel is empty")
	}
}

func TestLoopDetectionMiddleware_DifferentPatterns(t *testing.T) {
	// Different tool call patterns produce different digests → no loop.
	cfg := NewLoopDetectionConfig().WithWarnThreshold(2).WithHardLimit(5)

	inner := llmResultStage("step", &llm.GenerateResult{
		Steps: []llm.StepResult{{
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "tc1", ToolName: "search", Input: "query_a"},
			},
		}},
	})
	mw := LoopDetectionMiddleware(cfg)
	wrapped := mw(inner)

	// Pattern 1: search("query_a")
	env1 := core.NewEnvelope(core.Message{ID: "t1", Channel: "ch-1"})
	_, _ = wrapped.Process(context.Background(), env1)

	// Pattern 2: search("query_b") — different digest
	inner2 := llmResultStage("step", &llm.GenerateResult{
		Steps: []llm.StepResult{{
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "tc1", ToolName: "search", Input: "query_b"},
			},
		}},
	})
	wrapped2 := mw(inner2)

	env2 := core.NewEnvelope(core.Message{ID: "t2", Channel: "ch-1"})
	result2, err := wrapped2.Process(context.Background(), env2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if HasWarning(result2, "loop_detection") {
		t.Error("expected no loop warning for different tool patterns")
	}
}

func TestLoopDetectionMiddleware_HardWarnedPreventsFurtherDetection(t *testing.T) {
	cfg := NewLoopDetectionConfig().WithWarnThreshold(1).WithHardLimit(2)
	inner := llmResultStage("step", &llm.GenerateResult{
		Steps: []llm.StepResult{{
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "tc1", ToolName: "search", Input: "x"},
			},
		}},
	})

	mw := LoopDetectionMiddleware(cfg)
	wrapped := mw(inner)

	// Trigger hard state quickly.
	env1 := core.NewEnvelope(core.Message{ID: "t1", Channel: "ch-1"})
	_, _ = wrapped.Process(context.Background(), env1) // soft
	env2 := core.NewEnvelope(core.Message{ID: "t2", Channel: "ch-1"})
	r2, _ := wrapped.Process(context.Background(), env2) // hard
	if !HasWarningLevel(r2, core.WarningLevelHard) {
		t.Fatal("expected hard warning at count=2")
	}

	// After hard, detection is skipped entirely — llm.result won't be checked.
	env3 := core.NewEnvelope(core.Message{ID: "t3", Channel: "ch-1"})
	_, err := wrapped.Process(context.Background(), env3)
	if err != nil {
		t.Fatalf("unexpected error after hard warn: %v", err)
	}
}

func TestToolCallsDigest(t *testing.T) {
	// Same tool calls → same digest.
	r1 := &llm.GenerateResult{
		Steps: []llm.StepResult{{
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "tc1", ToolName: "search", Input: "hello"},
			},
		}},
	}
	r2 := &llm.GenerateResult{
		Steps: []llm.StepResult{{
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "tc2", ToolName: "search", Input: "hello"},
			},
		}},
	}
	if toolCallsDigest(r1) != toolCallsDigest(r2) {
		t.Error("same (name, args) should produce same digest")
	}

	// Different args → different digest.
	r3 := &llm.GenerateResult{
		Steps: []llm.StepResult{{
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "tc1", ToolName: "search", Input: "world"},
			},
		}},
	}
	if toolCallsDigest(r1) == toolCallsDigest(r3) {
		t.Error("different args should produce different digest")
	}

	// No steps → empty digest.
	r4 := &llm.GenerateResult{}
	if toolCallsDigest(r4) != "" {
		t.Error("expected empty digest for no steps")
	}

	// Nil result → empty digest.
	if toolCallsDigest(nil) != "" {
		t.Error("expected empty digest for nil result")
	}
}

// ============================================================================
// loopWindow 测试
// ============================================================================

func TestLoopWindow_Push(t *testing.T) {
	w := newLoopWindow(3)

	c1 := w.push("hash_a")
	if c1 != 1 {
		t.Errorf("expected count=1, got %d", c1)
	}

	c2 := w.push("hash_a")
	if c2 != 2 {
		t.Errorf("expected count=2, got %d", c2)
	}

	c3 := w.push("hash_b")
	if c3 != 1 {
		t.Errorf("expected count=1 for new hash, got %d", c3)
	}

	// Verify hash_a count is still 2.
	if w.freqCount["hash_a"] != 2 {
		t.Errorf("expected hash_a freq=2, got %d", w.freqCount["hash_a"])
	}
}

func TestLoopWindow_PushOverflow(t *testing.T) {
	w := newLoopWindow(2)

	w.push("hash_a") // window: [a]
	w.push("hash_b") // window: [a, b]
	if w.freqCount["hash_a"] != 1 || w.freqCount["hash_b"] != 1 {
		t.Error("expected both hashes with freq=1")
	}

	w.push("hash_c") // window: [b, c], a evicted
	if _, ok := w.freqCount["hash_a"]; ok {
		t.Error("hash_a should be evicted")
	}
	if w.freqCount["hash_b"] != 1 {
		t.Errorf("expected hash_b freq=1, got %d", w.freqCount["hash_b"])
	}
	if w.freqCount["hash_c"] != 1 {
		t.Errorf("expected hash_c freq=1, got %d", w.freqCount["hash_c"])
	}
}

// ============================================================================
// 测试辅助
// ============================================================================

// llmResultStage returns a StageFunc that stores a GenerateResult into the Envelope.
func llmResultStage(name string, result *llm.GenerateResult) core.Stage {
	return &core.StageFunc{
		StageName: name,
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			env.Set("llm.result", result)
			return env, nil
		},
	}
}

// HasWarning checks if any warning with the given source is queued in the envelope.
func HasWarning(env *core.Envelope, source string) bool {
	v, ok := env.Get(core.WarningsKey)
	if !ok {
		return false
	}
	warnings, ok := v.([]core.Warning)
	if !ok {
		return false
	}
	for _, w := range warnings {
		if w.Source == source {
			return true
		}
	}
	return false
}

// HasWarningLevel checks if any warning with the given level is queued.
func HasWarningLevel(env *core.Envelope, level string) bool {
	v, ok := env.Get(core.WarningsKey)
	if !ok {
		return false
	}
	warnings, ok := v.([]core.Warning)
	if !ok {
		return false
	}
	for _, w := range warnings {
		if w.Level == level {
			return true
		}
	}
	return false
}

// ClearWarnings clears all warnings in the envelope for test isolation.
func ClearWarnings(env *core.Envelope) {
	core.ClearWarnings(env)
}
