package dao

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// Migrate 执行所有数据库表的自动迁移。
func Migrate(database *gorm.DB) error {
	if err := database.AutoMigrate(
		&User{},
		&Setting{},
		&WorkflowModel{},
		&UsageDaily{},
		&EntryModel{},
		&WindowStateModel{},
		&BotDefinition{},
		&ChannelDefinition{},
		&ChatSession{},
		&ChatMessage{},
		&SessionThreadIndex{},
		&UserMessageEvent{},
		&BindCode{},
		&IdentityMapping{},
		&TieredMemoryModel{},
		&MemoryTombstoneModel{},
		&BotToolPermission{},
		&BotBrowserCookie{},
		&JudgeRecord{},
		&WorkflowUsage{},
		&OutreachCommitment{},
		&OutreachRecord{},
		&OutreachLastSeen{},
		&ContextCheckpoint{},
		&NotifyToken{},
		&NotifyEvent{},
	); err != nil {
		return err
	}
	// GORM AutoMigrate 在 SQLite 存量表上不会 ALTER 加列（仅新建表时建列），
	// 因此存量表的新增列需手动补齐，否则写入会报 “no such column”。
	// 此处幂等：列已存在则跳过。
	if err := ensureColumns(database); err != nil {
		return err
	}
	// 索引必须最后建：partial index 的 WHERE 子句引用的是上面补齐的列，
	// 列不存在时建索引会直接失败。
	if err := ensureIndexes(database); err != nil {
		return err
	}
	// 一次性数据修复（依赖上面补齐的列），每项只执行一次，见 data_migration.go。
	return runDataMigrations(database)
}

// columnSpec 描述一个需要补齐的存量表列。
type columnSpec struct {
	table  string // 真实表名（GORM 复数化后的名称）
	column string
	ddl    string // 完整列定义，如 "max_steps INTEGER NOT NULL DEFAULT 0"
}

// ensureColumns 幂等地为存量表补齐 AutoMigrate 未自动添加的列。
func ensureColumns(db *gorm.DB) error {
	specs := []columnSpec{
		{"bot_definitions", "max_steps", "max_steps INTEGER NOT NULL DEFAULT 0"},
		{"bot_definitions", "hard_max_steps", "hard_max_steps INTEGER NOT NULL DEFAULT 0"},
		{"bot_definitions", "memory_limit_mb", "memory_limit_mb INTEGER NOT NULL DEFAULT 2048"},
		{"bot_definitions", "cost_quota", "cost_quota TEXT NOT NULL DEFAULT ''"},
		{"chat_messages", "session_id", "session_id TEXT NOT NULL DEFAULT ''"},
		{"bot_tool_permissions", "auto", "auto INTEGER NOT NULL DEFAULT 0"},
		{"bot_tool_permissions", "chat_id", "chat_id TEXT NOT NULL DEFAULT ''"},
		{"outreach_commitments", "attempts", "attempts INTEGER NOT NULL DEFAULT 0"},
		{"stats_usage_daily", "cost_input", "cost_input REAL NOT NULL DEFAULT 0"},
		{"stats_usage_daily", "cost_output", "cost_output REAL NOT NULL DEFAULT 0"},
		{"stats_usage_daily", "cost_total", "cost_total REAL NOT NULL DEFAULT 0"},

		// —— 会话划分（S0）——
		// 全部带 DEFAULT 或可空：SQLite 的 ALTER TABLE ADD COLUMN 不允许
		// 无默认值的 NOT NULL 列，存量表加列会直接失败。
		{"chat_sessions", "external_key", "external_key TEXT NOT NULL DEFAULT ''"},
		{"chat_sessions", "session_kind", "session_kind TEXT NOT NULL DEFAULT ''"},
		{"chat_sessions", "thread_root", "thread_root TEXT NOT NULL DEFAULT ''"},
		{"chat_sessions", "parent_session_id", "parent_session_id INTEGER"},
		{"chat_sessions", "root_message_id", "root_message_id INTEGER"},
		{"chat_messages", "external_msg_id", "external_msg_id TEXT NOT NULL DEFAULT ''"},
		{"chat_messages", "is_context", "is_context INTEGER NOT NULL DEFAULT 0"},
		{"chat_messages", "origin_session_id", "origin_session_id TEXT NOT NULL DEFAULT ''"},
		{"chat_messages", "forked_session_id", "forked_session_id TEXT NOT NULL DEFAULT ''"},
	}
	for _, s := range specs {
		var cnt int64
		// 表名是常量、非外部输入，用 Sprintf 拼接；列名用参数占位避免引号问题。
		q := fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name = ?", s.table)
		if err := db.Raw(q, s.column).Scan(&cnt).Error; err != nil {
			return err
		}
		if cnt > 0 {
			continue
		}
		if err := db.Exec(
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", s.table, s.ddl),
		).Error; err != nil {
			// 并发/重复迁移时可能出现 “duplicate column”，幂等忽略。
			if isDuplicateColumnErr(err) {
				continue
			}
			return err
		}
	}
	return nil
}

// indexSpec 描述一个需要补齐的索引（含 partial index）。
type indexSpec struct {
	name string // 索引名，仅用于报错时定位
	ddl  string // 完整 CREATE [UNIQUE] INDEX 语句，必须自带 IF NOT EXISTS（幂等）
}

// sessionIndexSpecs 会话划分（S0）需要的索引。
//
// ⚠️ 两张表都用 **partial index**（`WHERE col <> ”`）而不是普通唯一索引：
// Web 会话消息没有渠道 ID（`external_msg_id` 恒为空串），普通唯一索引会让
// 第 2 条 web 消息插入失败；partial index 直接把空值排除在索引之外（缺陷 B3）。
//
// ⚠️ chat_sessions 没有独立的 channel 列（原方案写的 `(bot_id, channel, external_key)`
// 不成立），改用 `(bot_id, external_key)` —— 三段式 key 的**第一段就是 channel**，
// 语义等价且不需要新增列。
var sessionIndexSpecs = []indexSpec{
	{
		name: "idx_chat_sessions_external_key",
		ddl:  `CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_sessions_external_key ON chat_sessions (bot_id, external_key) WHERE external_key <> ''`,
	},
	{
		// I1「一条 inbound 只落一行」的数据库级保障（缺陷 B2）。
		name: "idx_chat_messages_external_msg_id",
		ddl:  `CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_messages_external_msg_id ON chat_messages (bot_id, external_msg_id) WHERE external_msg_id <> ''`,
	},
	{
		// reclaim / 入站归属都按根帖枚举子树，无此索引会全表扫（缺陷 B11）。
		name: "idx_sti_root",
		ddl:  `CREATE INDEX IF NOT EXISTS idx_sti_root ON session_thread_index (bot_id, channel, root_note_id)`,
	},
}

// ensureIndexes 幂等地补齐索引。
//
// 与 ensureColumns 分离：DDL 依赖的列必须先由 ensureColumns 建好，
// 且 GORM AutoMigrate 既不认识 partial index、也不会给存量表补索引。
func ensureIndexes(db *gorm.DB) error {
	for _, s := range sessionIndexSpecs {
		if err := db.Exec(s.ddl).Error; err != nil {
			return fmt.Errorf("create index %s: %w", s.name, err)
		}
	}
	return nil
}

// isDuplicateColumnErr 判断错误是否为 “重复列”（SQLite: duplicate column name: xxx）。
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}
