package core

import (
	"strings"
	"testing"
)

// ============================================================================
// Warning 测试
// ============================================================================

func TestQueueWarning_Single(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	QueueWarning(env, Warning{
		Source:  "token_budget",
		Level:   WarningLevelSoft,
		Message: "budget at 80%",
	})

	v, ok := env.Get(WarningsKey)
	if !ok {
		t.Fatal("expected warnings key to exist")
	}
	warnings, ok := v.([]Warning)
	if !ok {
		t.Fatalf("expected []Warning, got %T", v)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
	if warnings[0].Source != "token_budget" {
		t.Errorf("expected source token_budget, got %s", warnings[0].Source)
	}
}

func TestQueueWarning_Multiple(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	QueueWarning(env, Warning{Source: "s1", Level: WarningLevelSoft, Message: "m1"})
	QueueWarning(env, Warning{Source: "s2", Level: WarningLevelHard, Message: "m2"})
	QueueWarning(env, Warning{Source: "s3", Level: WarningLevelSoft, Message: "m3"})

	v, _ := env.Get(WarningsKey)
	warnings := v.([]Warning)
	if len(warnings) != 3 {
		t.Fatalf("expected 3 warnings, got %d", len(warnings))
	}
}

func TestMergeWarnings_NoWarnings(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	base := "You are a helpful assistant."

	result := MergeWarnings(env, base)
	if result != base {
		t.Errorf("expected unchanged prompt when no warnings, got: %s", result)
	}
}

func TestMergeWarnings_SoftWarning(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	QueueWarning(env, Warning{
		Source:  "token_budget",
		Level:   WarningLevelSoft,
		Message: "budget at 80%",
	})

	result := MergeWarnings(env, "base prompt")
	if !strings.Contains(result, "base prompt") {
		t.Errorf("expected base prompt to be preserved")
	}
	if !strings.Contains(result, "[SYSTEM WARNING]") {
		t.Errorf("expected soft warning marker, got: %s", result)
	}
	if !strings.Contains(result, "[token_budget]") {
		t.Errorf("expected source in warning, got: %s", result)
	}
	if !strings.Contains(result, "budget at 80%") {
		t.Errorf("expected warning message, got: %s", result)
	}

	// Soft warnings should be consumed after MergeWarnings.
	v, ok := env.Get(WarningsKey)
	if !ok || v != nil {
		t.Errorf("expected nil warnings after merge (soft consumed), got %v (ok=%v)", v, ok)
	}
}

func TestMergeWarnings_HardWarning(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	QueueWarning(env, Warning{
		Source:  "loop_detection",
		Level:   WarningLevelHard,
		Message: "stop calling tools",
	})

	result := MergeWarnings(env, "base")
	if !strings.Contains(result, "[SYSTEM WARNING - URGENT]") {
		t.Errorf("expected hard/urgent warning marker, got: %s", result)
	}
	if !strings.Contains(result, "[loop_detection]") {
		t.Errorf("expected loop_detection source, got: %s", result)
	}

	// Hard warnings should persist after MergeWarnings.
	v, ok := env.Get(WarningsKey)
	if !ok {
		t.Fatal("expected warnings key to still exist")
	}
	warnings, ok := v.([]Warning)
	if !ok {
		t.Fatalf("expected []Warning, got %T", v)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 hard warning remaining, got %d", len(warnings))
	}
	if warnings[0].Source != "loop_detection" {
		t.Errorf("expected loop_detection remaining, got %s", warnings[0].Source)
	}
}

func TestMergeWarnings_MixedSoftAndHard(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	QueueWarning(env, Warning{Source: "s1", Level: WarningLevelSoft, Message: "soft"})
	QueueWarning(env, Warning{Source: "s2", Level: WarningLevelHard, Message: "hard"})
	QueueWarning(env, Warning{Source: "s3", Level: WarningLevelSoft, Message: "soft2"})

	result := MergeWarnings(env, "base")
	if !strings.Contains(result, "soft") || !strings.Contains(result, "hard") || !strings.Contains(result, "soft2") {
		t.Errorf("expected all warnings merged, got: %s", result)
	}

	// Only hard warning should remain.
	v, _ := env.Get(WarningsKey)
	warnings := v.([]Warning)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 remaining (hard), got %d", len(warnings))
	}
	if warnings[0].Level != WarningLevelHard {
		t.Errorf("expected hard level remaining, got %s", warnings[0].Level)
	}
}

func TestMergeWarnings_EmptyBasePrompt(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	QueueWarning(env, Warning{Source: "test", Level: WarningLevelSoft, Message: "empty base"})

	result := MergeWarnings(env, "")
	if !strings.Contains(result, "[SYSTEM WARNING]") {
		t.Errorf("expected warning appended to empty prompt, got: %s", result)
	}
	// Note: basePrompt="" produces a leading \n\n since the warning format
	// uses double-newline separation. This is cosmetic but consistent.
}

func TestMergeWarnings_InvalidType(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	env.Set(WarningsKey, "not-a-slice") // inject wrong type

	result := MergeWarnings(env, "base")
	if result != "base" {
		t.Errorf("expected unchanged prompt with invalid warnings type, got: %s", result)
	}
}

func TestHasHardWarning_None(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	if HasHardWarning(env) {
		t.Error("expected no hard warning")
	}

	QueueWarning(env, Warning{Source: "s", Level: WarningLevelSoft, Message: "m"})
	if HasHardWarning(env) {
		t.Error("expected no hard warning with only soft warning")
	}
}

func TestHasHardWarning_True(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	QueueWarning(env, Warning{Source: "s", Level: WarningLevelHard, Message: "m"})
	if !HasHardWarning(env) {
		t.Error("expected hard warning")
	}
}

func TestClearWarnings(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})
	QueueWarning(env, Warning{Source: "s1", Level: WarningLevelHard, Message: "m1"})
	QueueWarning(env, Warning{Source: "s2", Level: WarningLevelSoft, Message: "m2"})

	ClearWarnings(env)

	v, ok := env.Get(WarningsKey)
	if !ok || v != nil {
		t.Errorf("expected nil value after clear, got %v (ok=%v)", v, ok)
	}
	if HasHardWarning(env) {
		t.Error("expected no hard warning after clear")
	}
}

func TestWarningConstants(t *testing.T) {
	if WarningLevelSoft != "soft" {
		t.Errorf("expected WarningLevelSoft='soft', got %s", WarningLevelSoft)
	}
	if WarningLevelHard != "hard" {
		t.Errorf("expected WarningLevelHard='hard', got %s", WarningLevelHard)
	}
	if WarningsKey != "pipeline.warnings" {
		t.Errorf("expected WarningsKey='pipeline.warnings', got %s", WarningsKey)
	}
}
