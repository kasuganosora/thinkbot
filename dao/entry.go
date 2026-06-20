package dao

import "time"

// EntryModel 记忆条目表。
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
