package api

import (
	"strconv"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
)

func sessionCounts(t *testing.T, s *ChatHistoryService, id uint64) (stored, actual int64) {
	t.Helper()
	var sess dao.ChatSession
	if err := s.db.First(&sess, id).Error; err != nil {
		t.Fatalf("load session: %v", err)
	}
	if err := s.db.Model(&dao.ChatMessage{}).Where("session_id = ?", strconv.FormatUint(id, 10)).Count(&actual).Error; err != nil {
		t.Fatal(err)
	}
	return int64(sess.MessageCount), actual
}

// TestMessageCount_StaysConsistentAcrossWritePaths: every insert/delete path
// (save, streaming upsert, /compact delete + summary insert, /clear) keeps
// chat_sessions.message_count equal to the real row count, including when the
// stored counter had already drifted.
func TestMessageCount_StaysConsistentAcrossWritePaths(t *testing.T) {
	s := newB32TestHistory(t)
	const bot, user = "bot-a", "1"
	sess := dao.ChatSession{BotID: bot, Title: "t", Status: dao.SessionStatusActive, MessageCount: 4}
	if err := s.db.Create(&sess).Error; err != nil {
		t.Fatal(err)
	}
	sid := strconv.FormatUint(sess.ID, 10)
	check := func(step string) {
		t.Helper()
		if st, ac := sessionCounts(t, s, sess.ID); st != ac {
			t.Fatalf("%s: message_count=%d actual=%d", step, st, ac)
		}
	}

	for i := 0; i < 6; i++ {
		if err := s.SaveMessage(bot, user, dao.ChatRoleUser, "m"+strconv.Itoa(i), "t"+strconv.Itoa(i), sid); err != nil {
			t.Fatal(err)
		}
	}
	check("save (pre-drifted counter healed)")

	if err := s.UpsertAssistantByTrace(bot, user, "a", "tr", "", "", sid, true); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAssistantByTrace(bot, user, "ab", "tr", "", "", sid, false); err != nil {
		t.Fatal(err)
	}
	check("upsert insert+update")

	var rows []dao.ChatMessage
	s.db.Where("session_id = ?", sid).Order("id").Limit(5).Find(&rows)
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if n, err := s.DeleteMessages(bot, sid, ids); err != nil || n != 5 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
	check("compact delete")
	if err := s.SaveMessageAt(bot, user, dao.ChatRoleAssistant, "summary", "c", sid, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	check("compact summary insert")

	// Notify-style write into a *different* session must not touch this one.
	other := dao.ChatSession{BotID: bot, Title: "o", Status: dao.SessionStatusActive}
	s.db.Create(&other)
	before, _ := sessionCounts(t, s, sess.ID)
	if err := s.SaveMessage(bot, user, "notify", "alert", "ntf-1", strconv.FormatUint(other.ID, 10)); err != nil {
		t.Fatal(err)
	}
	if after, _ := sessionCounts(t, s, sess.ID); after != before {
		t.Fatalf("write to other session changed count %d -> %d", before, after)
	}
	if st, ac := sessionCounts(t, s, other.ID); st != ac || st != 1 {
		t.Fatalf("other session count=%d actual=%d", st, ac)
	}

	if _, err := s.ClearSessionMessages(sid); err != nil {
		t.Fatal(err)
	}
	check("clear")
}
