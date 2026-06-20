// Package engagement 提供主动参与决策能力。
//
// Engagement 解决的问题：Bot 观察到时间线上的帖子（未被 @），如何决定
// 是否主动参与讨论？
//
// 三层漏斗设计，逐级过滤避免每条帖子都烧 LLM：
//
//	时间线帖子
//	    │
//	┌───▼───────────────┐
//	│ Tier 0: 能力检查    │  ← 渠道是否可写？毫秒级
//	│ 挡掉 RSS 等只读源    │
//	└───┬───────────────┘
//	    │
//	┌───▼───────────────┐
//	│ Tier 1: 规则引擎    │  ← 关键词/频率/冷却/黑名单，毫秒级
//	│ 挡掉 ~90% 噪音     │
//	└───┬───────────────┘
//	    │
//	┌───▼───────────────┐
//	│ Tier 2: LLM 快判   │  ← 小模型 YES/NO，可选
//	│ 挡掉剩余 ~70%      │
//	└───┬───────────────┘
//	    │ 值得参与
//	┌───▼───────────────┐
//	│ 提升 → 正常 Pipeline │
//	└───────────────────┘
//
// Pipeline 中的位置：
//
//	Filter(20) → Engagement(40) → Enricher(30→调整) → Session(50) → ...
//
// EngagementStage 在 Filter 之后、Session 之前：
//   - Filter 已经决定消息进入 Pipeline
//   - Engagement 决定时间线帖子是否"升级"为可回复消息
//   - Session 接收升级后的消息并创建会话
package engagement

import (
	"context"
	"fmt"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Decision — 评估结果
// ============================================================================

// Tier 标识做出决策的层级。
type Tier string

const (
	// TierCapability 渠道能力检查层（可写性）。
	TierCapability Tier = "capability"
	// TierRule 规则引擎层。
	TierRule Tier = "rule"
	// TierLLM LLM 快判层。
	TierLLM Tier = "llm"
	// TierPass 所有层通过。
	TierPass Tier = "pass"
)

// Decision 是评估一条消息是否值得主动参与的结果。
type Decision struct {
	// Engage 是否应该主动参与。
	Engage bool
	// Action 三状态决策（参考 MaiBot Timing Gate）。
	// 默认根据 Engage 推导：Engage=true → ActionContinue，false → ActionNoAction。
	// 可以显式设为 ActionWait 表示"此刻不适合，稍后再评估"。
	Action Action
	// Reason 决策原因（用于日志和调试）。
	Reason string
	// Tier 做出决策的层级。
	Tier Tier
	// Metadata 扩展信息（如匹配到的关键词、冷却剩余时间等）。
	Metadata map[string]any
}

// ============================================================================
// EngagementPolicy — 主动参与策略
// ============================================================================

// EngagementPolicy 评估一条消息并决定 Bot 是否应该主动参与。
//
// 实现应遵循三层漏斗：
//   - Tier 0: 检查渠道是否可写（CanReply）
//   - Tier 1: 规则预筛（关键词/频率/冷却/黑名单）
//   - Tier 2: LLM 快判（可选）
type EngagementPolicy interface {
	// Evaluate 评估消息，返回是否参与及原因。
	Evaluate(ctx context.Context, msg *core.Message) Decision
}

// ============================================================================
// ChannelCapability — 渠道可写性检查
// ============================================================================

// WritableChecker 判断消息来源渠道是否支持回复。
//
// 只读渠道（如 RSS 订阅源）永远不会参与讨论。
// 实现者应基于 msg.Source 或 msg.Metadata 判断。
type WritableChecker interface {
	// IsWritable 返回该消息来源是否支持回复。
	IsWritable(msg *core.Message) bool
}

// WritableCheckerFunc 函数适配器。
type WritableCheckerFunc func(msg *core.Message) bool

// IsWritable 实现 WritableChecker。
func (f WritableCheckerFunc) IsWritable(msg *core.Message) bool {
	return f(msg)
}

// SourceAllowlist 基于 msg.Source 白名单的可写性检查器。
//
// 只有 source 在白名单中的消息才被视为可写。
// 这是最常用的实现：用户明确配置哪些渠道支持主动参与。
//
// 用法：
//
//	checker := engagement.NewSourceAllowlist("misskey", "telegram")
type SourceAllowlist struct {
	allowed map[string]bool
}

// NewSourceAllowlist 创建基于来源白名单的可写性检查器。
func NewSourceAllowlist(sources ...string) *SourceAllowlist {
	m := make(map[string]bool, len(sources))
	for _, s := range sources {
		m[s] = true
	}
	return &SourceAllowlist{allowed: m}
}

// IsWritable 实现 WritableChecker。
func (a *SourceAllowlist) IsWritable(msg *core.Message) bool {
	return a.allowed[msg.Source]
}

// AllowAll 始终返回可写（用于测试或全渠道参与）。
type AllowAll struct{}

// IsWritable 实现 WritableChecker。
func (AllowAll) IsWritable(_ *core.Message) bool { return true }

// DenyAll 始终返回不可写（默认禁用 engagement）。
type DenyAll struct{}

// IsWritable 实现 WritableChecker。
func (DenyAll) IsWritable(_ *core.Message) bool { return false }

// ============================================================================
// 默认实现 — CompositePolicy
// ============================================================================

// CompositePolicy 组合三层漏斗的默认策略实现。
//
// 构造：
//   - checker: 渠道可写性检查（必需）
//   - rules: Tier 1 规则引擎（可选，nil 则跳过）
//   - judge: Tier 2 LLM 快判（可选，nil 则跳过）
//   - engagementThreshold: 评分阈值（可选，0=禁用评分模式）
type CompositePolicy struct {
	checker             WritableChecker
	rules               *RuleEngine
	judge               LLMJudge
	engagementThreshold int // 0=禁用，>0 时启用评分模式
}

// PolicyOption 配置 CompositePolicy。
type PolicyOption func(*CompositePolicy)

// WithRules 设置 Tier 1 规则引擎。
func WithRules(rules *RuleEngine) PolicyOption {
	return func(p *CompositePolicy) { p.rules = rules }
}

// WithJudge 设置 Tier 2 LLM 快判。
func WithJudge(judge LLMJudge) PolicyOption {
	return func(p *CompositePolicy) { p.judge = judge }
}

// WithEngagementThreshold 设置 Tier 2 评分阈值（0-100）。
// 当 > 0 且 judge 返回 Score > 0 时，只有 Score ≥ threshold 才通过。
// 参考 Houde et al. (2025)：评分阈值是群组中控制 AI 参与度最有效的方式。
func WithEngagementThreshold(threshold int) PolicyOption {
	return func(p *CompositePolicy) { p.engagementThreshold = threshold }
}

// NewCompositePolicy 创建组合策略。
func NewCompositePolicy(checker WritableChecker, opts ...PolicyOption) *CompositePolicy {
	if checker == nil {
		checker = DenyAll{}
	}
	p := &CompositePolicy{checker: checker}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Evaluate 实现 EngagementPolicy。
func (p *CompositePolicy) Evaluate(ctx context.Context, msg *core.Message) Decision {
	// Tier 0: 渠道可写性
	if !p.checker.IsWritable(msg) {
		return p.deriveAction(Decision{
			Engage: false,
			Reason: "channel not writable: " + msg.Source,
			Tier:   TierCapability,
		})
	}

	// Tier 1: 规则引擎
	if p.rules != nil {
		if !p.rules.Allow(msg) {
			return p.deriveAction(Decision{
				Engage: false,
				Reason: "rule engine rejected: " + p.rules.LastReason(),
				Tier:   TierRule,
			})
		}
	}

	// Tier 2: LLM 快判（可选）
	if p.judge != nil {
		judgeResult, err := p.judge.Judge(ctx, msg)
		if err != nil {
			return p.deriveAction(Decision{
				Engage: false,
				Reason: "llm judge error: " + err.Error(),
				Tier:   TierLLM,
			})
		}

		// 评分模式：使用 engagementThreshold 做阈值判断
		// 参考 Houde et al. (2025)：评分制 + 可配置阈值比二元判断更受用户认可
		if p.engagementThreshold > 0 && judgeResult.Score > 0 {
			if judgeResult.Score < p.engagementThreshold {
				return p.deriveAction(Decision{
					Engage: false,
					Reason: fmt.Sprintf("score %d < threshold %d: %s", judgeResult.Score, p.engagementThreshold, judgeResult.Reason),
					Tier:   TierLLM,
					Metadata: map[string]any{
						"score":     judgeResult.Score,
						"threshold": p.engagementThreshold,
					},
				})
			}
			// 分数通过阈值，继续到 all-pass
		} else if !judgeResult.Engage {
			// 传统二元模式
			return p.deriveAction(Decision{
				Engage: false,
				Reason: "llm judge declined: " + judgeResult.Reason,
				Tier:   TierLLM,
			})
		}
	}

	// 所有层通过
	return p.deriveAction(Decision{
		Engage: true,
		Reason: "all tiers passed",
		Tier:   TierPass,
	})
}

// deriveAction 在 Action 未显式设置时，根据 Engage 自动推导。
func (p *CompositePolicy) deriveAction(d Decision) Decision {
	if d.Action == "" {
		if d.Engage {
			d.Action = ActionContinue
		} else {
			d.Action = ActionNoAction
		}
	}
	return d
}
