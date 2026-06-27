package stats

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/llm"
)

// Recorder 实现 llm.UsageRecorder 接口。
// 通过异步 channel 收集指标，后台 goroutine 定时批量 upsert 到数据库。
type Recorder struct {
	db     *gorm.DB
	logger *zap.SugaredLogger

	// 异步写入
	ch     chan llm.UsageMetric
	stopCh chan struct{}
	wg     sync.WaitGroup

	flushInterval time.Duration
	batchSize     int
}

// RecorderParams 是 fx 注入参数。
type RecorderParams struct {
	DB     *gorm.DB
	Logger *zap.SugaredLogger
}

// NewRecorder 创建使用统计记录器。
func NewRecorder(db *gorm.DB, logger *zap.SugaredLogger) *Recorder {
	r := &Recorder{
		db:            db,
		logger:        logger,
		ch:            make(chan llm.UsageMetric, 1024),
		stopCh:        make(chan struct{}),
		flushInterval: 5 * time.Second,
		batchSize:     100,
	}
	return r
}

// Start 启动后台写入 goroutine。
func (r *Recorder) Start() {
	r.wg.Add(1)
	go r.run()
}

// Stop 停止后台写入 goroutine，刷新剩余指标。
func (r *Recorder) Stop() {
	close(r.stopCh)
	r.wg.Wait()
}

// SyncFlush 同步刷新所有缓冲的指标。用于测试。
func (r *Recorder) SyncFlush() {
	// 通过 stopCh + 重启实现同步 flush
	// 更简单：直接从 channel drain 并 flush
	var batch []llm.UsageMetric
	for {
		select {
		case m := <-r.ch:
			batch = append(batch, m)
		default:
			if len(batch) > 0 {
				if err := r.flushBatch(batch); err != nil {
					r.logger.Errorw("stats recorder: sync flush failed", "err", err)
				}
			}
			return
		}
	}
}

// RecordUsage 异步记录一次使用指标（实现 llm.UsageRecorder 接口）。
// 非阻塞：channel 满时丢弃并记录警告。
func (r *Recorder) RecordUsage(ctx context.Context, metric llm.UsageMetric) {
	select {
	case r.ch <- metric:
	default:
		r.logger.Warnw("stats recorder: channel full, metric dropped",
			"bot_id", metric.BotID,
			"model", metric.Model,
			"feature", metric.Feature)
	}
}

// run 是后台写入循环。
func (r *Recorder) run() {
	defer r.wg.Done()

	batch := make([]llm.UsageMetric, 0, r.batchSize)
	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.flushBatch(batch); err != nil {
			r.logger.Errorw("stats recorder: flush failed",
				"count", len(batch), "err", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case metric := <-r.ch:
			batch = append(batch, metric)
			if len(batch) >= r.batchSize {
				flush()
			}

		case <-ticker.C:
			flush()

		case <-r.stopCh:
			// drain 剩余
			for {
				select {
				case metric := <-r.ch:
					batch = append(batch, metric)
				default:
					flush()
					return
				}
			}
		}
	}
}

// aggRow 是聚合后的单行数据。
type aggRow struct {
	BotID             string
	Model             string
	Feature           string
	Channel           string
	Date              time.Time
	TotalRequests     int
	CacheHitRequests  int
	CacheMissRequests int
	CacheReadTokens   int
	CacheWriteTokens  int
	NonCacheTokens    int
	InputTokens       int
	OutputTokens      int
	TotalTokens       int
	ToolCalls         int
	Steps             int
}

// flushBatch 将一批指标按维度聚合后逐行 upsert 到数据库。
func (r *Recorder) flushBatch(metrics []llm.UsageMetric) error {
	// 按 (bot_id, model, feature, channel, date) 聚合
	type aggKey struct {
		botID   string
		model   string
		feature string
		channel string
		date    time.Time
	}
	aggregated := make(map[aggKey]*aggRow)

	for _, m := range metrics {
		date := truncateToDate(time.Now())
		key := aggKey{
			botID:   m.BotID,
			model:   m.Model,
			feature: m.Feature,
			channel: m.Channel,
			date:    date,
		}
		row, ok := aggregated[key]
		if !ok {
			row = &aggRow{
				BotID:   m.BotID,
				Model:   m.Model,
				Feature: m.Feature,
				Channel: m.Channel,
				Date:    date,
			}
			aggregated[key] = row
		}

		row.TotalRequests++
		if m.Usage.CachedInputTokens > 0 || m.Usage.InputTokenDetails.CacheReadTokens > 0 {
			row.CacheHitRequests++
		} else {
			row.CacheMissRequests++
		}
		row.CacheReadTokens += m.Usage.InputTokenDetails.CacheReadTokens
		row.CacheWriteTokens += m.Usage.InputTokenDetails.CacheWriteTokens
		row.NonCacheTokens += m.Usage.InputTokenDetails.NoCacheTokens
		row.InputTokens += m.Usage.InputTokens
		row.OutputTokens += m.Usage.OutputTokens
		row.TotalTokens += m.Usage.TotalTokens
		row.ToolCalls += m.ToolCalls
		row.Steps += m.Steps
	}

	// 逐行 upsert（SQLite UPSERT 语法）
	for _, row := range aggregated {
		if err := r.upsertRow(row); err != nil {
			return err
		}
	}
	return nil
}

// upsertRow 使用 SQLite 原生 UPSERT 语法插入或累加一行。
func (r *Recorder) upsertRow(row *aggRow) error {
	now := time.Now().UTC()
	sql := `INSERT INTO stats_usage_daily (
		bot_id, model, feature, channel, date,
		total_requests, cache_hit_requests, cache_miss_requests,
		cache_read_tokens, cache_write_tokens, non_cache_tokens,
		input_tokens, output_tokens, total_tokens,
		tool_calls, steps,
		created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(bot_id, model, feature, channel, date) DO UPDATE SET
		total_requests = total_requests + excluded.total_requests,
		cache_hit_requests = cache_hit_requests + excluded.cache_hit_requests,
		cache_miss_requests = cache_miss_requests + excluded.cache_miss_requests,
		cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
		cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens,
		non_cache_tokens = non_cache_tokens + excluded.non_cache_tokens,
		input_tokens = input_tokens + excluded.input_tokens,
		output_tokens = output_tokens + excluded.output_tokens,
		total_tokens = total_tokens + excluded.total_tokens,
		tool_calls = tool_calls + excluded.tool_calls,
		steps = steps + excluded.steps,
		updated_at = excluded.updated_at`

	return r.db.Exec(sql,
		row.BotID, row.Model, row.Feature, row.Channel, row.Date,
		row.TotalRequests, row.CacheHitRequests, row.CacheMissRequests,
		row.CacheReadTokens, row.CacheWriteTokens, row.NonCacheTokens,
		row.InputTokens, row.OutputTokens, row.TotalTokens,
		row.ToolCalls, row.Steps,
		now, now,
	).Error
}

// truncateToDate 截断到当天零点（UTC）。
func truncateToDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
