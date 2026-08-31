package dao

import "time"

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
