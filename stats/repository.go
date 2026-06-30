package stats

import (
	"time"

	"gorm.io/gorm"
)

// BotModelStat 是按 (bot_id, model) 维度的汇总查询结果。
type BotModelStat struct {
	BotID             string `gorm:"column:bot_id" json:"botId"`
	Model             string `gorm:"column:model" json:"model"`
	TotalRequests     int    `gorm:"column:total_requests" json:"totalRequests"`
	CacheHitRequests  int    `gorm:"column:cache_hit_requests" json:"cacheHitRequests"`
	CacheMissRequests int    `gorm:"column:cache_miss_requests" json:"cacheMissRequests"`
	CacheReadTokens   int    `gorm:"column:cache_read_tokens" json:"cacheReadTokens"`
	CacheWriteTokens  int    `gorm:"column:cache_write_tokens" json:"cacheWriteTokens"`
	NonCacheTokens    int    `gorm:"column:non_cache_tokens" json:"nonCacheTokens"`
	InputTokens       int    `gorm:"column:input_tokens" json:"inputTokens"`
	OutputTokens      int    `gorm:"column:output_tokens" json:"outputTokens"`
	TotalTokens       int    `gorm:"column:total_tokens" json:"totalTokens"`
	ToolCalls         int    `gorm:"column:tool_calls" json:"toolCalls"`
}

// ModelFeatureStat 是按 (model, feature) 维度的汇总查询结果。
type ModelFeatureStat struct {
	Model             string `gorm:"column:model" json:"model"`
	Feature           string `gorm:"column:feature" json:"feature"`
	TotalRequests     int    `gorm:"column:total_requests" json:"totalRequests"`
	CacheHitRequests  int    `gorm:"column:cache_hit_requests" json:"cacheHitRequests"`
	CacheMissRequests int    `gorm:"column:cache_miss_requests" json:"cacheMissRequests"`
	CacheReadTokens   int    `gorm:"column:cache_read_tokens" json:"cacheReadTokens"`
	TotalTokens       int    `gorm:"column:total_tokens" json:"totalTokens"`
}

// DailyStat 是按日期维度的汇总查询结果。
type DailyStat struct {
	Date              time.Time `gorm:"column:date" json:"date"`
	TotalRequests     int       `gorm:"column:total_requests" json:"totalRequests"`
	CacheHitRequests  int       `gorm:"column:cache_hit_requests" json:"cacheHitRequests"`
	CacheMissRequests int       `gorm:"column:cache_miss_requests" json:"cacheMissRequests"`
	CacheReadTokens   int       `gorm:"column:cache_read_tokens" json:"cacheReadTokens"`
	CacheWriteTokens  int       `gorm:"column:cache_write_tokens" json:"cacheWriteTokens"`
	NonCacheTokens    int       `gorm:"column:non_cache_tokens" json:"nonCacheTokens"`
	TotalTokens       int       `gorm:"column:total_tokens" json:"totalTokens"`
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
			SUM(cache_read_tokens) as cache_read_tokens,
			SUM(cache_write_tokens) as cache_write_tokens,
			SUM(non_cache_tokens) as non_cache_tokens,
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

// GetDailyStatsGlobal 查询全局按日汇总统计（可选限定单个 bot）。
func GetDailyStatsGlobal(db *gorm.DB, botID string, from, to *time.Time) ([]DailyStat, error) {
	var results []DailyStat
	q := db.Table("stats_usage_daily").
		Select(`
			date,
			SUM(total_requests) as total_requests,
			SUM(cache_hit_requests) as cache_hit_requests,
			SUM(cache_miss_requests) as cache_miss_requests,
			SUM(cache_read_tokens) as cache_read_tokens,
			SUM(cache_write_tokens) as cache_write_tokens,
			SUM(non_cache_tokens) as non_cache_tokens,
			SUM(total_tokens) as total_tokens
		`)

	if botID != "" {
		q = q.Where("bot_id = ?", botID)
	}

	q = q.Group("date").Order("date ASC")
	q = applyDateRange(q, from, to)

	err := q.Scan(&results).Error
	return results, err
}

// DailyByBotEntry 按日×Bot 维度的每日用量。
type DailyByBotEntry struct {
	Date    time.Time `gorm:"column:date" json:"date"`
	BotID   string    `gorm:"column:bot_id" json:"botId"`
	Tokens  int       `gorm:"column:total_tokens" json:"tokens"`
}

// GetDailyByBotStats 查询按日×Bot 的 token 使用量（用于堆叠图表）。
func GetDailyByBotStats(db *gorm.DB, from, to *time.Time) ([]DailyByBotEntry, error) {
	var results []DailyByBotEntry
	q := db.Table("stats_usage_daily").
		Select(`date, bot_id, SUM(total_tokens) as total_tokens`).
		Group("date, bot_id").
		Order("date ASC, bot_id ASC")

	q = applyDateRange(q, from, to)

	err := q.Scan(&results).Error
	return results, err
}

// UsageRecord 单条用量记录（流水明细查询）。
type UsageRecord struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	Date            time.Time `gorm:"column:date" json:"time"`
	BotID           string    `gorm:"column:bot_id" json:"botId"`
	Model           string    `gorm:"column:model" json:"model"`
	Feature         string    `gorm:"column:feature" json:"feature"`
	CacheReadTokens int       `gorm:"column:cache_read_tokens" json:"cacheReadTokens"`
	InputTokens     int       `gorm:"column:input_tokens" json:"inputTokens"`
	OutputTokens    int       `gorm:"column:output_tokens" json:"outputTokens"`
	TotalRequests   int       `gorm:"column:total_requests" json:"totalRequests"`
}

// GetUsageRecords 分页查询用量流水记录。
func GetUsageRecords(db *gorm.DB, botID string, from, to *time.Time, page, pageSize int) ([]UsageRecord, int64, error) {
	q := db.Table("stats_usage_daily").
		Select(`rowid as id, date, bot_id, model, feature, cache_read_tokens, input_tokens, output_tokens, total_requests`)

	if botID != "" {
		q = q.Where("bot_id = ?", botID)
	}
	q = applyDateRange(q, from, to)

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var results []UsageRecord
	offset := (page - 1) * pageSize
	err := q.Order("date DESC, bot_id ASC").Offset(offset).Limit(pageSize).Scan(&results).Error
	return results, total, err
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
