package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/stages"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

func newCheckpointTestHistory(t *testing.T) *ChatHistoryService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&dao.ChatMessage{}, &dao.ContextCheckpoint{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &ChatHistoryService{db: db, logger: zap.NewNop().Sugar()}
}

func seedSession(t *testing.T, s *ChatHistoryService, botID, sessionID string, n int) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < n; i++ {
		role := dao.ChatRoleUser
		if i%2 == 1 {
			role = dao.ChatRoleAssistant
		}
		if err := s.SaveMessageAt(botID, "u1", role, fmt.Sprintf("msg-%d", i), "t", sessionID, base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContextCheckpoint_HistoryUsesSummaryPlusRecent(t *testing.T) {
	s := newCheckpointTestHistory(t)
	seedSession(t, s, "bot", "sess", 10)
	seedSession(t, s, "bot", "other", 4)

	raw, err := s.LoadContextBySession("bot", "sess", 20)
	if err != nil || len(raw) != 10 {
		t.Fatalf("raw load: %d %v", len(raw), err)
	}
	// No checkpoint → unchanged.
	if got := s.ApplyContextCheckpoint("bot", "sess", raw); len(got) != 10 {
		t.Fatalf("no checkpoint should keep history, got %d", len(got))
	}

	boundary := raw[5].ID // msg-0..msg-5 compacted
	if err := s.SaveContextCheckpoint(stages.ContextCheckpoint{
		BotID: "bot", SessionID: "sess", BoundaryMessageID: boundary, Summary: "## Goal\n- old stuff", Source: "compact_context",
	}); err != nil {
		t.Fatal(err)
	}

	got := s.ApplyContextCheckpoint("bot", "sess", raw)
	if len(got) != 1+4 {
		t.Fatalf("want summary + 4 recent, got %d", len(got))
	}
	if got[0].Role != dao.ChatRoleContextSummary || got[0].ID != 0 {
		t.Fatalf("first entry should be synthetic summary: %+v", got[0])
	}
	if got[1].Content != "msg-6" || got[4].Content != "msg-9" {
		t.Fatalf("recent window wrong: %q..%q", got[1].Content, got[4].Content)
	}

	// MessageBuilder view: system summary + verbatim recent, IDs aligned.
	msgs, ids := chatHistoryToLLM(got)
	if len(msgs) != 5 || len(ids) != 5 {
		t.Fatalf("llm msgs=%d ids=%d", len(msgs), len(ids))
	}
	if msgs[0].Role != llm.MessageRoleSystem || !strings.Contains(llm.TextFromParts(msgs[0].Content), "old stuff") ||
		!strings.HasPrefix(llm.TextFromParts(msgs[0].Content), llm.ConversationSummaryHeader) {
		t.Fatalf("summary message wrong: %+v", msgs[0])
	}
	if ids[0] != 0 || ids[1] != got[1].ID || llm.TextFromParts(msgs[1].Content) != "msg-6" {
		t.Fatalf("alignment wrong: ids=%v", ids)
	}
	// Same via metadata (as the real MessageBuilder / HistoryMessageIDs see it).
	meta := core.Message{Metadata: map[string]any{"chat_history": got}}
	if mids := chatHistoryMessageIDs(meta); len(mids) != 5 || mids[4] != got[4].ID {
		t.Fatalf("chatHistoryMessageIDs=%v", mids)
	}

	// Raw rows are untouched (reversible / auditable).
	var cnt int64
	s.db.Model(&dao.ChatMessage{}).Where("session_id = ?", "sess").Count(&cnt)
	if cnt != 10 {
		t.Fatalf("raw rows must not be deleted, count=%d", cnt)
	}
	// Other sessions unaffected.
	other, _ := s.LoadContextBySession("bot", "other", 20)
	if got := s.ApplyContextCheckpoint("bot", "other", other); len(got) != 4 || got[0].Role == dao.ChatRoleContextSummary {
		t.Fatal("checkpoint leaked into another session")
	}

	// A newer checkpoint supersedes the old one (old kept, inactive).
	if err := s.SaveContextCheckpoint(stages.ContextCheckpoint{
		BotID: "bot", SessionID: "sess", BoundaryMessageID: raw[7].ID, Summary: "newer",
	}); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.LatestContextCheckpointBoundary("bot", "sess"); b != raw[7].ID {
		t.Fatalf("latest boundary=%d want %d", b, raw[7].ID)
	}
	var total, active int64
	s.db.Model(&dao.ContextCheckpoint{}).Count(&total)
	s.db.Model(&dao.ContextCheckpoint{}).Where("active = ?", true).Count(&active)
	if total != 2 || active != 1 {
		t.Fatalf("checkpoints total=%d active=%d, want 2/1", total, active)
	}
	got = s.ApplyContextCheckpoint("bot", "sess", raw)
	if len(got) != 3 || got[0].Content != "newer" {
		t.Fatalf("newer checkpoint not applied: %d %q", len(got), got[0].Content)
	}

	// Revert: deactivate → full history again.
	s.db.Model(&dao.ContextCheckpoint{}).Where("session_id = ?", "sess").Update("active", false)
	if got := s.ApplyContextCheckpoint("bot", "sess", raw); len(got) != 10 {
		t.Fatalf("after revert want full history, got %d", len(got))
	}
}

func TestContextCheckpoint_FailOpenAndGuards(t *testing.T) {
	var nilSvc *ChatHistoryService
	h := []dao.ChatMessage{{ID: 1, Role: dao.ChatRoleUser, Content: "x"}}
	if got := nilSvc.ApplyContextCheckpoint("b", "s", h); len(got) != 1 {
		t.Fatal("nil service must return history unchanged")
	}
	s := newTestChatHistory(t) // no context_checkpoints table → lookup error → fail open
	if got := s.ApplyContextCheckpoint("b", "s", h); len(got) != 1 {
		t.Fatal("lookup error must fail open")
	}
	if got := s.ApplyContextCheckpoint("b", "", h); len(got) != 1 {
		t.Fatal("empty session must be ignored")
	}
	if err := s.SaveContextCheckpoint(stages.ContextCheckpoint{BotID: "b"}); err == nil {
		t.Fatal("checkpoint without session must be rejected")
	}
}

func TestContextCheckpoint_LastContextCheckpointAt(t *testing.T) {
	s := newCheckpointTestHistory(t)
	if at, err := s.LastContextCheckpointAt("bot", "sess"); err != nil || !at.IsZero() {
		t.Fatalf("no checkpoint: %v %v", at, err)
	}
	before := time.Now().Add(-time.Second)
	if err := s.SaveContextCheckpoint(stages.ContextCheckpoint{BotID: "bot", SessionID: "sess", BoundaryMessageID: 1, Summary: "x"}); err != nil {
		t.Fatal(err)
	}
	at, err := s.LastContextCheckpointAt("bot", "sess")
	if err != nil || at.Before(before) {
		t.Fatalf("last at: %v %v", at, err)
	}
	// Deactivated checkpoints still count for the cooldown.
	s.db.Model(&dao.ContextCheckpoint{}).Update("active", false)
	if at2, _ := s.LastContextCheckpointAt("bot", "sess"); !at2.Equal(at) {
		t.Fatalf("deactivated checkpoint should still report its time: %v vs %v", at2, at)
	}
	if at3, _ := s.LastContextCheckpointAt("bot", "other"); !at3.IsZero() {
		t.Fatal("other session must be independent")
	}
}
