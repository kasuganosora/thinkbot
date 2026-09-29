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

	// ---- 以下为会话划分（S0）新增列，S0 阶段**不参与任何逻辑**，仅建列/索引 ----
	// 全部 `json:"-"`：S0 要求「零行为变化」，因此不对既有 API 响应暴露新字段；
	// S1/S2 启用时按需逐个放开。

	// ExternalKey 三段式外部标识 `<channel>:<kind>:<id...>`，例如
	// `telegram:chat:76017910` / `misskey:thread:<rootNoteID>` / `misskey:timeline`。
	// ⚠️ 与 core.Message.Channel（记忆 scope，裸 chatID / userID）**必须解耦**：
	// 前者是展示侧的会话空间（thread 级），后者是记忆 scope（user 级）。
	// 空串 = Web 会话等无外部标识的会话（不进 partial 唯一索引，见 migrate.go）。
	ExternalKey string `gorm:"size:256;not null;default:''" json:"-"`

	// SessionKind 会话类型：direct / group / topic / thread / timeline / web / notify。
	// 空串 = 未分类（历史数据），由 S1 的 SessionResolver 回填。
	// ⚠️ 不用 default 'direct'：历史 web 会话会被贴上错误标签，宁可留空。
	//
	// 已对 API 放开（S1）：Web 列表靠它区分 TG 私聊 / TG 群 / Misskey 时间线，
	// 否则它们在列表里长得一模一样。
	SessionKind string `gorm:"size:16;not null;default:''" json:"sessionKind,omitempty"`

	// ThreadRoot thread 类会话的根帖/根消息 ID（渠道侧字符串 ID，非 chat_messages.id），
	// 供反查与归并；非 thread 类会话为空串。
	ThreadRoot string `gorm:"size:128;not null;default:''" json:"-"`

	// ParentSessionID fork 来源会话（thread 的父 = timeline）。可空 = 非 fork 产生。
	ParentSessionID *uint64 `json:"-"`

	// RootMessageID thread 引用的 root 在 chat_messages 里的行 ID。
	// root 按 I2 原位留在主站，thread 视图靠它引用（不落库副本）。可空。
	RootMessageID *uint64 `json:"-"`
}

// TableName 指定 GORM 表名。
func (ChatSession) TableName() string { return "chat_sessions" }

// Session 状态常量。
const (
	SessionStatusActive   = "active"
	SessionStatusArchived = "archived"
)
