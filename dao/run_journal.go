package dao

import "time"

// RunJournal 记录单次 LLM 调用的完整快照，用于调试和成本审计。
// 每条记录对应一次 LLM generate 请求（包括多步 orchestration 中的每一步）。
type RunJournal struct {
	ID uint `gorm:"primaryKey;autoIncrement" json:"id"`

	// 追踪维度
	TraceID   string `gorm:"column:trace_id;size:64;not null;index" json:"traceId"`
	RunID     string `gorm:"column:run_id;size:64;not null;index" json:"runId"`
	BotID     string `gorm:"column:bot_id;size:255;not null;index" json:"botId"`
	Channel   string `gorm:"column:channel;size:255" json:"channel"`
	UserID    string `gorm:"column:user_id;size:255" json:"userId"`
	MessageID string `gorm:"column:message_id;size:64" json:"messageId"`

	// LLM 调用维度
	Model   string `gorm:"column:model;size:255" json:"model"`
	Feature string `gorm:"column:feature;size:100" json:"feature"`
	Step    int    `gorm:"column:step;default:0" json:"step"`
	Caller  string `gorm:"column:caller;size:100;default:'lead_agent'" json:"caller"`

	// Token 用量
	InputTokens     int `gorm:"column:input_tokens;default:0" json:"inputTokens"`
	OutputTokens    int `gorm:"column:output_tokens;default:0" json:"outputTokens"`
	TotalTokens     int `gorm:"column:total_tokens;default:0" json:"totalTokens"`
	CacheReadTokens int `gorm:"column:cache_read_tokens;default:0" json:"cacheReadTokens"`

	// 工具调用统计
	ToolCalls int `gorm:"column:tool_calls;default:0" json:"toolCalls"`

	// 编排步数
	Steps int `gorm:"column:steps;default:0" json:"steps"`

	// 耗时（毫秒）
	LatencyMs int64 `gorm:"column:latency_ms;default:0" json:"latencyMs"`

	// 状态
	Status string `gorm:"column:status;size:50;default:'success'" json:"status"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime" json:"createdAt"`
}

// TableName 指定表名。
func (RunJournal) TableName() string { return "run_journal" }
