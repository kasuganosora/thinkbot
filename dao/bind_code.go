package dao

import "time"

// BindCode 授权绑定码。
// 用户在 Web 页面生成一次性码，然后在 Telegram/Misskey 等非 Web 渠道发送该码，
// 完成平台身份与内部用户的绑定。
type BindCode struct {
	// ID 主键。
	ID uint `gorm:"primaryKey;autoIncrement" json:"id"`

	// UserID 内部用户 ID（关联 users 表）。
	UserID uint `gorm:"index;not null" json:"userId"`

	// Code 授权码（格式 TB-XXXX-XXXX，唯一）。
	Code string `gorm:"uniqueIndex;size:32;not null" json:"code"`

	// UsedAt 使用时间（nil 表示未使用）。
	// 一次性码：使用后非 nil，不可重复使用。
	UsedAt *time.Time `json:"usedAt,omitempty"`

	// ExpiresAt 过期时间。
	// 生成时设为 CreatedAt + 5 分钟。
	ExpiresAt time.Time `gorm:"not null" json:"expiresAt"`

	// CreatedAt 创建时间。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`
}

// TableName 指定 GORM 表名。
func (BindCode) TableName() string { return "bind_codes" }
