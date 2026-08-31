package outbound

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// 测试辅助 — 记录型 NoteWriter
// ============================================================================

// recordingNoteWriter 记录所有写入的 NoteEntry（测试用）。
type recordingNoteWriter struct {
	mu      sync.Mutex
	entries []NoteEntry
}

func newRecordingNoteWriter() *recordingNoteWriter {
	return &recordingNoteWriter{}
}

func (w *recordingNoteWriter) WriteNote(_ context.Context, entry NoteEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, entry)
	return nil
}

func (w *recordingNoteWriter) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.entries)
}

func (w *recordingNoteWriter) Last() *NoteEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.entries) == 0 {
		return nil
	}
	e := w.entries[len(w.entries)-1]
	return &e
}

func (w *recordingNoteWriter) All() []NoteEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]NoteEntry, len(w.entries))
	copy(out, w.entries)
	return out
}

// failingNoteWriter 始终返回错误。
type failingNoteWriter struct {
	err error
}

func (w *failingNoteWriter) WriteNote(_ context.Context, _ NoteEntry) error {
	return w.err
}

// ============================================================================
// NoteHandler 测试
// ============================================================================

func TestNoteHandler_BasicSave(t *testing.T) {
	writer := newRecordingNoteWriter()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(writer, logger, tp)

	action := core.Action{
		Type:    core.ActionNote,
		Channel: "user-123",
		UserID:  "u1",
		Payload: "User prefers Go over Python",
		Metadata: map[string]any{
			"source_channel": "mem",
			"bot_id":         "test-bot",
			"message_id":     "msg-42",
			"category":       "preference",
		},
	}

	err := handler.Handle(context.Background(), action)
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}

	if writer.Count() != 1 {
		t.Fatalf("expected 1 entry, got %d", writer.Count())
	}
	e := writer.Last()
	if e.Content != "User prefers Go over Python" {
		t.Errorf("content = %q, want %q", e.Content, "User prefers Go over Python")
	}
	if e.Source != "note" {
		t.Errorf("source = %q, want %q", e.Source, "note")
	}
	if e.Category != "preference" {
		t.Errorf("category = %q, want %q", e.Category, "preference")
	}
	if e.ID == "" {
		t.Error("entry ID should not be empty")
	}
	if e.CreatedAt.IsZero() {
		t.Error("createdAt should not be zero")
	}
	if e.ScopeKind != "channel" {
		t.Errorf("scopeKind = %q, want %q", e.ScopeKind, "channel")
	}
	if e.ScopeID != "user-123" {
		t.Errorf("scopeID = %q, want %q", e.ScopeID, "user-123")
	}
	// 验证 metadata 中保留了关联信息
	if e.Metadata["bot_id"] != "test-bot" {
		t.Errorf("metadata[bot_id] = %v, want test-bot", e.Metadata["bot_id"])
	}
	if e.Metadata["message_id"] != "msg-42" {
		t.Errorf("metadata[message_id] = %v, want msg-42", e.Metadata["message_id"])
	}
	if e.Metadata["user_id"] != "u1" {
		t.Errorf("metadata[user_id] = %v, want u1", e.Metadata["user_id"])
	}
}

func TestNoteHandler_EmptyPayload_Skips(t *testing.T) {
	writer := newRecordingNoteWriter()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(writer, logger, tp)

	action := core.Action{
		Type:    core.ActionNote,
		Channel: "ch1",
		Payload: "", // empty
	}

	err := handler.Handle(context.Background(), action)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if writer.Count() != 0 {
		t.Errorf("expected 0 entries, got %d", writer.Count())
	}
}

func TestNoteHandler_NilPayload_Skips(t *testing.T) {
	writer := newRecordingNoteWriter()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(writer, logger, tp)

	action := core.Action{
		Type:    core.ActionNote,
		Channel: "ch1",
		Payload: nil,
	}

	err := handler.Handle(context.Background(), action)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if writer.Count() != 0 {
		t.Errorf("expected 0 entries, got %d", writer.Count())
	}
}

func TestNoteHandler_StoreError(t *testing.T) {
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	writer := &failingNoteWriter{err: errors.New("disk full")}
	handler := NewNoteHandler(writer, logger, tp)

	action := core.Action{
		Type:    core.ActionNote,
		Channel: "ch1",
		Payload: "something important",
	}

	err := handler.Handle(context.Background(), action)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, writer.err) {
		t.Errorf("error = %v, want wrapped %v", err, writer.err)
	}
}

func TestNoteHandler_MultipleNotes(t *testing.T) {
	writer := newRecordingNoteWriter()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(writer, logger, tp)

	for i := 0; i < 5; i++ {
		action := core.Action{
			Type:    core.ActionNote,
			Channel: "ch1",
			Payload: "note content",
		}
		if err := handler.Handle(context.Background(), action); err != nil {
			t.Fatalf("Handle %d failed: %v", i, err)
		}
	}

	if writer.Count() != 5 {
		t.Errorf("expected 5 entries, got %d", writer.Count())
	}

	// IDs should be unique
	ids := make(map[string]bool)
	for _, e := range writer.All() {
		if ids[e.ID] {
			t.Errorf("duplicate ID: %s", e.ID)
		}
		ids[e.ID] = true
	}
}

func TestNoteHandler_NoMetadata(t *testing.T) {
	writer := newRecordingNoteWriter()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(writer, logger, tp)

	action := core.Action{
		Type:    core.ActionNote,
		Channel: "ch1",
		Payload: "bare note",
		// no Metadata
	}

	err := handler.Handle(context.Background(), action)
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}
	e := writer.Last()
	if e == nil {
		t.Fatal("expected an entry")
	}
	// 默认 category 应为 "observation"
	if e.Category != "observation" {
		t.Errorf("category = %q, want %q", e.Category, "observation")
	}
	// 默认 importance 应为 0.5
	if e.Importance != 0.5 {
		t.Errorf("importance = %f, want 0.5", e.Importance)
	}
}

func TestNoteHandler_BotScope_WhenNoChannel(t *testing.T) {
	writer := newRecordingNoteWriter()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(writer, logger, tp)

	action := core.Action{
		Type:    core.ActionNote,
		Channel: "", // 无 channel
		Payload: "bot-level insight",
		Metadata: map[string]any{
			"bot_id": "bot-1",
		},
	}

	err := handler.Handle(context.Background(), action)
	if err != nil {
		t.Fatalf("Handle failed: %v", err)
	}

	e := writer.Last()
	if e == nil {
		t.Fatal("expected an entry")
	}
	if e.ScopeKind != "bot" {
		t.Errorf("scopeKind = %q, want bot", e.ScopeKind)
	}
	if e.ScopeID != "bot-1" {
		t.Errorf("scopeID = %q, want bot-1", e.ScopeID)
	}
	if e.Content != "bot-level insight" {
		t.Errorf("content = %q", e.Content)
	}
}

func TestNoteHandler_IntegrationWithMultiDispatcher(t *testing.T) {
	writer := newRecordingNoteWriter()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	noteHandler := NewNoteHandler(writer, logger, tp)
	dispatcher := NewMultiDispatcher(logger, tp)
	dispatcher.Register(core.ActionNote, noteHandler)

	actions := []core.Action{
		{
			Type:    core.ActionNote,
			Channel: "user-1",
			Payload: "first note",
			Metadata: map[string]any{
				"category": "todo",
			},
		},
		{
			Type:    core.ActionNote,
			Channel: "user-2",
			Payload: "second note",
			Metadata: map[string]any{
				"category": "insight",
			},
		},
	}

	err := dispatcher.Dispatch(context.Background(), actions)
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}

	if writer.Count() != 2 {
		t.Errorf("expected 2 entries, got %d", writer.Count())
	}
}

func TestNoteHandler_MixedActionsWithDispatcher(t *testing.T) {
	writer := newRecordingNoteWriter()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	noteHandler := NewNoteHandler(writer, logger, tp)
	replyHandler := NewChannelReplyHandler(logger, tp)

	dispatcher := NewMultiDispatcher(logger, tp)
	dispatcher.Register(core.ActionNote, noteHandler)
	dispatcher.Register(core.ActionReply, replyHandler)

	actions := []core.Action{
		{
			Type:    core.ActionReply,
			Channel: "chat-123",
			Payload: "hello user",
			Metadata: map[string]any{
				"source_channel": "tg-bot",
			},
		},
		{
			Type:    core.ActionNote,
			Channel: "user-1",
			Payload: "user asked about deployment",
			Metadata: map[string]any{
				"category": "observation",
			},
		},
	}

	// Reply will fail (no sender registered), but Note should succeed
	_ = dispatcher.Dispatch(context.Background(), actions)

	if writer.Count() != 1 {
		t.Fatalf("expected 1 entry saved, got %d", writer.Count())
	}
	if writer.Last().Content != "user asked about deployment" {
		t.Errorf("content = %q", writer.Last().Content)
	}
}

func TestNoteHandler_SourceAlwaysNote(t *testing.T) {
	writer := newRecordingNoteWriter()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(writer, logger, tp)

	action := core.Action{
		Type:    core.ActionNote,
		Channel: "ch1",
		Payload: "thinking about something",
	}

	_ = handler.Handle(context.Background(), action)

	if writer.Count() != 1 {
		t.Fatal("expected 1 entry")
	}
	if writer.Last().Source != "note" {
		t.Errorf("source = %q, want %q", writer.Last().Source, "note")
	}
}
