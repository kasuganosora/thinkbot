// Package storage 提供 agent 模块的持久化层实现（SQLite/GORM）。
//
// 设计原则：
//   - 基础设施层：只依赖领域接口（memory.Repository），不反向依赖
//   - DDD 端口-适配器模式：本包是适配器，领域层定义端口（接口）
//   - 一个 DB 连接服务所有持久化需求，减少资源开销
//   - 所有 model 使用 GORM 约定，通过 AutoMigrate 自动建表
//   - 对外暴露 NewXxxRepository 工厂函数，返回的类型实现领域接口
package storage

import (
	"time"

	"gorm.io/gorm"
)

// ============================================================================
// GORM Models — 数据库表结构
// ============================================================================

// EntryModel 记忆条目表。
// 对应领域模型 memory.Entry。
// 统一存储所有来源的记忆：对话提取（source="conversation"）、
// Bot 自主备注（source="note"）、系统注入（source="system"）等。
type EntryModel struct {
	ID        string `gorm:"primaryKey;size:64"`
	ScopeKind string `gorm:"size:32;not null;index:idx_scope"`
	ScopeID   string `gorm:"size:128;index:idx_scope"`

	Content    string  `gorm:"type:text;not null"`
	Category   string  `gorm:"size:64;index:idx_category"`
	Source     string  `gorm:"size:64;index:idx_source"`
	Importance float64 `gorm:"default:0"`

	// Metadata 以 JSON 文本存储。
	MetadataJSON string `gorm:"type:text"`

	CreatedAt      time.Time `gorm:"not null;index:idx_created"`
	LastAccessedAt time.Time `gorm:"index:idx_accessed"`
}

// TableName 指定表名。
func (EntryModel) TableName() string {
	return "memory_entries"
}

// ScopeKey 返回 scope 分桶键（与 memory.Scope.Key() 一致）。
func (m *EntryModel) ScopeKey() string {
	if m.ScopeID == "" {
		return m.ScopeKind
	}
	return m.ScopeKind + ":" + m.ScopeID
}

// WindowStateModel 上下文窗口状态表。
// 按 scope 存储，每个 scope 一行（upsert 语义）。
type WindowStateModel struct {
	ScopeKey string `gorm:"primaryKey;size:256"`

	UsedTokens        int   `gorm:"not null;default:0"`
	RoundCount        int   `gorm:"not null;default:0"`
	TotalInputTokens  int64 `gorm:"not null;default:0"`
	TotalOutputTokens int64 `gorm:"not null;default:0"`
	Compressions      int64 `gorm:"not null;default:0"`

	UpdatedAt time.Time `gorm:"not null"`
}

// TableName 指定表名。
func (WindowStateModel) TableName() string {
	return "window_states"
}

// ============================================================================
// Migration
// ============================================================================

// Migrate 执行所有 agent 存储层的数据库迁移。
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&EntryModel{},
		&WindowStateModel{},
	)
}
