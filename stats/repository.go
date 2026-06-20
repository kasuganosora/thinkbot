package stats

import (
	"time"

	"gorm.io/gorm"
)

// BotModelStat 是按 (bot_id, model) 维度的汇总查询结果。
type BotModelStat struct {
	BotID            string `gorm:"column:bot_id" json:"botId"`
	Model            string `gorm:"column:model" json:"model"`
	TotalRequests    int    `gorm:"column:total_requests" json:"totalRequests"`
	CacheHitRequests int    `gorm:"column:cache_hit_requests" json:"cacheHitRequests"`
	CacheMissRequests int   `gorm:"column:cache_miss_requests" json:"cacheMissRequests"`
	CacheReadTokens  int    `gorm:"column:cache_read_tokens" json:"cacheReadTokens"`
	CacheWriteTokens int    `gorm:"column:cache_write_tokens" json:"cacheWriteTokens"`
	NonCacheTokens   int    `gorm:"column:non_cache_tokens" json:"nonCacheTokens"`
	InputTokens      int    `gorm:"column:input_tokens" json:"inputTokens"`
	OutputTokens     int    `gorm:"column:output_tokens" json:"outputTokens"`
	TotalTokens      int    `gorm:"column:total_tokens" json:"totalTokens"`
	ToolCalls        int    `gorm:"column:tool_calls" json:"toolCalls"`
}

// ModelFeatureStat 是按 (model, feature) 维度的汇总查询结果。
type ModelFeatureStat struct {
	Model            string `gorm:"column:model" json:"model"`
	Feature          string `gorm:"column:feature" json:"feature"`
	TotalRequests    int    `gorm:"column:total_requests" json:"totalRequests"`
	CacheHitRequests int    `gorm:"column:cache_hit_requests" json:"cacheHitRequests"`
	CacheMissRequests int   `gorm:"column:cache_miss_requests" json:"cacheMissRequests"`
	CacheReadTokens  int    `gorm:"column:cache_read_tokens" json:"cacheReadTokens"`
	TotalTokens      int    `gorm:"column:total_tokens" json:"totalTokens"`
}

// DailyStat 是按日期维度的汇总查询结果。
type DailyStat struct {
	Date             time.Time `gorm:"column:date" json:"date"`
	TotalRequests    int       `gorm:"column:total_requests" json:"totalRequests"`
	CacheHitRequests int       `gorm:"column:cache_hit_requests" json:"cacheHitRequests"`
	CacheMissRequests int      `gorm:"column:cache_miss_requests" json:"cacheMissRequests"`
	TotalTokens      int       `gorm:"column:total_tokens" json:"totalTokens"`
}

// GetBotModelStats 查询指定 Bot 使用的各模型统计（可限定日期范围）。
func GetBotModelStats(db *gorm.DB, botID string, from, to *time.Time) ([]BotModelStat, error) {
	var results []BotModelStat
	q := db.Table("stats_usage_daily").
		Select(`
			bot_id, model,
			SUM(total_requests) as total_requests,
			SUM(cache_hit_requests) as cache_hit_requests,
			SUM(cache_miss_requests) as cache_miss_requests,
			SUM(cache_read_tokens) as cache_read_tokens,
			SUM(cache_write_tokens) as cache_write_tokens,
			SUM(non_cache_tokens) as non_cache_tokens,
			SUM(input_tokens) as input_tokens,
			SUM(output_tokens) as output_tokens,
			SUM(total_tokens) as total_tokens,
			SUM(tool_calls) as tool_calls
		`).
		Where("bot_id = ?", botID).
		Group("bot_id, model").
		Order("total_tokens DESC")

	q = applyDateRange(q, from, to)

	err := q.Scan(&results).Error
	return results, err
}

// GetModelFeatureStats 查询指定 Bot + Model 在各功能中的使用统计。
func GetModelFeatureStats(db *gorm.DB, botID, model string, from, to *time.Time) ([]ModelFeatureStat, error) {
	var results []ModelFeatureStat
	q := db.Table("stats_usage_daily").
		Select(`
			model, feature,
			SUM(total_requests) as total_requests,
			SUM(cache_hit_requests) as cache_hit_requests,
			SUM(cache_miss_requests) as cache_miss_requests,
			SUM(cache_read_tokens) as cache_read_tokens,
			SUM(total_tokens) as total_tokens
		`).
		Where("bot_id = ? AND model = ?", botID, model).
		Group("model, feature").
		Order("total_requests DESC")

	q = applyDateRange(q, from, to)

	err := q.Scan(&results).Error
	return results, err
}

// GetDailyStats 查询指定 Bot 的按日汇总统计。
func GetDailyStats(db *gorm.DB, botID string, from, to *time.Time) ([]DailyStat, error) {
	var results []DailyStat
	q := db.Table("stats_usage_daily").
		Select(`
			date,
			SUM(total_requests) as total_requests,
			SUM(cache_hit_requests) as cache_hit_requests,
			SUM(cache_miss_requests) as cache_miss_requests,
			SUM(total_tokens) as total_tokens
		`).
		Where("bot_id = ?", botID).
		Group("date").
		Order("date DESC")

	q = applyDateRange(q, from, to)

	err := q.Scan(&results).Error
	return results, err
}

// GetAllBotsModelStats 查询所有 Bot 的模型使用统计（管理面板用）。
func GetAllBotsModelStats(db *gorm.DB, from, to *time.Time) ([]BotModelStat, error) {
	var results []BotModelStat
	q := db.Table("stats_usage_daily").
		Select(`
			bot_id, model,
			SUM(total_requests) as total_requests,
			SUM(cache_hit_requests) as cache_hit_requests,
			SUM(cache_miss_requests) as cache_miss_requests,
			SUM(cache_read_tokens) as cache_read_tokens,
			SUM(cache_write_tokens) as cache_write_tokens,
			SUM(non_cache_tokens) as non_cache_tokens,
			SUM(input_tokens) as input_tokens,
			SUM(output_tokens) as output_tokens,
			SUM(total_tokens) as total_tokens,
			SUM(tool_calls) as tool_calls
		`).
		Group("bot_id, model").
		Order("bot_id ASC, total_tokens DESC")

	q = applyDateRange(q, from, to)

	err := q.Scan(&results).Error
	return results, err
}

// applyDateRange 应用日期范围过滤。
func applyDateRange(q *gorm.DB, from, to *time.Time) *gorm.DB {
	if from != nil {
		q = q.Where("date >= ?", truncateToDate(*from))
	}
	if to != nil {
		q = q.Where("date <= ?", truncateToDate(to.AddDate(0, 0, 1)))
	}
	return q
}
