package dao

import "time"

// ContextCheckpoint 是一次「上下文压缩」的持久化检查点（compact_context 工具产出）。
//
// 语义：同一 (bot_id, session_id) 下 id <= BoundaryMessageID 的 chat_messages 行
// 在构建 LLM 上下文时由 Summary 代替；原始行**不删除**（前端历史、审计照常可见），
// 因而可逆：把 Active 置 false（或删除本行）即恢复为完整历史。
// 同一会话可有多条检查点，仅最新一条 Active 生效（新检查点写入时会停用旧的）。
type ContextCheckpoint struct {
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"id"`

	BotID     string `gorm:"size:64;not null;index:idx_ctx_ckpt_session,priority:1" json:"botId"`
	SessionID string `gorm:"size:128;not null;index:idx_ctx_ckpt_session,priority:2" json:"sessionId"`

	// BoundaryMessageID chat_messages.id 边界（含）：该 id 及更早的行已被摘要覆盖。
	BoundaryMessageID uint64 `gorm:"not null;default:0" json:"boundaryMessageId"`

	// Summary 结构化摘要正文。
	Summary string `gorm:"type:text;not null" json:"summary"`
	// Focus 模型要求摘要必须保留的要点（可空）。
	Focus string `gorm:"type:text;default:''" json:"focus"`

	// 统计信息（估算值），便于审计。
	CompactedMessages int `gorm:"not null;default:0" json:"compactedMessages"`
	TokensBefore      int `gorm:"not null;default:0" json:"tokensBefore"`
	TokensAfter       int `gorm:"not null;default:0" json:"tokensAfter"`

	// Source 产生来源（如 "compact_context"）。
	Source string `gorm:"size:64;default:''" json:"source"`

	// Active 是否生效。停用即回滚到完整历史。
	Active bool `gorm:"not null;default:true;index" json:"active"`

	CreatedAt time.Time `gorm:"not null" json:"createdAt"`
}

// TableName 指定 GORM 表名。
func (ContextCheckpoint) TableName() string { return "context_checkpoints" }
