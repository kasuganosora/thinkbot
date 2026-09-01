package dao

import "time"

// WorkflowUsage 工作流节点维度的**逐条**调用明细。
//
// 与 UsageDaily 的分工（刻意分开，不要合并）：
//
//	UsageDaily     —— (bot, model, feature, channel, date) 五维**日聚合**，
//	                  有唯一索引，行数随维度基数增长。回答「今天花了多少」。
//	WorkflowUsage  —— 逐条明细，每条带 workflow_id / node_id。
//	                  回答「这条工作流花在哪、哪个节点最贵」。
//
// 为什么不能把 workflow/node 塞进 UsageDaily：
//   1. 日聚合表会被撑成明细表（每条工作流每节点每天一行），行数爆炸；
//   2. 五列唯一索引要扩成七列，存量行的空 workflow_id 会与新数据冲突；
//   3. 不感知新维度的既有聚合查询会被拆散，按日统计结果失真。
//
// 两表通过 bot_id + created_at 关联，口径一致便于对账。
type WorkflowUsage struct {
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime;index" json:"createdAt"`

	// 归因维度
	WorkflowID string `gorm:"column:workflow_id;size:64;not null;index:idx_wf_usage_wf" json:"workflowId"`
	NodeID     string `gorm:"column:node_id;size:64;not null" json:"nodeId"`
	BotID      string `gorm:"column:bot_id;size:255;index" json:"botId"`
	Model      string `gorm:"column:model;size:255" json:"model"`
	Feature    string `gorm:"column:feature;size:100" json:"feature"`

	// Token 明细（与 UsageDaily 口径一致，便于对账）
	InputTokens      int `gorm:"column:input_tokens;default:0" json:"inputTokens"`
	OutputTokens     int `gorm:"column:output_tokens;default:0" json:"outputTokens"`
	TotalTokens      int `gorm:"column:total_tokens;default:0" json:"totalTokens"`
	CacheReadTokens  int `gorm:"column:cache_read_tokens;default:0" json:"cacheReadTokens"`
	CacheWriteTokens int `gorm:"column:cache_write_tokens;default:0" json:"cacheWriteTokens"`

	// 编排指标
	ToolCalls int `gorm:"column:tool_calls;default:0" json:"toolCalls"`
	Steps     int `gorm:"column:steps;default:0" json:"steps"`
}

// TableName 指定表名。
func (WorkflowUsage) TableName() string {
	return "workflow_usage"
}
