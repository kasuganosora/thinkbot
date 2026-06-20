package dao

import "time"

// UsageDaily 按日聚合的 LLM 使用统计表。
// 维度组合 (bot_id, model, feature, date) 唯一，同一组合的多次调用累加计数。
type UsageDaily struct {
	ID uint `gorm:"primaryKey;autoIncrement" json:"id"`

	// 维度
	BotID   string    `gorm:"column:bot_id;size:255;not null;index:idx_usage_daily_unique,unique" json:"botId"`
	Model   string    `gorm:"column:model;size:255;not null;index:idx_usage_daily_unique,unique" json:"model"`
	Feature string    `gorm:"column:feature;size:100;not null;index:idx_usage_daily_unique,unique" json:"feature"`
	Date    time.Time `gorm:"column:date;type:date;not null;index:idx_usage_daily_unique,unique" json:"date"`

	// 请求数
	TotalRequests int `gorm:"column:total_requests;default:0" json:"totalRequests"`
	// 缓存命中/未命中请求数（按请求维度：当次调用是否有缓存读取）
	CacheHitRequests  int `gorm:"column:cache_hit_requests;default:0" json:"cacheHitRequests"`
	CacheMissRequests int `gorm:"column:cache_miss_requests;default:0" json:"cacheMissRequests"`

	// Token 维度
	CacheReadTokens  int `gorm:"column:cache_read_tokens;default:0" json:"cacheReadTokens"`
	CacheWriteTokens int `gorm:"column:cache_write_tokens;default:0" json:"cacheWriteTokens"`
	NonCacheTokens   int `gorm:"column:non_cache_tokens;default:0" json:"nonCacheTokens"`
	InputTokens      int `gorm:"column:input_tokens;default:0" json:"inputTokens"`
	OutputTokens     int `gorm:"column:output_tokens;default:0" json:"outputTokens"`
	TotalTokens      int `gorm:"column:total_tokens;default:0" json:"totalTokens"`

	// 工具调用
	ToolCalls int `gorm:"column:tool_calls;default:0" json:"toolCalls"`

	// 编排步数累计
	Steps int `gorm:"column:steps;default:0" json:"steps"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime" json:"createdAt"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime" json:"updatedAt"`
}

// TableName 指定表名。
func (UsageDaily) TableName() string { return "stats_usage_daily" }
