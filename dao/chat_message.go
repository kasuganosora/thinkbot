package dao

import "time"

// ChatMessage 持久化的聊天消息记录。
// 记录 Web 聊天中的每一条用户消息和 Bot 回复，支持游标分页查询。
//
// 索引设计：复合索引 (bot_id, user_id, created_at DESC) 覆盖
// 游标分页查询的 WHERE + ORDER BY，时间复杂度 O(log N + page_size)。
type ChatMessage struct {
	// ID 自增主键（游标的一部分，用于同时间戳的消息排序）。
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"id"`

	// BotID Bot 标识。
	BotID string `gorm:"size:64;not null;index:idx_chat_session_time,priority:1" json:"botId"`

	// UserID 用户标识。
	UserID string `gorm:"size:64;not null;index:idx_chat_session_time,priority:2" json:"userId"`

	// Role 消息角色："user" 或 "assistant"。
	Role string `gorm:"size:16;not null" json:"role"`

	// Content 消息内容。
	Content string `gorm:"type:text" json:"content"`

	// TraceID 追踪 ID，用于关联请求-回复对。
	TraceID string `gorm:"size:128" json:"traceId"`

	// CreatedAt 创建时间（游标的主排序键）。
	CreatedAt time.Time `gorm:"not null;index:idx_chat_session_time,priority:3,sort:desc" json:"createdAt"`
}

// TableName 指定 GORM 表名。
func (ChatMessage) TableName() string { return "chat_messages" }

// 消息角色常量。
const (
	ChatRoleUser      = "user"
	ChatRoleAssistant = "assistant"
)
