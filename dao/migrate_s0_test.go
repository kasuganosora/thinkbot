package dao

import (
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// legacySessionDDL S0 之前（2026-09-29 实测 thinkbot.db）的两张表结构。
// 用它建库是为了验证**存量升级**路径——GORM AutoMigrate 不会给 SQLite 存量表
// 加列（见 migrate.go），新列全靠 ensureColumns，索引全靠 ensureIndexes。
const legacySessionDDL = `
CREATE TABLE chat_sessions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  bot_id TEXT NOT NULL,
  title TEXT,
  status TEXT NOT NULL DEFAULT 'active',
  message_count INTEGER NOT NULL DEFAULT 0,
  last_msg_at DATETIME,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);
CREATE TABLE chat_messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  bot_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  session_id TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL,
  content TEXT,
  trace_id TEXT,
  streaming INTEGER NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL,
  tool_calls TEXT,
  parts_json TEXT DEFAULT ''
);`

// newS0TestDB 建一个「S0 之前」的存量库（旧 schema + 少量数据）。
func newS0TestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "s0.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.Exec(legacySessionDDL).Error; err != nil {
		t.Fatalf("create legacy tables: %v", err)
	}
	return db
}

func tableColumns(t *testing.T, db *gorm.DB, table string) map[string]bool {
	t.Helper()
	var names []string
	if err := db.Raw("SELECT name FROM pragma_table_info(?)", table).Scan(&names).Error; err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func hasIndex(t *testing.T, db *gorm.DB, name string) bool {
	t.Helper()
	var cnt int64
	if err := db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name = ?", name).Scan(&cnt).Error; err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	return cnt > 0
}

func indexSQL(t *testing.T, db *gorm.DB, name string) string {
	t.Helper()
	var sql string
	err := db.Raw("SELECT IFNULL(sql,'') FROM sqlite_master WHERE type='index' AND name = ?", name).Scan(&sql).Error
	if err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	return sql
}

func countRows(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw("SELECT COUNT(*) FROM " + table).Scan(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestMigrateSessionPartitionSchema 验证 S0：加列、加表、加索引，且存量数据一行不丢。
func TestMigrateSessionPartitionSchema(t *testing.T) {
	db := newS0TestDB(t)

	// 存量数据：1 个会话 + 2 条消息（模拟基线 chat_sessions=2 / chat_messages=479 的形态）
	if err := db.Exec(`INSERT INTO chat_sessions (bot_id, title, status, message_count, created_at, updated_at)
		VALUES ('bot-a', '老会话', 'active', 2, '2026-09-01 10:00:00', '2026-09-01 10:00:00')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO chat_messages (bot_id, user_id, session_id, role, content, created_at)
		VALUES ('bot-a', 'u1', '1', 'user', '迁移前的老消息', '2026-09-01 10:00:01'),
		       ('bot-a', 'u1', '1', 'assistant', '老回复', '2026-09-01 10:00:02')`).Error; err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // 第二次必须是空操作（幂等）
		if err := Migrate(db); err != nil {
			t.Fatalf("Migrate run #%d: %v", i+1, err)
		}
	}

	// 1. 新列全部就位
	want := map[string][]string{
		"chat_sessions": {"external_key", "session_kind", "thread_root", "parent_session_id", "root_message_id"},
		"chat_messages": {"external_msg_id", "is_context", "origin_session_id", "forked_session_id"},
	}
	for table, cols := range want {
		got := tableColumns(t, db, table)
		for _, c := range cols {
			if !got[c] {
				t.Errorf("table %s missing column %s (has %v)", table, c, got)
			}
		}
	}

	// 2. 索引就位，且 chat_messages 那条确实是 partial index（空值不受约束）
	for _, name := range []string{"idx_chat_sessions_external_key", "idx_chat_messages_external_msg_id", "idx_sti_root"} {
		if !hasIndex(t, db, name) {
			t.Errorf("index %s not created", name)
		}
	}
	if sql := indexSQL(t, db, "idx_chat_messages_external_msg_id"); !strings.Contains(sql, "WHERE") {
		t.Errorf("idx_chat_messages_external_msg_id is not a partial index: %s", sql)
	}

	// 3. 存量数据一行不丢，且新列为空/默认（不是被填充成错误语义）
	if n := countRows(t, db, "chat_sessions"); n != 1 {
		t.Errorf("chat_sessions rows = %d, want 1", n)
	}
	if n := countRows(t, db, "chat_messages"); n != 2 {
		t.Errorf("chat_messages rows = %d, want 2", n)
	}
	var s ChatSession
	if err := db.First(&s).Error; err != nil {
		t.Fatal(err)
	}
	// session_kind 故意留空而不是 default 'direct'：历史 web 会话不该被贴错标签
	if s.ExternalKey != "" || s.SessionKind != "" || s.ThreadRoot != "" ||
		s.ParentSessionID != nil || s.RootMessageID != nil {
		t.Errorf("legacy session got non-default values: %+v", s)
	}
	var m ChatMessage
	if err := db.Where("content = ?", "迁移前的老消息").First(&m).Error; err != nil {
		t.Fatal(err)
	}
	if m.ExternalMsgID != "" || m.IsContext || m.OriginSessionID != "" || m.ForkedSessionID != "" {
		t.Errorf("legacy message got non-default values: %+v", m)
	}

	// 4. partial 唯一索引生效：同 (bot_id, external_msg_id) 第二条必须失败
	err := db.Exec(`INSERT INTO chat_messages (bot_id, user_id, session_id, role, content, created_at, external_msg_id)
		VALUES ('bot-a','u1','1','user','x','2026-09-02 10:00:00','note-1')`).Error
	if err != nil {
		t.Fatalf("first external_msg_id insert failed: %v", err)
	}
	err = db.Exec(`INSERT INTO chat_messages (bot_id, user_id, session_id, role, content, created_at, external_msg_id)
		VALUES ('bot-a','u1','1','user','y','2026-09-02 10:00:01','note-1')`).Error
	if err == nil || !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Fatalf("duplicate external_msg_id should violate partial unique index, got: %v", err)
	}
	// 空值不受约束：第 2 条 web 消息（无渠道 ID）必须能插入（缺陷 B3）
	if err := db.Exec(`INSERT INTO chat_messages (bot_id, user_id, session_id, role, content, created_at)
		VALUES ('bot-a','u1','1','user','web msg','2026-09-02 10:00:02')`).Error; err != nil {
		t.Fatalf("empty external_msg_id must be allowed more than once: %v", err)
	}
	// 跨 bot 不算冲突
	if err := db.Exec(`INSERT INTO chat_messages (bot_id, user_id, session_id, role, content, created_at, external_msg_id)
		VALUES ('bot-b','u1','1','user','x','2026-09-02 10:00:03','note-1')`).Error; err != nil {
		t.Fatalf("same external_msg_id under another bot must be allowed: %v", err)
	}

	// 5. chat_sessions 的 external_key 唯一约束同理
	if err := db.Exec(`INSERT INTO chat_sessions (bot_id, title, status, created_at, updated_at, external_key)
		VALUES ('bot-a','t','active','2026-09-02 10:00:00','2026-09-02 10:00:00','misskey:thread:abc')`).Error; err != nil {
		t.Fatal(err)
	}
	err = db.Exec(`INSERT INTO chat_sessions (bot_id, title, status, created_at, updated_at, external_key)
		VALUES ('bot-a','t2','active','2026-09-02 10:00:01','2026-09-02 10:00:01','misskey:thread:abc')`).Error
	if err == nil || !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Fatalf("duplicate external_key should violate partial unique index, got: %v", err)
	}
}

// TestSessionThreadIndexTable 验证反向索引表的复合主键与可空 session_id。
func TestSessionThreadIndexTable(t *testing.T) {
	db := newS0TestDB(t)
	if err := db.AutoMigrate(&SessionThreadIndex{}); err != nil {
		t.Fatalf("automigrate session_thread_index: %v", err)
	}

	// session_id 可空 = 该链尚未 fork（Bot 未参与），这是 D9 的落地前提
	row := SessionThreadIndex{BotID: "bot-a", Channel: "misskey", NoteID: "n1", RootNoteID: "root1"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("insert with NULL session_id: %v", err)
	}
	var got SessionThreadIndex
	if err := db.Where("bot_id = ? AND note_id = ?", "bot-a", "n1").First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.SessionID != nil {
		t.Errorf("session_id should be NULL, got %v", *got.SessionID)
	}

	// 复合主键：同 (bot_id, channel, note_id) 第二次必须失败
	if err := db.Create(&SessionThreadIndex{BotID: "bot-a", Channel: "misskey", NoteID: "n1", RootNoteID: "root2"}).Error; err == nil {
		t.Fatal("duplicate (bot_id, channel, note_id) should violate primary key")
	}
	// 同 note 不同 channel 允许
	if err := db.Create(&SessionThreadIndex{BotID: "bot-a", Channel: "telegram", NoteID: "n1", RootNoteID: "root1"}).Error; err != nil {
		t.Fatalf("same note_id under another channel must be allowed: %v", err)
	}

	// fork 后回填 session_id
	sid := uint64(42)
	if err := db.Model(&SessionThreadIndex{}).
		Where("bot_id = ? AND channel = ? AND root_note_id = ?", "bot-a", "misskey", "root1").
		Update("session_id", sid).Error; err != nil {
		t.Fatalf("backfill session_id: %v", err)
	}
	var after SessionThreadIndex
	if err := db.Where("bot_id = ? AND channel = ? AND note_id = ?", "bot-a", "misskey", "n1").First(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after.SessionID == nil || *after.SessionID != 42 {
		t.Errorf("session_id backfill failed: %+v", after)
	}
}
