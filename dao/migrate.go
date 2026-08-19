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
		&UserMessageEvent{},
		&BindCode{},
		&IdentityMapping{},
		&TieredMemoryModel{},
		&BotToolPermission{},
		&BotBrowserCookie{},
	); err != nil {
		return err
	}
	// GORM AutoMigrate 在 SQLite 存量表上不会 ALTER 加列（仅新建表时建列），
	// 因此存量表的新增列需手动补齐，否则写入会报 “no such column”。
	// 此处幂等：列已存在则跳过。
	return ensureColumns(database)
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
		{"chat_messages", "session_id", "session_id TEXT NOT NULL DEFAULT ''"},
		{"bot_tool_permissions", "auto", "auto INTEGER NOT NULL DEFAULT 0"},
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

// isDuplicateColumnErr 判断错误是否为 “重复列”（SQLite: duplicate column name: xxx）。
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}
