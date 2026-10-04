package dao

import "time"

// MemoryTombstoneModel records that a statement was deliberately deleted.
// Dreaming and consolidation must not write that statement back.
// The row keeps a fingerprint, not a second copy of the text.
type MemoryTombstoneModel struct {
	ID          string    `gorm:"primaryKey;size:64"`
	ScopeKey    string    `gorm:"size:191;not null;index:idx_tomb_scope_fp,priority:1"`
	Fingerprint string    `gorm:"size:64;not null;index:idx_tomb_scope_fp,priority:2"`
	Topic       string    `gorm:"size:128"`
	Source      string    `gorm:"size:128"`
	CreatedAt   time.Time `gorm:"index"`
}

func (MemoryTombstoneModel) TableName() string { return "memory_tombstones" }
