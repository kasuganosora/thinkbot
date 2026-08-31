package dao

import "time"

// ChannelDefinition 持久化的 Channel 配置定义。
// 每个 Bot 可以关联多个 Channel（如 Telegram、Misskey），管理员通过 API 配置。
// Config 字段存储 Channel 类型特有的配置 JSON（不同 Channel 类型字段不同）。
type ChannelDefinition struct {
	// ID 自增主键。
	ID uint `gorm:"primaryKey;autoIncrement" json:"id"`

	// BotID 所属 Bot ID。
	BotID string `gorm:"size:64;index;not null" json:"botId"`

	// Name Channel 实例名称（如 "telegram-cs-bot"）。
	Name string `gorm:"size:128;not null" json:"name"`

	// Type Channel 类型标识（"telegram"、"misskey" 等）。
	Type string `gorm:"size:32;not null" json:"type"`

	// Config Channel 类型特有的配置 JSON 字符串。
	Config string `gorm:"type:text" json:"config"`

	// Enabled 是否启用。false 时 StartBot 不会实例化此 Channel。
	Enabled bool `gorm:"default:true" json:"enabled"`

	// CreatedAt 创建时间。
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`

	// UpdatedAt 更新时间。
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

// TableName 指定 GORM 表名。
func (ChannelDefinition) TableName() string { return "channel_definitions" }
