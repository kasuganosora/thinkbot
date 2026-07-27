package dao

import "time"

// TieredMemoryModel 分层记忆条目表。
// 持久化 agent/memory.TieredStore 中的记忆（L0 工作 / L1 长期 / L2 场景 / L3 画像），
// 使分层记忆在进程重启后仍能恢复，避免“运行几天但记忆只在内存里、重启即失”。
type TieredMemoryModel struct {
	ID         string `gorm:"primaryKey;size:64"`
	Tier       int    `gorm:"not null;index:idx_tier"`
	ScopeKind  string `gorm:"size:32;not null;index:idx_tscope"`
	ScopeID    string `gorm:"size:128;index:idx_tscope"`
	Content    string `gorm:"type:text;not null"`
	Category   string `gorm:"size:64;index:idx_tcat"`
	Source     string `gorm:"size:64"`
	Importance float64

	// Metadata 以 JSON 文本存储。
	MetadataJSON string `gorm:"type:text"`

	// ExpiresAt L0 条目的过期时间，零值表示不过期。
	ExpiresAt time.Time `gorm:"index:idx_texpires"`
	// PromotedFrom 标记此条目从哪个层级提升而来（如 L0→L1）。
	PromotedFrom int

	CreatedAt      time.Time `gorm:"not null;index:idx_tcreated"`
	LastAccessedAt time.Time `gorm:"index:idx_taccessed"`
}

// TableName 指定表名。
func (TieredMemoryModel) TableName() string {
	return "tiered_memories"
}
