package dao

import "time"

// JudgeRecord 一次 LLM 快判（Tier 2 judge）的落库记录。
//
// 背景：agent/engagement 的 LLMJudge 每次判定只用于派生「参不参与」的决策，
// 结果用完即弃——改了 prompt、换了模型，无从判断变好还是变坏。
// 本表把判定结果持久化，使参与决策的质量可观测。
//
// 与 UsageDaily 的关系：那是 (bot, model, feature, channel, date) 的**日聚合**表，
// 记录 token / 请求数；本表是**逐条明细**，记录判定语义（engage / score / reason）。
// 两者维度不同、用途不同，刻意分开——把判定结果塞进 UsageDaily 会破坏它的
// 聚合粒度（每条判定一行，日表被撑成明细表）。
type JudgeRecord struct {
	ID        uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime;index" json:"createdAt"`

	// 维度
	BotID   string `gorm:"column:bot_id;size:255;not null;index" json:"botId"`
	Channel string `gorm:"column:channel;size:100;not null;default:''" json:"channel"`
	// Feature 判定来源，如 "engagement"。预留给未来的其它判定场景。
	Feature string `gorm:"column:feature;size:100;not null;default:'engagement'" json:"feature"`
	Model   string `gorm:"column:model;size:255;not null;default:''" json:"model"`

	// 判定结果
	// Engage LLM 认为是否值得参与。
	Engage bool `gorm:"column:engage" json:"engage"`
	// Score 0-100 评分。0 表示未使用评分模式（传统 YES/NO），与 JudgeResult.Score 语义一致。
	Score int `gorm:"column:score;default:0" json:"score"`
	// Reason LLM 给出的理由（截断存储，避免超长文本撑爆表）。
	Reason string `gorm:"column:reason;size:512" json:"reason"`
	// Tier 决策层（TierRule / TierLLM）。
	Tier string `gorm:"column:tier;size:16" json:"tier"`

	// 耗时
	LatencyMS int64 `gorm:"column:latency_ms;default:0" json:"latencyMs"`
}

// TableName 指定表名。
func (JudgeRecord) TableName() string {
	return "judge_records"
}
