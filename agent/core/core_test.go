package core

import (
	"context"
	"testing"
	"time"
)

// ============================================================================
// Envelope 测试
// ============================================================================

func TestNewEnvelope(t *testing.T) {
	msg := Message{
		ID:        "msg-001",
		Source:    "test",
		Channel:   "ch-1",
		UserID:    "user-1",
		Text:      "hello",
		CreatedAt: time.Now(),
	}
	env := NewEnvelope(msg)

	if env.Message.ID != "msg-001" {
		t.Errorf("expected ID msg-001, got %s", env.Message.ID)
	}
	if env.Aborted() {
		t.Error("new envelope should not be aborted")
	}
	if env.Err() != nil {
		t.Error("new envelope should have nil error")
	}
	if len(env.Actions()) != 0 {
		t.Error("new envelope should have no actions")
	}
}

func TestEnvelope_SetGet(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})

	env.Set("key1", "value1")
	env.Set("key2", 42)

	v1, ok1 := env.Get("key1")
	if !ok1 || v1 != "value1" {
		t.Errorf("expected value1, got %v (ok=%v)", v1, ok1)
	}

	v2, ok2 := env.Get("key2")
	if !ok2 || v2 != 42 {
		t.Errorf("expected 42, got %v (ok=%v)", v2, ok2)
	}

	_, ok3 := env.Get("nonexistent")
	if ok3 {
		t.Error("expected false for nonexistent key")
	}
}

func TestEnvelope_MustGet_Panic(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for missing key")
		}
	}()
	env.MustGet("missing")
}

func TestEnvelope_AddAction(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})

	env.AddAction(Action{Type: ActionReply, Channel: "ch-1", Payload: "hello"})
	env.AddAction(Action{Type: ActionForward, Channel: "ch-2"})

	actions := env.Actions()
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}
	if actions[0].Type != ActionReply {
		t.Errorf("expected reply, got %s", actions[0].Type)
	}
	if actions[1].Type != ActionForward {
		t.Errorf("expected forward, got %s", actions[1].Type)
	}

	// Verify Actions() returns a copy
	actions[0].Type = ActionDrop
	original := env.Actions()
	if original[0].Type != ActionReply {
		t.Error("Actions() should return a copy")
	}
}

func TestEnvelope_Abort(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})

	if env.Aborted() {
		t.Error("should not be aborted initially")
	}

	testErr := &AbortError{Reason: "test abort"}
	env.Abort(testErr)

	if !env.Aborted() {
		t.Error("should be aborted after Abort()")
	}
	if env.Err() != testErr {
		t.Errorf("expected abort error, got %v", env.Err())
	}
}

func TestEnvelope_SetErr(t *testing.T) {
	env := NewEnvelope(Message{ID: "test"})

	testErr := &PipelineError{Stage: "test", Message: "boom"}
	env.SetErr(testErr)

	if env.Aborted() {
		t.Error("SetErr should not abort")
	}
	if env.Err() != testErr {
		t.Errorf("expected pipeline error, got %v", env.Err())
	}
}

// ============================================================================
// Stage 测试
// ============================================================================

func TestStageFunc(t *testing.T) {
	called := false
	stage := &StageFunc{
		StageName: "test-stage",
		Fn: func(ctx context.Context, env *Envelope) (*Envelope, error) {
			called = true
			env.Set("processed", true)
			return env, nil
		},
	}

	if stage.Name() != "test-stage" {
		t.Errorf("expected test-stage, got %s", stage.Name())
	}

	env := NewEnvelope(Message{ID: "msg-1"})
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("stage function was not called")
	}
	v, _ := result.Get("processed")
	if v != true {
		t.Error("expected processed=true")
	}
}

// ============================================================================
// Error 测试
// ============================================================================

func TestPipelineError(t *testing.T) {
	err := &PipelineError{
		Stage:   "filter",
		Message: "invalid format",
	}
	expected := `pipeline stage "filter": invalid format`
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}

	cause := &PipelineError{
		Stage:   "llm",
		Message: "timeout",
		Cause:   context.DeadlineExceeded,
	}
	if cause.Unwrap() != context.DeadlineExceeded {
		t.Error("Unwrap should return cause")
	}
}

func TestAbortError(t *testing.T) {
	err := &AbortError{Reason: "rate limit"}
	if !IsAbortError(err) {
		t.Error("should be recognized as AbortError")
	}
	if IsAbortError(context.Canceled) {
		t.Error("context.Canceled should not be AbortError")
	}
}

func TestSkipError(t *testing.T) {
	err := &SkipError{Reason: "not applicable"}
	if !IsSkipError(err) {
		t.Error("should be recognized as SkipError")
	}
	if IsSkipError(context.Canceled) {
		t.Error("context.Canceled should not be SkipError")
	}
	expected := "stage skipped: not applicable"
	if err.Error() != expected {
		t.Errorf("expected %q, got %q", expected, err.Error())
	}
}

// ============================================================================
// ActionType 常量测试
// ============================================================================

func TestActionTypes(t *testing.T) {
	types := []ActionType{
		ActionReply, ActionForward, ActionBroadcast,
		ActionNote, ActionCallback, ActionSilent, ActionDrop,
	}
	expected := []string{
		"reply", "forward", "broadcast",
		"note", "callback", "silent", "drop",
	}
	for i, at := range types {
		if string(at) != expected[i] {
			t.Errorf("action type %d: expected %s, got %s", i, expected[i], at)
		}
	}

	// 确保所有 ActionType 常量都被测试覆盖
	if len(types) != 7 {
		t.Errorf("expected 7 action types, got %d — if you added a new ActionType, update this test", len(types))
	}
}
