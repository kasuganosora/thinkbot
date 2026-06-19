package config

import "time"

// Setting 是配置持久化的数据库模型。
// 每个配置项存储为一行记录，包含值及前端渲染所需的元数据。
type Setting struct {
	// Key 配置键名（如 "llm.openai.api_key"），主键。
	Key string `gorm:"primaryKey;size:255" json:"key"`

	// Value 配置值（字符串形式存储）。
	Value string `gorm:"type:text" json:"value"`

	// Category 分类，用于前端分组展示（如 "LLM", "Bot", "Database"）。
	Category string `gorm:"size:100;default:''" json:"category"`

	// Description 描述文本，用于前端渲染设置界面的帮助说明。
	Description string `gorm:"type:text;default:''" json:"description"`

	// UpdatedAt 最后更新时间。
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

// TableName 指定 GORM 表名。
func (Setting) TableName() string { return "config_settings" }
