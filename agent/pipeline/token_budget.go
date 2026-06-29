package pipeline

import (
	"context"
	"fmt"
	"sync"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// TokenBudgetMiddleware — 按会话追踪 Token 消耗，超限时注入警告或终止
//
// 借鉴 deer-flow 的 TokenBudgetMiddleware 设计：
//   - 软限制（warn）：注入警告提示 LLM "尽快总结"
//   - 硬限制（hard）：强制剥离 tool_calls / 中止当前请求
//
// 在 thinkbot 中的实现：
//   - 按 Channel（会话）累积 token 用量，跨多条消息持久跟踪
//   - 每次 Pipeline 执行前后检查预算
//   - 通过 Envelope KV 传递警告（延迟注入模式）
//
// 使用方式：
//
//	budget := NewTokenBudgetConfig().
//	    WithMaxTokens(100_000).
//	    WithWarnPercent(0.8).
//	    WithHardPercent(1.0)
//	llmStage := stages.NewLLMStage(...)
//	guarded := TokenBudgetMiddleware(budget)(llmStage)
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

// tokenBudgetState 是 TokenBudgetMiddleware 的内部状态。
type tokenBudgetState struct {
	mu     sync.Mutex
	usage  map[string]*llm.Usage // key: channel
	warned map[string]bool       // key: channel，防止重复警告
}

// TokenBudgetMiddleware 返回一个 Middleware，用于包装 LLMStage 并追踪 token 预算。
//
// Before: 检查累积 token 是否超限，超限时注入警告或返回 PipelineError。
// After:  从 llm.result 中提取 Usage 并累积到 session tracker。
func TokenBudgetMiddleware(cfg TokenBudgetConfig) Middleware {
	if cfg.IsZero() {
		// 未配置则透传
		return func(next core.Stage) core.Stage { return next }
	}

	state := &tokenBudgetState{
		usage:  make(map[string]*llm.Usage),
		warned: make(map[string]bool),
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
				current := state.usage[channel]
				if current == nil {
					current = &llm.Usage{}
					state.usage[channel] = current
				}
				currentTotal := current.TotalTokens
				wasWarned := state.warned[channel]
				state.mu.Unlock()

				// 硬限制：超限时中止
				if hardLimit > 0 && currentTotal >= hardLimit {
					return env, &core.AbortError{
						Reason: fmt.Sprintf("token budget hard limit exceeded: %d/%d", currentTotal, hardLimit),
						Cause:  fmt.Errorf("token budget exhausted"),
					}
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
func (s *tokenBudgetState) Snapshot(channel string) (total int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, exists := s.usage[channel]
	if !exists || u == nil {
		return 0, false
	}
	return u.TotalTokens, true
}

// ResetChannel 重置某 channel 的预算追踪（如会话结束或手动重置）。
func (s *tokenBudgetState) ResetChannel(channel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.usage, channel)
	delete(s.warned, channel)
}
