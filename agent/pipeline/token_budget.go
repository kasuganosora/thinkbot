package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// TokenBudgetMiddleware — 按会话追踪 Token 消耗，超限时注入警告或终止
//
// 借鉴 deer-flow 的 TokenBudgetMiddleware 设计：
//   - 软限制（warn）：注入警告提示 LLM "尽快总结"
//   - 硬限制（hard）：触顶时重置该会话的计数窗口并注入一次硬警告，
//     本条消息仍正常处理（不再永久中止，避免活跃会话被卡死）
//
// 在 thinkbot 中的实现：
//   - 按 Channel（会话）累积 token 用量，跨多条消息持久跟踪
//   - 每次 Pipeline 执行前后检查预算
//   - 通过 Envelope KV 传递警告（延迟注入模式）
//
// 防止"预算永久卡死"：
//   - 若某 channel 空闲超过 resetTTL，下次访问自动清零（按会话自然恢复）
//   - 也可通过 ResetChannel / ResetAll 手动重置
//
// 使用方式：
//
//	budget := NewTokenBudgetConfig().
//	    WithMaxTokens(100_000).
//	    WithWarnPercent(0.8).
//	    WithHardPercent(1.0)
//	llmStage := stages.NewLLMStage(...)
//	guarded := TokenBudgetMiddleware(budget)(llmStage)
//
// 若需要在多个 Pipeline（多个 Bot）间共享同一份状态以便统一重置：
//
//	state := NewTokenBudgetState(time.Hour)
//	guarded := TokenBudgetMiddlewareWithState(budget, state)(llmStage)
// ============================================================================

// TokenBudgetConfig 配置 token 预算策略。
type TokenBudgetConfig struct {
	// MaxTokens 每个会话（Channel）允许的最大 token 数。0 = 不限制。
	MaxTokens int
	// WarnPercent 软警告阈值（0.0-1.0）。0 = 不警告。
	WarnPercent float64
	// HardPercent 硬限制阈值（0.0-1.0）。超限时中止请求。0 = 不限制。
	HardPercent float64

	// StatsRecorder 可选的 stats 记录器，用于记录预算告警/超限事件。
	StatsRecorder llm.UsageRecorder
}

// NewTokenBudgetConfig 返回默认预算配置（10 万 token，80% 警告，100% 硬限制）。
func NewTokenBudgetConfig() TokenBudgetConfig {
	return TokenBudgetConfig{
		MaxTokens:   100_000,
		WarnPercent: 0.8,
		HardPercent: 1.0,
	}
}

// WithMaxTokens 设置最大 token 数。
func (c TokenBudgetConfig) WithMaxTokens(n int) TokenBudgetConfig {
	c.MaxTokens = n
	return c
}

// WithWarnPercent 设置软警告阈值。
func (c TokenBudgetConfig) WithWarnPercent(p float64) TokenBudgetConfig {
	c.WarnPercent = p
	return c
}

// WithHardPercent 设置硬限制阈值。
func (c TokenBudgetConfig) WithHardPercent(p float64) TokenBudgetConfig {
	c.HardPercent = p
	return c
}

// WithStatsRecorder 注入 stats 记录器，超限事件自动记录。
func (c TokenBudgetConfig) WithStatsRecorder(r llm.UsageRecorder) TokenBudgetConfig {
	c.StatsRecorder = r
	return c
}

// IsZero 判断配置是否为空（所有字段为零值）。
func (c TokenBudgetConfig) IsZero() bool {
	return c.MaxTokens == 0 && c.WarnPercent == 0 && c.HardPercent == 0
}

// TokenBudgetState 是 TokenBudgetMiddleware 的共享状态（按 channel 追踪 token 用量）。
//
// 通过 NewTokenBudgetState 创建后可被多个 Pipeline 共享，并支持：
//   - 空闲超过 resetTTL 自动清零（防止预算永久卡死）
//   - 手动 ResetChannel / ResetAll 强制清零
type TokenBudgetState struct {
	mu       sync.Mutex
	usage    map[string]*llm.Usage // key: channel
	warned   map[string]bool       // key: channel，防止重复警告
	lastUsed map[string]time.Time  // key: channel，上次活跃时间（用于空闲重置）
	resetTTL time.Duration         // 空闲超过该时长则自动重置（0 = 不自动重置）
}

// NewTokenBudgetState 创建共享预算状态。resetTTL 为 0 表示不自动重置。
func NewTokenBudgetState(resetTTL time.Duration) *TokenBudgetState {
	return &TokenBudgetState{
		usage:    make(map[string]*llm.Usage),
		warned:   make(map[string]bool),
		lastUsed: make(map[string]time.Time),
		resetTTL: resetTTL,
	}
}

// TokenBudgetMiddleware 返回一个 Middleware，用于包装 LLMStage 并追踪 token 预算。
//
// 该版本为每个 Middleware 实例创建独立的内部状态（彼此不共享，也无法从外部重置）。
// 若需要共享状态或手动重置，请使用 TokenBudgetMiddlewareWithState。
//
// Before: 检查累积 token 是否超限，超限时注入警告或返回 PipelineError。
// After:  从 llm.result 中提取 Usage 并累积到 session tracker。
func TokenBudgetMiddleware(cfg TokenBudgetConfig) Middleware {
	return TokenBudgetMiddlewareWithState(cfg, nil)
}

// TokenBudgetMiddlewareWithState 与 TokenBudgetMiddleware 行为一致，但使用调用方提供的
// 共享 TokenBudgetState（便于跨 Pipeline 共享与手动重置）。
func TokenBudgetMiddlewareWithState(cfg TokenBudgetConfig, shared *TokenBudgetState) Middleware {
	if cfg.IsZero() {
		// 未配置则透传
		return func(next core.Stage) core.Stage { return next }
	}

	var state *TokenBudgetState
	if shared != nil {
		state = shared
	} else {
		state = NewTokenBudgetState(0)
	}

	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name(),
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				channel := env.Message.Channel
				if channel == "" {
					return next.Process(ctx, env)
				}

				// ---- Before: 检查预算 ----
				hardLimit := int(float64(cfg.MaxTokens) * cfg.HardPercent)
				warnLimit := int(float64(cfg.MaxTokens) * cfg.WarnPercent)

				state.mu.Lock()
				// 空闲自动重置：超过 TTL 视为新会话，清零预算
				if state.resetTTL > 0 {
					if lu, ok := state.lastUsed[channel]; ok && time.Since(lu) > state.resetTTL {
						delete(state.usage, channel)
						delete(state.warned, channel)
					}
				}
				current := state.usage[channel]
				if current == nil {
					current = &llm.Usage{}
					state.usage[channel] = current
				}
				currentTotal := current.TotalTokens
				wasWarned := state.warned[channel]
				state.lastUsed[channel] = time.Now()
				state.mu.Unlock()

				// 硬限制：超限时不永久中止，而是重置该 channel 的计数后继续处理。
				//
				// 原逻辑在跨过阈值后直接 return AbortError，且计数器永不清零，
				// 导致持续使用的会话（Bot 几乎不空闲）被永久卡死、所有新消息都被秒拒。
				// 改为：触顶即重置计数窗口，本条消息照常处理，
				// 既避免"永久回复失败"，又由 LoopDetection 中间件继续负责拦截失控循环。
				// （80% 软警告已提前提示 LLM 收尾，此处无需重复注入警告。）
				if hardLimit > 0 && currentTotal >= hardLimit {
					state.mu.Lock()
					delete(state.usage, channel)
					delete(state.warned, channel)
					state.lastUsed[channel] = time.Now()
					state.mu.Unlock()
				}

				// 硬警告：如果已经接近硬限制（90%）
				hardWarnThreshold := int(float64(hardLimit) * 0.9)
				if hardLimit > 0 && currentTotal >= hardWarnThreshold && currentTotal < hardLimit {
					core.QueueWarning(env, core.Warning{
						Source: "token_budget",
						Level:  core.WarningLevelHard,
						Message: fmt.Sprintf("CRITICAL: Token budget nearly exhausted (%d/%d). You MUST stop making tool calls and produce your final answer NOW.",
							currentTotal, hardLimit),
					})
				} else if warnLimit > 0 && currentTotal >= warnLimit && !wasWarned {
					// 软警告：注入提示（与硬警告互斥）
					core.QueueWarning(env, core.Warning{
						Source: "token_budget",
						Level:  core.WarningLevelSoft,
						Message: fmt.Sprintf("Token budget usage at %.0f%% (%d/%d). Wrap up your current work and produce a final answer. Avoid starting new tool calls unless absolutely necessary.",
							float64(currentTotal)/float64(cfg.MaxTokens)*100, currentTotal, cfg.MaxTokens),
					})
					state.mu.Lock()
					state.warned[channel] = true
					state.mu.Unlock()

					// 记录预算告警事件到 stats
					if cfg.StatsRecorder != nil {
						cfg.StatsRecorder.RecordUsage(ctx, llm.UsageMetric{
							BotID:   env.Message.BotID,
							At:      time.Now(),
							Feature: "budget_warning",
							Channel: channel,
						})
					}
				}

				// ---- 执行 ----
				result, err := next.Process(ctx, env)

				// ---- After: 累积用量 ----
				if result != nil {
					if v, ok := result.Get("llm.result"); ok {
						if genResult, ok := v.(*llm.GenerateResult); ok && genResult != nil {
							state.mu.Lock()
							acc := state.usage[channel]
							if acc == nil {
								acc = &llm.Usage{}
								state.usage[channel] = acc
							}
							acc.Add(&genResult.Usage)
							state.lastUsed[channel] = time.Now()
							state.mu.Unlock()
						}
					}
				}

				return result, err
			},
		}
	}
}

// TokenBudgetSnapshot 返回某 channel 的当前预算使用情况。
// 返回值：(已用 tokens, 是否存在记录)。
func (s *TokenBudgetState) Snapshot(channel string) (total int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, exists := s.usage[channel]
	if !exists || u == nil {
		return 0, false
	}
	return u.TotalTokens, true
}

// ResetChannel 重置某 channel 的预算追踪（如会话结束或手动重置）。
func (s *TokenBudgetState) ResetChannel(channel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.usage, channel)
	delete(s.warned, channel)
	delete(s.lastUsed, channel)
}

// ResetAll 重置所有 channel 的预算追踪（用于解除预算永久卡死）。
func (s *TokenBudgetState) ResetAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = make(map[string]*llm.Usage)
	s.warned = make(map[string]bool)
	s.lastUsed = make(map[string]time.Time)
}
