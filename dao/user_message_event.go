package dao

import "time"

// UserMessageEvent 是「入站用户消息事件流」的持久化记录。
//
// 对应 deepseek-harness 的 append-only trajectory / session event stream 中
// 「用户说了什么」这一类事件。它是 dreaming 回灌（backfill）的权威数据源，
// 取代此前直接扫 chat_messages 的做法，使记忆回灌与原始渠道存储解耦
// （根治「清空 tiered/memory 表后从 chat_messages 回潮」的陷阱）。
//
// 写入时机：
//   - 运行期：NoteCaptureMiddleware 在摄取用户入站原文时并行写入（见 agent/stages/note_capture.go）。
//   - 历史补齐：首次部署时由 memory.SeedUserMessageEvents 从 chat_messages 一次性幂等补齐。
//
// 读取时机：memory.BackfillFromChatHistory 经 UserMessageSource 消费本表（带 id 水位线）。
type UserMessageEvent struct {
	// ID 自增主键，兼作回灌水位线（max id = 已处理边界）。
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"id"`

	// BotID Bot 标识。
	BotID string `gorm:"size:64;not null;index:idx_ume_bot,priority:1" json:"botId"`

	// Channel 渠道标识（记忆关联用；历史补齐时可能为空）。
	Channel string `gorm:"size:128" json:"channel"`

	// UserID 用户标识。
	UserID string `gorm:"size:128" json:"userId"`

	// MessageID 原始消息 ID（去重 / 追溯用；历史补齐时取 chat_messages 主键）。
	MessageID string `gorm:"size:128" json:"messageId"`

	// Content 用户消息原文。
	Content string `gorm:"type:text" json:"content"`

	// CreatedAt 摄取时间（游标主排序键）。
	CreatedAt time.Time `gorm:"not null;index:idx_ume_bot,priority:2,sort:asc" json:"createdAt"`
}

// TableName 指定 GORM 表名。
func (UserMessageEvent) TableName() string { return "user_message_events" }
