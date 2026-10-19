package dao

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMoveLegacyNotifyRows covers the 2026-10-19 fix: notify rows written to the
// legacy "tg:<chat>" key after D6 are moved into the chat's numeric session by
// an UPDATE of session_id only; non-notify rows, other bots and rows inside an
// existing compaction range are left alone; counters are recomputed; re-running
// is a no-op.
func TestMoveLegacyNotifyRows(t *testing.T) {
	db := newMigrationTestDB(t)
	const bot, otherBot = "bot-a", "bot-b"
	base := time.Date(2026, 10, 5, 2, 0, 0, 0, time.FixedZone("CST", 8*3600))

	mustSession := func(botID, key, kind string) ChatSession {
		t.Helper()
		s := ChatSession{BotID: botID, Title: "t", Status: SessionStatusActive, ExternalKey: key,
			SessionKind: kind, MessageCount: 1, CreatedAt: base, UpdatedAt: base}
		if err := db.Create(&s).Error; err != nil {
			t.Fatalf("create session: %v", err)
		}
		return s
	}
	msg := func(botID, sid, role, trace string, at time.Time) ChatMessage {
		t.Helper()
		m := ChatMessage{BotID: botID, UserID: sid, SessionID: sid, Role: role, Content: role + " " + trace,
			TraceID: trace, CreatedAt: at}
		if err := db.Create(&m).Error; err != nil {
			t.Fatalf("create message: %v", err)
		}
		return m
	}

	// chat 76017910: numeric session exists (backfill/inbound created it).
	s22 := mustSession(bot, TelegramChatKey("76017910"), KindDirect)
	sid22 := strconv.FormatUint(s22.ID, 10)
	oldNote := msg(bot, "tg:76017910", ChatRoleNotify, "ntf-old", base)
	oldSaid := msg(bot, "tg:76017910", ChatRoleAssistant, "ntf-old", base.Add(time.Millisecond))
	msg(bot, sid22, ChatRoleUser, "u1", base.Add(5*24*time.Hour))
	legacyUser := msg(bot, "tg:76017910", ChatRoleUser, "t-legacy", base.Add(6*24*time.Hour)) // non-notify: must stay
	newNote := msg(bot, "tg:76017910", ChatRoleNotify, "ntf-new", base.Add(14*24*time.Hour))
	msg(bot, sid22, ChatRoleUser, "u2", base.Add(14*24*time.Hour+time.Minute))

	// group chat without a numeric session yet: one is created.
	msg(bot, "tg:-100123", ChatRoleNotify, "ntf-grp", base)
	// malformed legacy key: skipped.
	msg(bot, "tg:abc", ChatRoleNotify, "ntf-bad", base)
	// another bot, same chat id: goes to its own session, never to bot-a's.
	sB := mustSession(otherBot, TelegramChatKey("76017910"), KindDirect)
	msg(otherBot, "tg:76017910", ChatRoleNotify, "ntf-b", base)

	// chat 555: session has a compaction checkpoint; notify rows at or below the
	// boundary stay in the legacy session, newer ones move.
	s555 := mustSession(bot, TelegramChatKey("555"), KindDirect)
	sid555 := strconv.FormatUint(s555.ID, 10)
	belowNote := msg(bot, "tg:555", ChatRoleNotify, "ntf-below", base)
	covered := msg(bot, sid555, ChatRoleUser, "c1", base.Add(time.Hour))
	if err := db.Create(&ContextCheckpoint{BotID: bot, SessionID: sid555, BoundaryMessageID: covered.ID,
		Summary: "s", Active: false, CreatedAt: base}).Error; err != nil {
		t.Fatal(err)
	}
	aboveNote := msg(bot, "tg:555", ChatRoleNotify, "ntf-above", base.Add(2*time.Hour))

	detail, err := MoveLegacyNotifyRows(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !strings.Contains(detail, "moved=6") || !strings.Contains(detail, "kept_below_boundary=1") ||
		!strings.Contains(detail, "skipped_groups=1") {
		t.Fatalf("detail = %q", detail)
	}

	reload := func(id uint64) ChatMessage {
		t.Helper()
		var m ChatMessage
		if err := db.First(&m, id).Error; err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, m := range []ChatMessage{oldNote, oldSaid, newNote} {
		got := reload(m.ID)
		if got.SessionID != sid22 {
			t.Errorf("row %d session = %q, want %s", m.ID, got.SessionID, sid22)
		}
		if !got.CreatedAt.Equal(m.CreatedAt) || got.UserID != m.UserID || got.Content != m.Content || got.TraceID != m.TraceID {
			t.Errorf("row %d changed beyond session_id: %+v", m.ID, got)
		}
	}
	if got := reload(legacyUser.ID); got.SessionID != "tg:76017910" {
		t.Errorf("non-notify legacy row moved to %q", got.SessionID)
	}
	if got := reload(belowNote.ID); got.SessionID != "tg:555" {
		t.Errorf("row inside compaction range moved to %q", got.SessionID)
	}
	if got := reload(aboveNote.ID); got.SessionID != sid555 {
		t.Errorf("row above boundary session = %q, want %s", got.SessionID, sid555)
	}
	var bad ChatMessage
	db.Where("trace_id = ?", "ntf-bad").First(&bad)
	if bad.SessionID != "tg:abc" {
		t.Errorf("malformed key moved to %q", bad.SessionID)
	}
	var b ChatMessage
	db.Where("trace_id = ?", "ntf-b").First(&b)
	if b.SessionID != strconv.FormatUint(sB.ID, 10) {
		t.Errorf("other bot row session = %q, want %d", b.SessionID, sB.ID)
	}
	grp, found, err := LookupSessionByExternalKey(db, bot, TelegramChatKey("-100123"))
	if err != nil || !found || grp.SessionKind != KindGroup {
		t.Fatalf("group session: %+v found=%v err=%v", grp, found, err)
	}

	// No duplicate rows were created: total row count unchanged.
	var total int64
	db.Model(&ChatMessage{}).Count(&total)
	if total != 12 {
		t.Errorf("total rows = %d, want 12 (UPDATE only)", total)
	}

	checkCount := func(id uint64, want int, wantLast time.Time) {
		t.Helper()
		var s ChatSession
		if err := db.First(&s, id).Error; err != nil {
			t.Fatal(err)
		}
		if s.MessageCount != want {
			t.Errorf("session %d message_count = %d, want %d", id, s.MessageCount, want)
		}
		if s.LastMsgAt == nil || !s.LastMsgAt.Equal(wantLast) {
			t.Errorf("session %d last_msg_at = %v, want %v", id, s.LastMsgAt, wantLast)
		}
	}
	checkCount(s22.ID, 5, base.Add(14*24*time.Hour+time.Minute))
	checkCount(s555.ID, 2, base.Add(2*time.Hour))
	checkCount(grp.ID, 1, base)

	// Re-run: nothing moves, nothing changes.
	detail2, err := MoveLegacyNotifyRows(db)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if !strings.Contains(detail2, "moved=0") {
		t.Fatalf("rerun detail = %q", detail2)
	}
	var total2 int64
	db.Model(&ChatMessage{}).Count(&total2)
	if total2 != total {
		t.Errorf("rerun changed row count %d -> %d", total, total2)
	}
	checkCount(s22.ID, 5, base.Add(14*24*time.Hour+time.Minute))
	if got := reload(belowNote.ID); got.SessionID != "tg:555" {
		t.Errorf("rerun moved row inside compaction range")
	}
}

// TestMoveLegacyNotifyRowsRegistered makes sure the move runs on startup, after
// the orphan backfill it depends on.
func TestMoveLegacyNotifyRowsRegistered(t *testing.T) {
	backfill, move := -1, -1
	for i, m := range dataMigrations {
		switch m.name {
		case orphanSessionBackfillName:
			backfill = i
		case NotifyLegacySessionMoveName:
			move = i
		}
	}
	if move < 0 || backfill < 0 || move < backfill {
		t.Fatalf("notify move must be registered after orphan backfill: backfill=%d move=%d", backfill, move)
	}
}
