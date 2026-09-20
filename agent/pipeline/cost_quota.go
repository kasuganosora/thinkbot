package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// CostQuota — 金钱额度管控（token↔金钱换算表 + 按 bot / 全局双档 + 四堵墙）
//
// 与 TokenQuotaMiddleware 的区别：
//   - 计量单位是「金钱」（float64），而非 token。
//   - 支持全局 + per-bot 双档（system.cost_quota / BotDefinition.CostQuota）。
//   - 一次 LLM 调用 (bot B, feature F) 同时受四堵墙约束，任意一堵先到即拦截
//     （「最低墙先碰」语义，无需显式取 min）：
//       1. bot 总预算        dim = "bot:B"
//       2. 全局总预算        dim = "system"
//       3. bot 功能预算      dim = "bot:B:feature:F"
//       4. 全局功能预算      dim = "feature:F"
//   - period 全局强制一致（daily|weekly|monthly），由 system.cost_quota.period 决定，
//     bot 不另设 period。
//   - 拦截强制全部落在 CostRecordingProvider（provider 层），故 cron / heartbeat /
//     dreaming 等非用户路径也能被覆盖；CostQuotaMiddleware 仅给用户路径补「友好回复」。
// ============================================================================

// ----------------------------------------------------------------------------
// 配置结构
// ----------------------------------------------------------------------------

// CostQuotaConfig 是 per-bot 的金钱额度配置（挂在 BotDefinition.CostQuota JSON 列）。
// 不含 Period：period 全局强制一致，由 SystemCostQuotaConfig.Period 决定。
type CostQuotaConfig struct {
	Enabled  bool               `json:"enabled"`           // 是否启用本 bot 额度
	Currency string             `json:"currency"`          // 货币，默认 CNY
	Total    float64            `json:"total"`             // 该周期总预算（金钱），0 = 不限制
	Features map[string]float64 `json:"features"`          // 各功能预算（金钱），0/缺 = 不限制
}

// SystemCostQuotaConfig 是全局金钱额度配置，存于 config 键 system.cost_quota（JSON）。
// Period 为全局唯一权威周期；bot 额度强制继承。
type SystemCostQuotaConfig struct {
	Period   string             `json:"period"`   // daily|weekly|monthly，默认 monthly
	Currency string             `json:"currency"` // 默认 CNY
	Total    float64            `json:"total"`    // 全局总预算，0 = 不限制
	Features map[string]float64 `json:"features"` // 全局各功能预算
}

// ParseCostQuotaConfig 从 JSON 字符串解析 per-bot 额度配置；空/非法 → 零值（不启用）。
func ParseCostQuotaConfig(raw string) (CostQuotaConfig, bool) {
	if raw == "" {
		return CostQuotaConfig{}, false
	}
	var cfg CostQuotaConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return CostQuotaConfig{}, false
	}
	return cfg, cfg.Enabled
}

// SystemCostQuotaFromStore 从 config store 读取全局金钱额度配置。
func SystemCostQuotaFromStore(store *config.Store) (SystemCostQuotaConfig, bool) {
	raw, ok := store.Get(config.SystemCostQuotaKey())
	if !ok || raw == "" {
		return SystemCostQuotaConfig{}, false
	}
	var cfg SystemCostQuotaConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return SystemCostQuotaConfig{}, false
	}
	if cfg.Period == "" {
		cfg.Period = "monthly"
	}
	if cfg.Currency == "" {
		cfg.Currency = "CNY"
	}
	return cfg, true
}

// ----------------------------------------------------------------------------
// 周期桶
// ----------------------------------------------------------------------------

// periodKey 返回当前周期的桶标识（跨桶即重置计数）。
// 统一使用 UTC，与 token quota（currentMonth 用 time.Now().UTC()）及
// stats_usage_daily.date 列（truncateToDate 落 UTC 零点）保持一致，避免时区边界错位。
func periodKey(now time.Time, period string) string {
	now = now.UTC()
	switch period {
	case "daily":
		return now.Format("2006-01-02")
	case "weekly":
		y, w := now.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	default: // monthly
		return now.Format("2006-01")
	}
}

// periodStart 返回当前周期起点（用于 RestoreFromStats 过滤 stats_usage_daily）。
// 使用 UTC，与 date 列（UTC 零点）对齐，保证 WHERE date >= start 边界正确。
func periodStart(now time.Time, period string) time.Time {
	now = now.UTC()
	switch period {
	case "daily":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case "weekly":
		// ISO 周以周一为起点
		offset := int(time.Monday - now.Weekday())
		if offset > 0 {
			offset -= 7
		}
		return time.Date(now.Year(), now.Month(), now.Day()+offset, 0, 0, 0, 0, time.UTC)
	default: // monthly
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
}

// periodLabel 周期中文标签（友好提示用）。
func periodLabel(period string) string {
	switch period {
	case "daily":
		return "今日"
	case "weekly":
		return "本周"
	default:
		return "本月"
	}
}

// PeriodNow 返回当前周期的桶标识（导出，供 API 展示）。
func PeriodNow(period string) string {
	return periodKey(time.Now(), period)
}

// PeriodStartNow 返回当前周期起点（导出，供 API 过滤 stats_usage_daily）。
func PeriodStartNow(period string) time.Time {
	return periodStart(time.Now(), period)
}

// ----------------------------------------------------------------------------
// 额度墙解析器
// ----------------------------------------------------------------------------

// costWall 一堵额度墙。
type costWall struct {
	dim    string  // 计数器维度
	limit  float64 // 预算上限（0 = 不限制）
	feature string // 触发的功能（空 = 总预算墙）
}

// CostQuotaResolver 解析一次调用涉及的额度墙。
// bot 配置默认在构造时捕获（来自 BotDefinition.CostQuota）；可通过 WithBotReader
// 改为实时读取（从 config store 镜像），使运行时更新额度无需重启 bot 即生效。
// 全局配置每次调用实时读取（与 token quota 的 system 档一致，改完即时生效）。
type CostQuotaResolver struct {
	botCfg    CostQuotaConfig
	botReader func() (CostQuotaConfig, bool)
	sysReader func() (SystemCostQuotaConfig, bool)
}

// NewCostQuotaResolver 创建解析器。
// botCfg 作为初始快照（botReader 设置后会被实时值覆盖）；sysReader 实时读取 system.cost_quota。
func NewCostQuotaResolver(botCfg CostQuotaConfig, sysReader func() (SystemCostQuotaConfig, bool)) *CostQuotaResolver {
	return &CostQuotaResolver{botCfg: botCfg, sysReader: sysReader}
}

// WithBotReader 使 bot 级额度配置实时读取（从 config store 镜像），更新后无需重启 bot 即生效。
func (r *CostQuotaResolver) WithBotReader(fn func() (CostQuotaConfig, bool)) *CostQuotaResolver {
	r.botReader = fn
	return r
}

// botConfig 返回当前 bot 额度配置：若设置了 botReader 则实时读取，否则用构造时快照。
func (r *CostQuotaResolver) botConfig() CostQuotaConfig {
	if r.botReader != nil {
		if c, ok := r.botReader(); ok {
			return c
		}
	}
	return r.botCfg
}

// Walls 返回 (botID, feature) 这一次调用涉及的所有「有限额」的墙。
// 未设限额的维度不出现（计数器也不必维护）。
func (r *CostQuotaResolver) Walls(botID, feature string) []costWall {
	var sys SystemCostQuotaConfig
	if s, ok := r.sysReader(); ok {
		sys = s
	}
	cfg := r.botConfig()
	walls := make([]costWall, 0, 4)
	if cfg.Enabled && cfg.Total > 0 {
		walls = append(walls, costWall{dim: costDimBot(botID), limit: cfg.Total})
	}
	if sys.Total > 0 {
		walls = append(walls, costWall{dim: costDimSystem(), limit: sys.Total})
	}
	if cfg.Enabled && len(cfg.Features) > 0 {
		if lim, ok := cfg.Features[feature]; ok && lim > 0 {
			walls = append(walls, costWall{dim: costDimBotFeature(botID, feature), limit: lim, feature: feature})
		}
	}
	if len(sys.Features) > 0 {
		if lim, ok := sys.Features[feature]; ok && lim > 0 {
			walls = append(walls, costWall{dim: costDimFeature(feature), limit: lim, feature: feature})
		}
	}
	return walls
}

// ----------------------------------------------------------------------------
// 周期计数器（线程安全，跨周期自动重置）
// ----------------------------------------------------------------------------

type costPeriodCounter struct {
	mu     sync.Mutex
	bucket string  // 如 "2026-09"
	amount float64
}

func newCostPeriodCounter() *costPeriodCounter {
	return &costPeriodCounter{bucket: periodKey(time.Now(), "monthly")}
}

func (c *costPeriodCounter) add(amount float64, bucket string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bucket != bucket {
		c.bucket = bucket
		c.amount = 0
	}
	c.amount += amount
	return c.amount
}

func (c *costPeriodCounter) get(bucket string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bucket != bucket {
		c.bucket = bucket
		c.amount = 0
	}
	return c.amount
}

// ----------------------------------------------------------------------------
// CostQuotaState — 中间件与 provider 共享的计费状态
// ----------------------------------------------------------------------------

// CostQuotaState 持有 per-dimension 的周期计数器。
// 全局 period 由构造时确定（来自 system.cost_quota.period），所有维度共用一个桶边界。
type CostQuotaState struct {
	mu       sync.Mutex
	period   string
	counters map[string]*costPeriodCounter
}

// NewCostQuotaState 创建计费状态。period 缺省 monthly。
func NewCostQuotaState(period string) *CostQuotaState {
	if period == "" {
		period = "monthly"
	}
	return &CostQuotaState{
		period:   period,
		counters: make(map[string]*costPeriodCounter),
	}
}

func (s *CostQuotaState) bucket() string {
	return periodKey(time.Now(), s.period)
}

func (s *CostQuotaState) counter(dim string) *costPeriodCounter {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.counters[dim]
	if !ok {
		c = newCostPeriodCounter()
		s.counters[dim] = c
	}
	return c
}

// Usage 返回某维度当前周期已花费（不累加）。
func (s *CostQuotaState) Usage(dim string) float64 {
	return s.counter(dim).get(s.bucket())
}

// AddCost 累加花费到某维度并返回新总额。
func (s *CostQuotaState) AddCost(dim string, cost float64) float64 {
	return s.counter(dim).add(cost, s.bucket())
}

// Snapshot 返回所有维度当前周期的花费快照。
func (s *CostQuotaState) Snapshot() map[string]float64 {
	s.mu.Lock()
	counters := make(map[string]*costPeriodCounter, len(s.counters))
	for k, c := range s.counters {
		counters[k] = c
	}
	s.mu.Unlock()

	bucket := s.bucket()
	m := make(map[string]float64, len(counters))
	for k, c := range counters {
		m[k] = c.get(bucket)
	}
	return m
}

// Reset 重置某维度计数。
func (s *CostQuotaState) Reset(dim string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counters, dim)
}

// ----------------------------------------------------------------------------
// 计费拦截 + 记账接口实现（供 llm.CostRecordingProvider 回调）
// ----------------------------------------------------------------------------

// CheckWalls 检查四堵墙，任意墙已耗尽返回 *llm.CostQuotaExceededError。
func (s *CostQuotaState) CheckWalls(resolver *CostQuotaResolver, botID, feature string) *llm.CostQuotaExceededError {
	bucket := s.bucket()
	for _, w := range resolver.Walls(botID, feature) {
		cur := s.Usage(w.dim)
		if cur >= w.limit {
			return llm.NewCostQuotaExceeded(w.dim, w.feature, cur, w.limit, bucket)
		}
	}
	return nil
}

// RecordCost 把一次调用的花费计入四维度计数器。
// feature 为空时只记两个总预算维度（避免污染 feature:"" 维度）。
func (s *CostQuotaState) RecordCost(botID, feature string, cost float64) {
	if cost <= 0 {
		return
	}
	s.AddCost(costDimBot(botID), cost)
	s.AddCost(costDimSystem(), cost)
	if feature != "" {
		s.AddCost(costDimBotFeature(botID, feature), cost)
		s.AddCost(costDimFeature(feature), cost)
	}
}

// ----------------------------------------------------------------------------
// RestoreFromStats — 重启后从 stats_usage_daily 回算已花费用
// ----------------------------------------------------------------------------

// RestoreFromStats 用 stats_usage_daily（含 model/feature/tokens）× 当前单价表回算
// 本周期已花费用，恢复四个维度计数器。无需新建表。
func (s *CostQuotaState) RestoreFromStats(ctx context.Context, db *gorm.DB, priceFor llm.ModelPriceResolver, botIDs ...string) error {
	if db == nil || priceFor == nil {
		return nil
	}
	start := periodStart(time.Now(), s.period)
	type row struct {
		BotID  string
		Model  string
		Feature string
		Input   int
		Output  int
		CacheRead int
	}
	var rows []row
	q := db.WithContext(ctx).Raw(`
		SELECT bot_id, model, feature,
		       SUM(input_tokens)  AS input,
		       SUM(output_tokens) AS output,
		       SUM(cache_read_tokens) AS cache_read
		FROM stats_usage_daily
		WHERE date >= ?
		GROUP BY bot_id, model, feature
	`, start)
	if len(botIDs) > 0 {
		q = db.WithContext(ctx).Raw(`
			SELECT bot_id, model, feature,
			       SUM(input_tokens)  AS input,
			       SUM(output_tokens) AS output,
			       SUM(cache_read_tokens) AS cache_read
			FROM stats_usage_daily
			WHERE date >= ? AND bot_id IN ?
			GROUP BY bot_id, model, feature
		`, start, botIDs)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return err
	}

	bucket := s.bucket()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		price, ok := priceFor(r.Model)
		if !ok || !price.HasPrice() {
			continue
		}
		usage := llm.Usage{
			InputTokens:       r.Input,
			OutputTokens:      r.Output,
			InputTokenDetails: llm.InputTokenDetail{CacheReadTokens: r.CacheRead},
		}
		_, _, total := llm.ComputeCost(usage, price)
		if total <= 0 {
			continue
		}
		addTo := func(dim string) {
			c, ok := s.counters[dim]
			if !ok {
				c = newCostPeriodCounter()
				s.counters[dim] = c
			}
			c.mu.Lock()
			if c.bucket != bucket {
				c.bucket = bucket
				c.amount = 0
			}
			c.amount += total
			c.mu.Unlock()
		}
		addTo(costDimBot(r.BotID))
		addTo(costDimSystem())
		if r.Feature != "" && r.Feature != "unknown" {
			addTo(costDimBotFeature(r.BotID, r.Feature))
			addTo(costDimFeature(r.Feature))
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// CostQuotaMiddleware — 用户路径的友好回复拦截
// ----------------------------------------------------------------------------

// CostQuotaMiddleware 返回一个把金钱额度耗尽转为「友好提示回复」的中间件（内部创建 state）。
// 如需跨中间件和 provider wrapper 共享状态，请用 CostQuotaMiddlewareWithState。
func CostQuotaMiddleware(resolver *CostQuotaResolver, state *CostQuotaState, tp trace.TracerProvider, logger *zap.SugaredLogger) Middleware {
	if resolver == nil || state == nil {
		return func(next core.Stage) core.Stage { return next }
	}
	return CostQuotaMiddlewareWithState(resolver, state, tp, logger)
}

// CostQuotaMiddlewareWithState 使用外部 state 创建中间件。
// 拦截逻辑本身在 CostRecordingProvider（pre-check），本中间件仅捕获
// *llm.CostQuotaExceededError 并转为友好回复，不致命。
func CostQuotaMiddlewareWithState(resolver *CostQuotaResolver, state *CostQuotaState, tp trace.TracerProvider, logger *zap.SugaredLogger) Middleware {
	if resolver == nil || state == nil {
		return func(next core.Stage) core.Stage { return next }
	}
	tracer := tp.Tracer("github.com/kasuganosora/thinkbot/agent/pipeline/cost_quota")
	logger = logger.With("component", "cost_quota")

	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: "cost_quota",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				msg := env.Message
				botID := msg.BotID

				ctx, span := tracer.Start(ctx, "pipeline.cost_quota.guard",
					trace.WithAttributes(
						attribute.String("cost_quota.bot_id", botID),
						attribute.String("cost_quota.period", state.period),
					))
				defer span.End()
				logger := traceid.WithLoggerFrom(ctx, logger)

				result, err := next.Process(ctx, env)
				if err != nil {
					var ce *llm.CostQuotaExceededError
					if errors.As(err, &ce) {
						span.SetAttributes(attribute.Bool("cost_quota.blocked", true))
						logger.Infow("cost quota exceeded (user path) → friendly reply",
							"bot_id", botID,
							"dimension", ce.Dimension,
							"feature", ce.Feature,
							"current", ce.Current,
							"limit", ce.Limit,
							"period", ce.Period)
						return friendlyCostReply(env, ce, state.period), nil
					}
				}
				return result, err
			},
		}
	}
}

// friendlyCostReply 构造一条「额度用尽」的友好回复，替换原信封的动作。
func friendlyCostReply(env *core.Envelope, ce *llm.CostQuotaExceededError, period string) *core.Envelope {
	env.ClearActions()
	label := periodLabel(period)
	wall := "预算"
	switch {
	case ce.Feature != "":
		wall = "功能「" + ce.Feature + "」预算"
	case ce.Dimension == costDimSystem():
		wall = "全局总预算"
	}
	text := fmt.Sprintf("%s%s已用尽（当前 ¥%.2f / 上限 ¥%.2f），请于新的计费周期再试。",
		label, wall, ce.Current, ce.Limit)
	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Payload: text,
	})
	return env
}

// ----------------------------------------------------------------------------
// Dimension 字符串
// ----------------------------------------------------------------------------

func costDimBot(botID string) string        { return "bot:" + botID }
func costDimSystem() string                 { return "system" }
func costDimBotFeature(botID, feature string) string { return "bot:" + botID + ":feature:" + feature }
func costDimFeature(feature string) string  { return "feature:" + feature }
