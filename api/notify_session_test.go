package api

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/notify"
)

// Regression (2026-10-19 smartd incident): a notify delivered to the owner's
// Telegram chat must land in the same session the next inbound Telegram
// message of that chat loads as history. Before the fix notify wrote to the
// legacy "tg:<chat>" key while inbound (since D6) reads the numeric session,
// so the model never saw its own alert.
func TestNotifyThenTelegramMessage_HistoryContainsNotify(t *testing.T) {
	e := newNotifyEnv(t)
	e.s.botSvc.db = e.db

	w := e.do(e.token, "", mdadmBody, nil)
	r := decodeNotify(t, w)
	if !r.Delivered {
		t.Fatalf("notify not delivered: %d %s", w.Code, w.Body)
	}

	// Inbound Telegram message in the same chat: resolve the session exactly as
	// the inbound-chat-history enricher does, then load its history window.
	in := core.Message{BotID: notifyTestBot, Channel: "76017910", TraceID: "tg-in-1", Text: "what happened?",
		Metadata: map[string]any{"channel_type": "telegram"}}
	sid, kind, ok := e.s.botSvc.inboundSessionID(&in)
	if !ok || kind != dao.KindDirect {
		t.Fatalf("inbound session: %q %q %v", sid, kind, ok)
	}
	history, err := e.s.chatHistory.LoadContextBySession(notifyTestBot, sid, 20)
	if err != nil {
		t.Fatal(err)
	}
	history = e.s.chatHistory.ApplyContextCheckpoint(in.TraceID, notifyTestBot, sid, history)
	found := false
	for _, m := range history {
		if m.Role == dao.ChatRoleNotify && m.TraceID == r.ID && strings.Contains(m.Content, "DegradedArray on /dev/md0") {
			found = true
		}
	}
	if !found {
		t.Fatalf("inbound history of session %s lacks the notify %s: %+v", sid, r.ID, history)
	}

	// The user message saved afterwards follows the notify in the same session.
	if err := e.s.chatHistory.SaveMessage(notifyTestBot, sid, dao.ChatRoleUser, in.Text, in.TraceID, sid); err != nil {
		t.Fatal(err)
	}
	history, _ = e.s.chatHistory.LoadContextBySession(notifyTestBot, sid, 20)
	if len(history) != 2 || history[0].Role != dao.ChatRoleNotify || history[1].Role != dao.ChatRoleUser {
		t.Fatalf("history order: %+v", history)
	}

	// Nothing went to the legacy key; the notify's own context load sees the same session.
	var legacy int64
	e.db.Model(&dao.ChatMessage{}).Where("session_id LIKE 'tg:%'").Count(&legacy)
	if legacy != 0 {
		t.Fatalf("%d rows written under a legacy tg: session", legacy)
	}
	got, err := e.s.notifySessionID(notifyTestBot, notify.Target{ChannelType: "telegram", ChatID: "76017910"})
	if err != nil || got != sid {
		t.Fatalf("notifySessionID = %q %v, want %s", got, err, sid)
	}
	if _, err := e.s.notifySessionID(notifyTestBot, notify.Target{ChannelType: "misskey", ChatID: "u1"}); err == nil {
		t.Fatal("non-telegram targets must not get a raw chat id as session id")
	}
}

// The legacy-row migration keeps original id/created_at, so 28 old alerts from
// 10-05/10-06 stay where they happened and do not displace the recent window
// (history loads ORDER BY created_at DESC, id DESC LIMIT n); recent alerts do
// enter the window at their real position.
func TestLegacyNotifyMigration_DoesNotDisplaceRecentWindow(t *testing.T) {
	e := newNotifyEnv(t)
	sid := e.ownerSID(t, "76017910")
	cst := time.FixedZone("CST", 8*3600)
	add := func(session, role, trace string, at time.Time) dao.ChatMessage {
		t.Helper()
		m := dao.ChatMessage{BotID: notifyTestBot, UserID: session, SessionID: session, Role: role,
			Content: role + " " + trace, TraceID: trace, CreatedAt: at}
		if err := e.db.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
		return m
	}

	// Inserted in chronological order, like production (ids grow with time).
	old := map[string]bool{}
	start := time.Date(2026, 10, 5, 2, 0, 0, 0, cst)
	for i := 0; i < 14; i++ {
		tr := fmt.Sprintf("ntf-old-%02d", i)
		old[tr] = true
		at := start.Add(time.Duration(i) * time.Hour)
		add("tg:76017910", dao.ChatRoleNotify, tr, at)
		add("tg:76017910", dao.ChatRoleAssistant, tr, at.Add(time.Millisecond))
	}
	chatStart := time.Date(2026, 10, 10, 10, 0, 0, 0, cst)
	var recent []dao.ChatMessage
	for i := 0; i < 20; i++ {
		role := dao.ChatRoleUser
		if i%2 == 1 {
			role = dao.ChatRoleAssistant
		}
		recent = append(recent, add(sid, role, "chat-"+strconv.Itoa(i), chatStart.Add(time.Duration(i)*time.Hour)))
	}
	alertAt := time.Date(2026, 10, 19, 21, 20, 28, 0, cst)
	add("tg:76017910", dao.ChatRoleNotify, "ntf-new-1", alertAt)
	add("tg:76017910", dao.ChatRoleAssistant, "ntf-new-1", alertAt.Add(time.Millisecond))
	add("tg:76017910", dao.ChatRoleNotify, "ntf-new-2", alertAt.Add(2*time.Second))
	add("tg:76017910", dao.ChatRoleAssistant, "ntf-new-2", alertAt.Add(2*time.Second+time.Millisecond))
	add(sid, dao.ChatRoleUser, "user-after", alertAt.Add(time.Minute))

	before, _ := e.s.chatHistory.LoadContextBySession(notifyTestBot, sid, 20)
	if _, err := dao.MoveLegacyNotifyRows(e.db); err != nil {
		t.Fatal(err)
	}
	rows, err := e.s.chatHistory.LoadContextBySession(notifyTestBot, sid, 20)
	if err != nil || len(rows) != 20 {
		t.Fatalf("window: %d %v", len(rows), err)
	}
	for _, m := range rows {
		if old[m.TraceID] {
			t.Fatalf("old migrated alert %s displaced the recent window: %+v", m.TraceID, rows)
		}
	}
	// Window = last 15 chat rows of 10-10..10-13, the 4 recent notify rows, the latest user row.
	if rows[0].TraceID != recent[5].TraceID || rows[14].TraceID != recent[19].TraceID ||
		rows[15].TraceID != "ntf-new-1" || rows[15].Role != dao.ChatRoleNotify ||
		rows[18].TraceID != "ntf-new-2" || rows[19].TraceID != "user-after" {
		t.Fatalf("unexpected window: %+v", rows)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].CreatedAt.Before(rows[i-1].CreatedAt) || rows[i].ID < rows[i-1].ID {
			t.Fatalf("window out of order at %d: %+v", i, rows)
		}
	}
	// Before the migration the same window had no alerts at all (the bug).
	for _, m := range before {
		if strings.HasPrefix(m.TraceID, "ntf-") {
			t.Fatalf("pre-migration window unexpectedly had %s", m.TraceID)
		}
	}
	// All 32 notify rows are in the numeric session now; total row count unchanged.
	var n, total int64
	e.db.Model(&dao.ChatMessage{}).Where("session_id = ? AND trace_id LIKE 'ntf-%'", sid).Count(&n)
	e.db.Model(&dao.ChatMessage{}).Count(&total)
	if n != 32 || total != 53 {
		t.Fatalf("moved=%d total=%d", n, total)
	}
}
