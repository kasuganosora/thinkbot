package dao

import "time"

// ChatSession 聊天会话记录。
//
// 每个 Bot 下可以有多个独立会话（对话线程），
// 用户可以在会话间切换，每个会话维护独立的上下文。
//
// 与 ChatMessage 的关系：
//   - ChatMessage.SessionID 外键关联到 ChatSession.ID
//   - 删除会话时级联删除该会话下的所有消息
type ChatSession struct {
	ID           uint64     `gorm:"primaryKey;autoIncrement" json:"id"`
	BotID        string     `gorm:"size:64;not null;index:idx_session_bot,priority:1" json:"botId"`
	Title        string     `gorm:"size:256;default:'新会话'" json:"title"`                   // 会话标题（默认取首条用户消息前 30 字）
	Status       string     `gorm:"size:16;not null;default:'active';index" json:"status"` // active / archived
	MessageCount int        `gorm:"not null;default:0" json:"messageCount"`
	LastMsgAt    *time.Time `gorm:"index:idx_session_bot,priority:2,sort:desc" json:"lastMsgAt,omitempty"` // 最后一条消息时间
	CreatedAt    time.Time  `gorm:"not null" json:"createdAt"`
	UpdatedAt    time.Time  `gorm:"not null" json:"updatedAt"`
}

// TableName 指定 GORM 表名。
func (ChatSession) TableName() string { return "chat_sessions" }

// Session 状态常量。
const (
	SessionStatusActive   = "active"
	SessionStatusArchived = "archived"
)
