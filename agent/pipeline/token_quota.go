package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// TokenQuotaMiddleware — 按月 Token 额度管控
//
// 层级限额继承（从细到粗）：
//   chat（具体会话） → channel（渠道类型） → bot（机器人） → system（系统）
//
// 规则：
//   - 不设置 = 不限制该层级
//   - 限额数按对应层级累积统计
//   - 允许最后一次超额度（e.g., 998 + 102 = 1100，但下次请求拦截）
//   - 每月 1 日自动重置计数
//
// 使用方式：
//
//	resolver := NewQuotaResolver(store)
//	llmStage := stages.NewLLMStage(...)
//	guarded := TokenQuotaMiddleware(resolver, tp, logger)(llmStage)
// ============================================================================

// ============================================================================
// 配置接口
// ============================================================================

// QuotaConfigReader 从配置存储读取 int64 值。
// config.Store 自动满足此接口。
type QuotaConfigReader interface {
	GetInt64(key string, defaultValue int64) int64
}

// QuotaResolution 一次层级解析的结果。
type QuotaResolution struct {
	Limit     int64  // 0 = unlimited
	Dimension string // 命中的维度标识（如 "chat:telegram:-123"），空 = 未命中
}

// ============================================================================
// QuotaResolver — 层级限额解析器
// ============================================================================

// QuotaResolver 负责从配置存储中解析层级 Token 额度。
type QuotaResolver struct {
	store QuotaConfigReader
}

// NewQuotaResolver 创建层级限额解析器。
func NewQuotaResolver(store QuotaConfigReader) *QuotaResolver {
	return &QuotaResolver{store: store}
}

// Resolve 按层级继承规则解析限额。
// 优先级：chat > channel > bot > system
func (r *QuotaResolver) Resolve(botID, channelType, chatID string) QuotaResolution {
	// 1. chat 级（最细粒度）
	if chatID != "" && channelType != "" {
		key := quotaChatKey(botID, channelType, chatID)
		if v := r.store.GetInt64(key, 0); v > 0 {
			return QuotaResolution{Limit: v, Dimension: quotaDimChat(botID, channelType, chatID)}
		}
	}

	// 2. channel 级
	if channelType != "" {
		key := quotaChannelKey(botID, channelType)
		if v := r.store.GetInt64(key, 0); v > 0 {
			return QuotaResolution{Limit: v, Dimension: quotaDimChannel(botID, channelType)}
		}
	}

	// 3. bot 级
	if botID != "" {
		key := quotaBotKey(botID)
		if v := r.store.GetInt64(key, 0); v > 0 {
			return QuotaResolution{Limit: v, Dimension: quotaDimBot(botID)}
		}
	}

	// 4. system 级
	if v := r.store.GetInt64(quotaSystemKey(), 0); v > 0 {
		return QuotaResolution{Limit: v, Dimension: quotaDimSystem()}
	}

	return QuotaResolution{} // unlimited
}

// ============================================================================
// MonthlyCounter — 月度计数（线程安全，自动跨月重置）
// ============================================================================

type monthlyCounter struct {
	mu     sync.Mutex
	month  string // "2026-06"
	tokens int64
}

func newMonthlyCounter() *monthlyCounter {
	return &monthlyCounter{month: currentMonth()}
}

// add 累加 tokens 并返回当前总额。
// 跨月自动重置。
func (c *monthlyCounter) add(n int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := currentMonth()
	if c.month != now {
		c.month = now
		c.tokens = 0
	}
	c.tokens += n
	return c.tokens
}

// get 返回当前额度（不累加）。
// 跨月自动重置。
func (c *monthlyCounter) get() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := currentMonth()
	if c.month != now {
		c.month = now
		c.tokens = 0
	}
	return c.tokens
}

// currentMonth 返回当前月份标识 "YYYY-MM"。
func currentMonth() string {
	now := time.Now()
	return fmt.Sprintf("%04d-%02d", now.Year(), now.Month())
}

// ============================================================================
// TokenQuotaState — 中间件共享状态
// ============================================================================

// TokenQuotaState 持有 per-dimension 月度计数器。
type TokenQuotaState struct {
	mu       sync.Mutex
	counters map[string]*monthlyCounter
}

// newTokenQuotaState 创建配额状态。
func newTokenQuotaState() *TokenQuotaState {
	return &TokenQuotaState{
		counters: make(map[string]*monthlyCounter),
	}
}

// getOrCreate 返回指定维度的计数器（线程安全）。
func (s *TokenQuotaState) counter(dim string) *monthlyCounter {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.counters[dim]
	if !ok {
		c = newMonthlyCounter()
		s.counters[dim] = c
	}
	return c
}

// Usage 返回指定维度的当前用量（不累加）。
func (s *TokenQuotaState) Usage(dim string) int64 {
	return s.counter(dim).get()
}

// addUsage 累加 tokens 到指定维度。
func (s *TokenQuotaState) AddUsage(dim string, tokens int64) int64 {
	return s.counter(dim).add(tokens)
}

// Snapshot 返回所有维度的用量快照。
func (s *TokenQuotaState) Snapshot() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]int64, len(s.counters))
	for k, c := range s.counters {
		m[k] = c.get()
	}
	return m
}

// Reset 重置指定维度的计数（跨月或手动）。
func (s *TokenQuotaState) Reset(dim string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.counters, dim)
}

// ============================================================================
// TokenQuotaMiddleware — 中间件
// ============================================================================

// TokenQuotaMiddleware 返回一个按月控制 Token 额度的中间件。
//
// Before: 解析层级限额 → 检查用量 → 超限则返回 PipelineError
// After:  从 llm.result 提取 Usage → 累加至对应维度
func TokenQuotaMiddleware(resolver *QuotaResolver, tp trace.TracerProvider, logger *zap.SugaredLogger) Middleware {
	if resolver == nil {
		return func(next core.Stage) core.Stage { return next }
	}

	state := newTokenQuotaState()
	tracer := tp.Tracer("github.com/kasuganosora/thinkbot/agent/pipeline/token_quota")
	logger = logger.With("component", "token_quota")

	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: "token_quota",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				msg := env.Message
				botID := msg.BotID
				channelType := getChannelType(&msg)
				chatID := getChatID(&msg)

				// 解析层级限额
				res := resolver.Resolve(botID, channelType, chatID)
				if res.Limit <= 0 {
					// 无限制，透传
					return next.Process(ctx, env)
				}

				ctx, span := tracer.Start(ctx, "pipeline.token_quota.check",
					trace.WithAttributes(
						attribute.String("quota.dimension", res.Dimension),
						attribute.Int64("quota.limit", res.Limit),
						attribute.String("quota.bot_id", botID),
						attribute.String("quota.channel_type", channelType),
						attribute.String("quota.chat_id", chatID),
					))
				defer span.End()
				logger := traceid.WithLoggerFrom(ctx, logger)

				// ---- Before: 检查额度 ----
				current := state.Usage(res.Dimension)
				span.SetAttributes(attribute.Int64("quota.current", current))

				if current >= res.Limit {
					span.SetAttributes(attribute.Bool("quota.blocked", true))
					logger.Warnw("token quota exceeded",
						"dimension", res.Dimension,
						"current", current,
						"limit", res.Limit,
						"bot_id", botID,
						"channel_type", channelType,
						"chat_id", chatID)
					return env, &core.PipelineError{
						Stage:   "token_quota",
						Message: fmt.Sprintf("monthly token quota exceeded: %d/%d (dimension: %s)", current, res.Limit, res.Dimension),
						Cause:   fmt.Errorf("token quota exhausted for dimension %s", res.Dimension),
					}
				}

				// ---- 执行 ----
				result, err := next.Process(ctx, env)

				// ---- After: 累加用量 ----
				if result != nil {
					if v, ok := result.Get("llm.result"); ok {
						if genResult, ok := v.(*llm.GenerateResult); ok && genResult != nil {
							used := int64(genResult.Usage.TotalTokens)
							newTotal := state.AddUsage(res.Dimension, used)
							blocked := newTotal >= res.Limit

							span.SetAttributes(
								attribute.Int64("quota.used_this_call", used),
								attribute.Int64("quota.new_total", newTotal),
								attribute.Bool("quota.will_block_next", blocked),
							)

							logger.Infow("token quota accumulated",
								"dimension", res.Dimension,
								"used", used,
								"total", newTotal,
								"limit", res.Limit,
								"percent", float64(newTotal)/float64(res.Limit)*100,
								"will_block_next", blocked)
						}
					}
				}

				return result, err
			},
		}
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

// getChannelType 从消息中提取 channel 类型（如 "telegram"）。
func getChannelType(msg *core.Message) string {
	if msg.Metadata != nil {
		if ct, ok := msg.Metadata["channel_type"].(string); ok && ct != "" {
			return ct
		}
	}
	return msg.Source
}

// getChatID 从消息中提取 chat ID（群 ID 或用户 ID）。
func getChatID(msg *core.Message) string {
	if msg.Metadata != nil {
		if cid, ok := msg.Metadata["chat_id"].(string); ok && cid != "" {
			return cid
		}
	}
	return ""
}

// ============================================================================
// 配置键（内部使用，对外通过 config/keys.go 暴露）
// ============================================================================

func quotaSystemKey() string { return "system.token_quota" }

func quotaBotKey(botID string) string { return "bot." + botID + ".token_quota" }

func quotaChannelKey(botID, channelType string) string {
	return "bot." + botID + ".token_quota.channel." + channelType
}

func quotaChatKey(botID, channelType, chatID string) string {
	return "bot." + botID + ".token_quota.channel." + channelType + "." + chatID
}

func quotaDimChat(botID, channelType, chatID string) string {
	return "chat:" + channelType + ":" + chatID
}

func quotaDimChannel(botID, channelType string) string {
	return "channel:" + channelType
}

func quotaDimBot(botID string) string {
	return "bot:" + botID
}

func quotaDimSystem() string {
	return "system"
}
