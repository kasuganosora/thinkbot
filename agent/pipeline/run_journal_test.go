package pipeline

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// RunJournalRecorder 测试
// ============================================================================

func TestRunJournalConfig_Defaults(t *testing.T) {
	cfg := DefaultRunJournalConfig()
	if cfg.FlushThreshold != 20 {
		t.Errorf("expected FlushThreshold=20, got %d", cfg.FlushThreshold)
	}
	if cfg.Caller != "lead_agent" {
		t.Errorf("expected Caller=lead_agent, got %s", cfg.Caller)
	}
}

func TestRunJournalRecorder_NoOp_WhenNoDB(t *testing.T) {
	// nil DB → NoOp mode: all methods succeed without error.
	r := NewRunJournalRecorder(nil, DefaultRunJournalConfig())

	// RecordUsage should not panic.
	r.RecordUsage(context.Background(), llm.UsageMetric{
		BotID: "bot-1",
		Model: "test-model",
		Usage: llm.Usage{TotalTokens: 100},
	})

	// Middleware should be a pass-through.
	mw := r.Middleware()
	inner := &core.StageFunc{
		StageName: "inner",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			env.Set("called", true)
			return env, nil
		},
	}
	wrapped := mw(inner)
	env := core.NewEnvelope(core.Message{ID: "test"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := result.Get("called")
	if v != true {
		t.Error("inner stage should be called in NoOp mode")
	}

	// Flush / Shutdown should not error.
	if err := r.Flush(context.Background()); err != nil {
		t.Errorf("unexpected flush error: %v", err)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Errorf("unexpected shutdown error: %v", err)
	}
}

func TestRunJournalRecorder_RecordUsage_Buffering(t *testing.T) {
	r := NewRunJournalRecorder(nil, RunJournalConfig{
		FlushThreshold: 20,
		Caller:         "test_caller",
		Feature:        "test_feature",
	})

	// Record multiple usages below threshold → no flush triggered.
	for i := 0; i < 5; i++ {
		r.RecordUsage(context.Background(), llm.UsageMetric{
			BotID:     "bot-1",
			Model:     "test",
			Feature:   "custom",
			Usage:     llm.Usage{TotalTokens: i * 100},
			ToolCalls: 1,
			Steps:     2,
		})
	}
	// With nil DB, these are just no-ops. Verify no panics.
}

func TestRunJournalRecorder_ContextExtraction(t *testing.T) {
	// Test the extractContextMeta method with a context that has journal metadata.
	r := NewRunJournalRecorder(nil, DefaultRunJournalConfig())

	ctx := context.WithValue(context.Background(), journalCtxKey{}, journalMeta{
		TraceID:   "trace-123",
		MessageID: "msg-456",
		Channel:   "ch-789",
		UserID:    "user-001",
	})

	traceID, msgID, channel, userID := r.extractContextMeta(ctx)
	if traceID != "trace-123" {
		t.Errorf("expected trace-123, got %s", traceID)
	}
	if msgID != "msg-456" {
		t.Errorf("expected msg-456, got %s", msgID)
	}
	if channel != "ch-789" {
		t.Errorf("expected ch-789, got %s", channel)
	}
	if userID != "user-001" {
		t.Errorf("expected user-001, got %s", userID)
	}
}

func TestRunJournalRecorder_ContextExtraction_Empty(t *testing.T) {
	r := NewRunJournalRecorder(nil, DefaultRunJournalConfig())
	traceID, msgID, channel, userID := r.extractContextMeta(context.Background())
	if traceID != "" || msgID != "" || channel != "" || userID != "" {
		t.Errorf("expected all empty for clean context, got %q/%q/%q/%q",
			traceID, msgID, channel, userID)
	}
}

func TestRunJournalRecorder_Middleware_InjectsContext(t *testing.T) {
	// When db is nil, middleware returns identity (no context injection).
	// Context injection is tested separately via extractContextMeta.
	// This test verifies the middleware pass-through behavior.
	r := NewRunJournalRecorder(nil, DefaultRunJournalConfig())

	inner := &core.StageFunc{
		StageName: "capture",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			// Verify no journal metadata (nil db = pass-through).
			if meta, ok := ctx.Value(journalCtxKey{}).(journalMeta); ok {
				t.Errorf("expected no journal metadata with nil db, got meta=%v", meta)
			}
			return env, nil
		},
	}

	mw := r.Middleware()
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{
		ID:      "msg-001",
		TraceID: "trace-abc",
		Channel: "telegram-123",
		UserID:  "user-x",
		BotID:   "bot-1",
	})
	_, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunJournalRecorder_Middleware_PassThrough_NoDB(t *testing.T) {
	r := NewRunJournalRecorder(nil, DefaultRunJournalConfig())
	mw := r.Middleware()

	// Middleware with nil db should be identity (returns next unchanged).
	// Use a type assertion: wrapped.Name() should match.
	inner := &core.StageFunc{
		StageName: "original",
		Fn:        func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) { return env, nil },
	}
	wrapped := mw(inner)
	if wrapped.Name() != "original" {
		t.Errorf("expected pass-through for nil db, got name %s", wrapped.Name())
	}
}

func TestRunJournalRecorder_FeatureFallback(t *testing.T) {
	// cfg.Feature is used when metric.Feature is empty.
	r := NewRunJournalRecorder(nil, RunJournalConfig{
		Caller:  "test",
		Feature: "default_feature",
	})

	// Should not panic.
	r.RecordUsage(context.Background(), llm.UsageMetric{
		BotID: "bot-1",
		Model: "m",
		Usage: llm.Usage{TotalTokens: 10},
		// Feature is empty → falls back to cfg.Feature = "default_feature"
	})
}

func TestRunJournalRecorder_Shutdown_Idempotent(t *testing.T) {
	r := NewRunJournalRecorder(nil, DefaultRunJournalConfig())

	// Multiple Shutdown calls should not panic (once.Do).
	if err := r.Shutdown(context.Background()); err != nil {
		t.Errorf("first shutdown: %v", err)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Errorf("second shutdown: %v", err)
	}
}

func TestRunJournalRecorder_Flush_EmptyBuffer(t *testing.T) {
	r := NewRunJournalRecorder(nil, DefaultRunJournalConfig())
	// Flush with empty buffer (no records) should not error.
	if err := r.Flush(context.Background()); err != nil {
		t.Errorf("unexpected flush error with empty buffer: %v", err)
	}
}

func TestRunJournalMiddleware_NoDB(t *testing.T) {
	// RunJournalMiddleware with nil db → identity.
	mw := RunJournalMiddleware(nil, DefaultRunJournalConfig())
	inner := &core.StageFunc{
		StageName: "inner",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			env.Set("called", true)
			return env, nil
		},
	}
	wrapped := mw(inner)
	env := core.NewEnvelope(core.Message{ID: "test"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, _ := result.Get("called")
	if v != true {
		t.Error("inner stage should be called")
	}
}

func TestRunJournalMiddleware_WithDB_NoOp(t *testing.T) {
	// Test with nil db that it extracts llm.result and creates a record.
	mw := RunJournalMiddleware(nil, RunJournalConfig{
		FlushThreshold: 5,
		Caller:         "test",
	})
	inner := llmResultStage("step", &llm.GenerateResult{
		Usage:     llm.Usage{InputTokens: 50, OutputTokens: 30, TotalTokens: 80},
		ToolCalls: []llm.ToolCall{{ToolCallID: "tc1", ToolName: "search", Input: "x"}},
		Steps:     []llm.StepResult{{ToolCalls: []llm.ToolCall{{ToolCallID: "tc1", ToolName: "search"}}}},
	})
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{
		ID:      "msg-1",
		TraceID: "trace-1",
		Channel: "ch-1",
		UserID:  "u-1",
		BotID:   "b-1",
	})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, _ := result.Get("llm.result"); v == nil {
		t.Error("expected llm.result to exist")
	}
}

func TestRunJournalMiddleware_ErrorStatus(t *testing.T) {
	mw := RunJournalMiddleware(nil, RunJournalConfig{
		FlushThreshold: 1,
		Caller:         "test",
	})
	inner := llmResultStage("step", &llm.GenerateResult{
		Usage:        llm.Usage{TotalTokens: 10},
		FinishReason: llm.FinishReasonError,
	})
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{ID: "test", TraceID: "t1", BotID: "b1"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result
	// With nil DB, the async write is skipped. Just verify no panic.
}
