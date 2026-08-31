package dao

import "time"

// User 用户表。
type User struct {
	// ID 主键。
	ID uint `gorm:"primaryKey" json:"id"`

	// Username 用户名（唯一）。
	Username string `gorm:"uniqueIndex;size:64;not null" json:"username"`

	// Email 邮箱（可选）。
	// 唯一性由应用层验证（空值不参与唯一约束）。
	Email string `gorm:"index;size:255;default:''" json:"email"`

	// PasswordHash bcrypt 密码哈希。
	PasswordHash string `gorm:"size:255;not null" json:"-"`

	// Role 角色：admin | member。
	Role string `gorm:"size:32;not null;default:'member'" json:"role"`

	// Status 状态：active | disabled。
	Status string `gorm:"size:32;not null;default:'active'" json:"status"`

	// DisplayName 显示名称（可选）。
	DisplayName string `gorm:"size:128;default:''" json:"displayName"`

	// Avatar 头像 URL（可选）。
	Avatar string `gorm:"size:512;default:''" json:"avatar"`

	// LastLoginAt 最后登录时间。
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`

	// CreatedAt 创建时间。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`

	// UpdatedAt 更新时间。
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

// TableName 指定 GORM 表名。
func (User) TableName() string { return "users" }
