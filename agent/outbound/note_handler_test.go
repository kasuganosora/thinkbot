package outbound

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func TestNoteHandler_BasicSave(t *testing.T) {
	store := NewMemoryNoteStore()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(store, logger, tp)

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

	notes := store.Notes()
	if len(notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(notes))
	}
	n := notes[0]
	if n.Text != "User prefers Go over Python" {
		t.Errorf("note text = %q, want %q", n.Text, "User prefers Go over Python")
	}
	if n.BotID != "test-bot" {
		t.Errorf("bot_id = %q, want %q", n.BotID, "test-bot")
	}
	if n.Channel != "user-123" {
		t.Errorf("channel = %q, want %q", n.Channel, "user-123")
	}
	if n.UserID != "u1" {
		t.Errorf("user_id = %q, want %q", n.UserID, "u1")
	}
	if n.MessageID != "msg-42" {
		t.Errorf("message_id = %q, want %q", n.MessageID, "msg-42")
	}
	if n.Category != "preference" {
		t.Errorf("category = %q, want %q", n.Category, "preference")
	}
	if n.ID == "" {
		t.Error("note ID should not be empty")
	}
	if n.CreatedAt.IsZero() {
		t.Error("createdAt should not be zero")
	}
}

func TestNoteHandler_EmptyPayload_Skips(t *testing.T) {
	store := NewMemoryNoteStore()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(store, logger, tp)

	action := core.Action{
		Type:    core.ActionNote,
		Channel: "ch1",
		Payload: "", // empty
	}

	err := handler.Handle(context.Background(), action)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 0 {
		t.Errorf("expected 0 notes, got %d", store.Count())
	}
}

func TestNoteHandler_NilPayload_Skips(t *testing.T) {
	store := NewMemoryNoteStore()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(store, logger, tp)

	action := core.Action{
		Type:    core.ActionNote,
		Channel: "ch1",
		Payload: nil,
	}

	err := handler.Handle(context.Background(), action)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.Count() != 0 {
		t.Errorf("expected 0 notes, got %d", store.Count())
	}
}

func TestNoteHandler_StoreError(t *testing.T) {
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	store := &failingNoteStore{err: errors.New("disk full")}
	handler := NewNoteHandler(store, logger, tp)

	action := core.Action{
		Type:    core.ActionNote,
		Channel: "ch1",
		Payload: "something important",
	}

	err := handler.Handle(context.Background(), action)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, store.err) {
		t.Errorf("error = %v, want wrapped %v", err, store.err)
	}
}

func TestNoteHandler_MultipleNotes(t *testing.T) {
	store := NewMemoryNoteStore()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(store, logger, tp)

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

	if store.Count() != 5 {
		t.Errorf("expected 5 notes, got %d", store.Count())
	}

	// IDs should be unique
	notes := store.Notes()
	ids := make(map[string]bool)
	for _, n := range notes {
		if ids[n.ID] {
			t.Errorf("duplicate ID: %s", n.ID)
		}
		ids[n.ID] = true
	}
}

func TestNoteHandler_NoMetadata(t *testing.T) {
	store := NewMemoryNoteStore()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()
	handler := NewNoteHandler(store, logger, tp)

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
	n := store.LastNote()
	if n == nil {
		t.Fatal("expected a note")
	}
	if n.BotID != "" {
		t.Errorf("bot_id should be empty, got %q", n.BotID)
	}
	if n.Category != "" {
		t.Errorf("category should be empty, got %q", n.Category)
	}
}

func TestNoteHandler_IntegrationWithMultiDispatcher(t *testing.T) {
	store := NewMemoryNoteStore()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	noteHandler := NewNoteHandler(store, logger, tp)
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

	if store.Count() != 2 {
		t.Fatalf("expected 2 notes, got %d", store.Count())
	}
}

func TestNoteHandler_MixedActionsWithDispatcher(t *testing.T) {
	store := NewMemoryNoteStore()
	tp := noop.NewTracerProvider()
	logger := zap.NewNop().Sugar()

	noteHandler := NewNoteHandler(store, logger, tp)
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

	if store.Count() != 1 {
		t.Fatalf("expected 1 note saved, got %d", store.Count())
	}
	n := store.LastNote()
	if n.Text != "user asked about deployment" {
		t.Errorf("note text = %q", n.Text)
	}
}

// MemoryNoteStore tests

func TestMemoryNoteStore_Clear(t *testing.T) {
	store := NewMemoryNoteStore()
	_ = store.Save(context.Background(), Note{Text: "a"})
	_ = store.Save(context.Background(), Note{Text: "b"})

	if store.Count() != 2 {
		t.Fatalf("expected 2, got %d", store.Count())
	}

	store.Clear()
	if store.Count() != 0 {
		t.Fatalf("expected 0 after clear, got %d", store.Count())
	}
}

func TestMemoryNoteStore_LastNote_Empty(t *testing.T) {
	store := NewMemoryNoteStore()
	if store.LastNote() != nil {
		t.Error("LastNote on empty store should return nil")
	}
}

// --- helpers ---

type failingNoteStore struct {
	err error
}

func (s *failingNoteStore) Save(_ context.Context, _ Note) error {
	return s.err
}
