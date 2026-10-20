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

	// DedupCheckedAt 记忆去重（memory_dedup）最近一次确认该条目「无可合并项」的时间。
	// NULL = 尚未检查：去重只处理未检查条目（及其近邻），已检查条目不会被反复送进 LLM。
	// 存量表由 ensureColumns 补列（migration: memory_entries.dedup_checked_at）。
	DedupCheckedAt *time.Time
}

// MemoryDigestState 记录被动收听 scope（如 misskey:timeline）每日摘要的进度：
// LastDate 为已完成摘要的最后一个本地日期（YYYY-MM-DD），保证重跑不重复。
// ScopeKey 形如 "<bot_id>|channel:misskey:timeline"（同一 scope 可被多个 bot 收听）。
type MemoryDigestState struct {
	ScopeKey  string    `gorm:"primaryKey;size:250"`
	LastDate  string    `gorm:"size:10;not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

// TableName 指定表名。
func (MemoryDigestState) TableName() string { return "memory_digest_states" }

// MemoryCompactState 记录每个记忆 scope 上次自动去重的时间。
// 冷却持久化到 DB，进程重启不再重置（此前仅为内存 sync.Map）。
type MemoryCompactState struct {
	ScopeKey  string    `gorm:"primaryKey;size:200"`
	LastRunAt time.Time `gorm:"not null"`
}

// TableName 指定表名。
func (MemoryCompactState) TableName() string { return "memory_compact_states" }

// TableName 指定表名。
func (EntryModel) TableName() string {
	return "memory_entries"
}
