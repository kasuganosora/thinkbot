package dao

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRecountSessionStats 覆盖 B34：message_count 与实际行数对账。
// 最关键的是 id(整数) 与 session_id(TEXT) 的 CAST —— 少了它所有会话都会算成 0 条，
// 且 UPDATE 不报错，是典型的静默错。
func TestRecountSessionStats(t *testing.T) {
	db := newMigrationTestDB(t)
	const bot = "bot-a"
	base := time.Now().Add(-time.Hour)

	s1 := ChatSession{BotID: bot, Title: "有消息但计数错", Status: SessionStatusActive,
		MessageCount: 99, CreatedAt: base, UpdatedAt: base}
	s2 := ChatSession{BotID: bot, Title: "空会话", Status: SessionStatusActive,
		MessageCount: 7, LastMsgAt: &base, CreatedAt: base, UpdatedAt: base}
	if err := db.Create(&s1).Error; err != nil || s1.ID == 0 {
		t.Fatalf("create s1: %v", err)
	}
	if err := db.Create(&s2).Error; err != nil || s2.ID == 0 {
		t.Fatalf("create s2: %v", err)
	}

	sid := strconv.FormatUint(s1.ID, 10)
	msgs := []ChatMessage{
		{BotID: bot, UserID: "1", SessionID: sid, Role: ChatRoleUser, Content: "a", CreatedAt: base},
		{BotID: bot, UserID: "1", SessionID: sid, Role: ChatRoleAssistant, Content: "b", CreatedAt: base.Add(time.Minute)},
		{BotID: bot, UserID: "1", SessionID: sid, Role: ChatRoleUser, Content: "c", CreatedAt: base.Add(2 * time.Minute)},
	}
	if err := db.Create(&msgs).Error; err != nil {
		t.Fatalf("create messages: %v", err)
	}

	detail, err := recountSessionStats(db)
	if err != nil {
		t.Fatalf("recount: %v", err)
	}
	if !strings.Contains(detail, "corrected=2") {
		t.Errorf("detail = %q, want corrected=2 (99->3 and 7->0)", detail)
	}

	var got1 ChatSession
	if err := db.First(&got1, s1.ID).Error; err != nil {
		t.Fatalf("reload s1: %v", err)
	}
	if got1.MessageCount != 3 {
		t.Errorf("s1.message_count = %d, want 3", got1.MessageCount)
	}
	if got1.LastMsgAt == nil || !got1.LastMsgAt.Equal(base.Add(2*time.Minute)) {
		t.Errorf("s1.last_msg_at = %v, want %v", got1.LastMsgAt, base.Add(2*time.Minute))
	}

	var got2 ChatSession
	if err := db.First(&got2, s2.ID).Error; err != nil {
		t.Fatalf("reload s2: %v", err)
	}
	if got2.MessageCount != 0 {
		t.Errorf("s2.message_count = %d, want 0", got2.MessageCount)
	}
	// 空会话的 last_msg_at 必须被 COALESCE 保住，不能被写成 NULL
	// （NULL 在 ORDER BY last_msg_at DESC 下会沉底，掩盖问题）。
	if got2.LastMsgAt == nil {
		t.Error("s2.last_msg_at became NULL, want preserved")
	}
}
