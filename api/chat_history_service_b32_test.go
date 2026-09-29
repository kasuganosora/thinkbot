package api

import (
	"strconv"
	"testing"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/dao"
)

// newB32TestHistory 建一个同时有 chat_messages 与 chat_sessions 的私有内存库。
// 与 newTestChatHistory 的区别：后者只建 chat_messages，而 B32 的兜底要写会话行。
func newB32TestHistory(t *testing.T) *ChatHistoryService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&dao.ChatMessage{}, &dao.ChatSession{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return &ChatHistoryService{db: db, logger: zap.NewNop().Sugar()}
}

// TestSaveMessage_EmptySessionID_FallsBackToDefaultSession 覆盖 B32：
// sessionID 为空时不再落孤儿行，而是解析（必要时创建）该 bot+user 的默认会话。
func TestSaveMessage_EmptySessionID_FallsBackToDefaultSession(t *testing.T) {
	s := newB32TestHistory(t)
	const bot, user = "bot-a", "1"

	if err := s.SaveMessage(bot, user, "user", "你好", "t1", ""); err != nil {
		t.Fatalf("save: %v", err)
	}

	var msg dao.ChatMessage
	if err := s.db.Where("trace_id = ?", "t1").First(&msg).Error; err != nil {
		t.Fatalf("load message: %v", err)
	}
	if msg.SessionID == "" {
		t.Fatal("session_id still empty -> orphan row (B32 未修复)")
	}
	sid, err := strconv.ParseUint(msg.SessionID, 10, 64)
	if err != nil {
		t.Fatalf("session_id %q is not numeric: %v", msg.SessionID, err)
	}

	var sess dao.ChatSession
	if err := s.db.First(&sess, sid).Error; err != nil {
		t.Fatalf("session row %d not found: %v", sid, err)
	}
	if sess.ExternalKey != dao.WebDefaultKey(user) {
		t.Errorf("external_key = %q, want %q", sess.ExternalKey, dao.WebDefaultKey(user))
	}
	if sess.SessionKind != dao.KindWeb {
		t.Errorf("session_kind = %q, want %q", sess.SessionKind, dao.KindWeb)
	}
}

// TestSaveMessage_EmptySessionID_ConvergesOnSameSession 同一 bot+user 反复以空
// sessionID 写入必须收敛到同一行 —— 否则「兜底」只是把孤儿换个方式继续造。
func TestSaveMessage_EmptySessionID_ConvergesOnSameSession(t *testing.T) {
	s := newB32TestHistory(t)
	const bot, user = "bot-a", "1"

	for i, trace := range []string{"t1", "t2", "t3"} {
		if err := s.SaveMessage(bot, user, "user", "msg "+trace, trace, ""); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	var n int64
	s.db.Model(&dao.ChatSession{}).Where("bot_id = ?", bot).Count(&n)
	if n != 1 {
		t.Errorf("sessions created = %d, want 1 (all fallback writes must converge)", n)
	}

	var msgs []dao.ChatMessage
	s.db.Where("bot_id = ?", bot).Find(&msgs)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	first := msgs[0].SessionID
	for _, m := range msgs {
		if m.SessionID != first {
			t.Errorf("session_id drifted: %q vs %q", m.SessionID, first)
		}
	}
}

// TestSaveMessage_EmptySessionID_SeparatesUsers 不同 user 的兜底会话必须分开，
// 否则单用户以外的写入会串到同一个「默认会话」里。
func TestSaveMessage_EmptySessionID_SeparatesUsers(t *testing.T) {
	s := newB32TestHistory(t)
	const bot = "bot-a"

	if err := s.SaveMessage(bot, "1", "user", "a", "t1", ""); err != nil {
		t.Fatalf("save u1: %v", err)
	}
	if err := s.SaveMessage(bot, "2", "user", "b", "t2", ""); err != nil {
		t.Fatalf("save u2: %v", err)
	}

	var s1, s2 dao.ChatMessage
	s.db.Where("trace_id = ?", "t1").First(&s1)
	s.db.Where("trace_id = ?", "t2").First(&s2)
	if s1.SessionID == s2.SessionID {
		t.Errorf("different users share session %q", s1.SessionID)
	}
}

// TestSaveMessage_NonEmptySessionID_Unchanged sessionID 非空时必须原样使用，
// 兜底只能补空、不能改写调用方显式指定的会话（否则会静默改错归属）。
func TestSaveMessage_NonEmptySessionID_Unchanged(t *testing.T) {
	s := newB32TestHistory(t)

	if err := s.SaveMessage("bot-a", "1", "user", "x", "t1", "42"); err != nil {
		t.Fatalf("save: %v", err)
	}
	var msg dao.ChatMessage
	s.db.Where("trace_id = ?", "t1").First(&msg)
	if msg.SessionID != "42" {
		t.Errorf("session_id = %q, want 42 (explicit id must not be overridden)", msg.SessionID)
	}

	// 兜底路径不应凭空建出会话行。
	var n int64
	s.db.Model(&dao.ChatSession{}).Count(&n)
	if n != 0 {
		t.Errorf("sessions created = %d, want 0", n)
	}
}
