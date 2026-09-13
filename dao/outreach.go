package dao

import "time"

// ============================================================================
// Outreach — 对人主动开口的结构化承诺 / 对账 / 最近互动
// ============================================================================

// OutreachCommitment 是「该不该对某人开口」的唯一真相。
// 由 remind 工具写入，轮询评估器只扫到期的 pending 行，不靠 LLM 猜。
type OutreachCommitment struct {
	ID                string     `gorm:"primaryKey;size:64" json:"id"`
	BotID             string     `gorm:"size:64;not null;index:idx_outreach_due,priority:1" json:"botId"`
	Kind              string     `gorm:"size:16;not null" json:"kind"` // reminder | watch
	IdentityKey       string     `gorm:"size:160;not null;index:idx_outreach_ident" json:"identityKey"`
	UserID            string     `gorm:"size:128;not null" json:"userId"`
	Channel           string     `gorm:"size:128;not null" json:"channel"`    // 渠道实例名，dispatcher 路由用
	ChannelType       string     `gorm:"size:32;not null" json:"channelType"` // web | telegram | misskey
	ConversationID    string     `gorm:"size:128;default:''" json:"conversationId"`
	SessionID         string     `gorm:"size:64;default:''" json:"sessionId"`
	DueAt             time.Time  `gorm:"not null;index:idx_outreach_due,priority:3" json:"dueAt"`
	Topic             string     `gorm:"size:256;not null" json:"topic"`
	Context           string     `gorm:"type:text" json:"context"`
	Status            string     `gorm:"size:16;not null;index:idx_outreach_due,priority:2" json:"status"`
	Attempts          int        `gorm:"not null;default:0" json:"attempts"`
	CreatedAt         time.Time  `gorm:"not null" json:"createdAt"`
	DeliveredAt       *time.Time `json:"deliveredAt,omitempty"`
	DeliveredRecordID string     `gorm:"size:64;default:''" json:"deliveredRecordId"`
}

func (OutreachCommitment) TableName() string { return "outreach_commitments" }

// OutreachRecord 是一次主动开口尝试的对账记录（含静默 / 被闸门跳过）。
type OutreachRecord struct {
	ID           string    `gorm:"primaryKey;size:64" json:"id"`
	BotID        string    `gorm:"size:64;not null;index:idx_outreach_rec,priority:1" json:"botId"`
	IdentityKey  string    `gorm:"size:160;not null;index:idx_outreach_quota,priority:2" json:"identityKey"`
	CommitmentID string    `gorm:"size:64;default:'';index" json:"commitmentId"`
	Trigger      string    `gorm:"size:16;not null;default:'none'" json:"trigger"` // reminder | watch | none
	Reason       string    `gorm:"type:text" json:"reason"`
	Content      string    `gorm:"type:text" json:"content"`
	ChannelType  string    `gorm:"size:32;not null;default:'';index:idx_outreach_quota,priority:3" json:"channelType"`
	Status       string    `gorm:"size:32;not null;index:idx_outreach_rec,priority:2" json:"status"`
	CostSec      float64   `gorm:"not null;default:0" json:"costSec"`
	TraceID      string    `gorm:"size:128;default:''" json:"traceId"`
	CreatedAt    time.Time `gorm:"not null;index:idx_outreach_quota,priority:4" json:"createdAt"`
}

func (OutreachRecord) TableName() string { return "outreach_records" }

// OutreachLastSeen 记录某身份在某平台最近一次真实入站时间（静默窗用）。
type OutreachLastSeen struct {
	BotID       string    `gorm:"size:64;primaryKey" json:"botId"`
	IdentityKey string    `gorm:"size:160;primaryKey" json:"identityKey"`
	ChannelType string    `gorm:"size:32;primaryKey" json:"channelType"`
	LastInbound time.Time `gorm:"not null" json:"lastInbound"`
}

func (OutreachLastSeen) TableName() string { return "outreach_last_seen" }

// 承诺状态。
const (
	OutreachPending   = "pending"
	OutreachDelivered = "delivered"
	OutreachCancelled = "cancelled"
	OutreachExpired   = "expired"
	OutreachFailed    = "failed"
)

// 承诺种类。
const (
	OutreachKindReminder = "reminder"
	OutreachKindWatch    = "watch"
)
