package dao

import "time"

// IdentityMapping 身份映射（平台用户 → 内部用户）。
// 当用户通过授权码绑定后，系统记录平台身份与内部用户的对应关系。
// 后续来自该平台用户的消息可通过此映射关联到内部用户，用于权限判断等。
type IdentityMapping struct {
	// ID 主键。
	ID uint `gorm:"primaryKey;autoIncrement" json:"id"`

	// UserID 内部用户 ID（关联 users 表）。
	UserID uint `gorm:"index;not null" json:"userId"`

	// Platform 平台类型（"telegram"、"misskey"、"discord" 等）。
	// 由 Source 前缀提取，同一平台的不同 Bot 实例共享同一平台类型。
	Platform string `gorm:"size:64;not null;index:idx_identity_mapping_unique,unique,priority:1" json:"platform"`

	// PlatformUserID 平台侧用户 ID。
	// Telegram: 数字 ID 字符串；Misskey: 用户 ID 字符串等。
	PlatformUserID string `gorm:"size:128;not null;index:idx_identity_mapping_unique,unique,priority:2" json:"platformUserId"`

	// CreatedAt 创建时间。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`

	// UpdatedAt 更新时间。
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

// TableName 指定 GORM 表名。
func (IdentityMapping) TableName() string { return "identity_mappings" }
