// Package engagement config 桥接层。
//
// 将 config.Store 中的 engagement.* 配置项转换为 engagement 模块内部的
// 各个组件（WritableChecker、RuleEngine、RateLimitRule、TimingGateConfig、StageConfig）。
//
// 使用方式：
//
//	cfg := builder.GetEngagementConfig()
//	policy := engagement.BuildPolicy(cfg, botUserID, nil /* judge */)
//	gate := engagement.BuildTimingGate(cfg, policy)
//	stage := engagement.NewEngagementStage("engagement", policy, ...).WithTimingGate(gate)

package engagement

import (
	"time"

	"github.com/kasuganosora/thinkbot/config"
)

// BuildWritableChecker 从 EngagementConfig 构建 Tier 0 渠道可写性检查器。
//
// 规则：
//   - cfg.Enabled == false → DenyAll（彻底禁用）
//   - cfg.Channels 非空 → SourceAllowlist（只允许指定渠道）
//   - cfg.Channels 为空但 Enabled → AllowAll（全部渠道参与）
func BuildWritableChecker(cfg config.EngagementConfig) WritableChecker {
	if !cfg.Enabled {
		return DenyAll{}
	}
	if len(cfg.Channels) == 0 {
		return AllowAll{}
	}
	return NewSourceAllowlist(cfg.Channels...)
}

// BuildRuleEngine 从 EngagementConfig 构建 Tier 1 规则引擎。
//
// 组装的规则（按执行顺序）：
//  1. SelfExclusionRule — 排除 Bot 自己的消息
//  2. RenoteExclusionRule — 排除纯转发/Boost（Misskey）
//  3. BlocklistRule — 用户/来源黑名单
//  4. LengthRule — 消息长度过滤
//  5. KeywordRule — 关键词匹配（配置了关键词时）
//  6. CooldownRule — 用户冷却
//  7. RateLimitRule — 令牌桶限流
//
// botUserID 传入 Bot 的用户 ID 用于自我排除。
// rateLimitRule 返回给调用方，Stage 需要在确定参与后调用 Consume()。
func BuildRuleEngine(cfg config.EngagementConfig, botUserID string) (engine *RuleEngine, rateLimit *RateLimitRule) {
	rules := make([]Rule, 0, 8)

	// 自我排除
	if botUserID != "" {
		rules = append(rules, NewSelfExclusionRule(botUserID))
	}

	// 纯转发排除
	rules = append(rules, NewRenoteExclusionRule())

	// 黑名单
	if len(cfg.BlockedUsers) > 0 || len(cfg.BlockedSources) > 0 {
		rules = append(rules, NewBlocklistRule(cfg.BlockedUsers, cfg.BlockedSources))
	}

	// 长度过滤
	if cfg.MinLength > 0 || cfg.MaxLength > 0 {
		rules = append(rules, NewLengthRule(cfg.MinLength, cfg.MaxLength))
	}

	// 关键词匹配
	if len(cfg.Keywords) > 0 {
		rules = append(rules, NewKeywordRule(cfg.Keywords...))
	}

	// 用户冷却
	if cfg.Cooldown > 0 {
		rules = append(rules, NewCooldownRule(cfg.Cooldown))
	}

	// 令牌桶限流
	if cfg.RateLimitCapacity > 0 {
		bucket := NewTokenBucket(cfg.RateLimitCapacity, cfg.RateLimitInterval)
		rateLimit = NewRateLimitRule(bucket)
		rules = append(rules, rateLimit)
	}

	engine = NewRuleEngine(rules...)
	return engine, rateLimit
}

// BuildTimingGateConfig 从 EngagementConfig 构建 TimingGateConfig。
func BuildTimingGateConfig(cfg config.EngagementConfig) TimingGateConfig {
	return TimingGateConfig{
		ReplyProbability:          cfg.ReplyProbability,
		BackoffBaseSeconds:        cfg.BackoffBaseSeconds,
		BackoffCapSeconds:         cfg.BackoffCapSeconds,
		BackoffStartCount:         cfg.BackoffStartCount,
		BurstIntervalSeconds:      cfg.BurstIntervalSeconds,
		WaitTimeoutSeconds:        cfg.WaitTimeoutSeconds,
		BackoffBypassPendingCount: cfg.BackoffBypassPendingCount,
		// 以下字段使用 DefaultTimingGateConfig 的值（不适合运行时调整）
		IdleCompensationMinInterval: 30.0,
		IdleCompensationWindow:      30 * time.Minute,
		EngagedResetDecline:         true,
		FrequencyMultiplier:         1.0,
	}
}

// BuildStageConfig 从 EngagementConfig 构建 StageConfig。
func BuildStageConfig(_ config.EngagementConfig) StageConfig {
	return DefaultStageConfig()
}

// PolicyBuildResult 包含从配置构建的全部 engagement 组件。
// 调用方一次性获取所有组件，无需分别构建。
type PolicyBuildResult struct {
	// Policy 组合策略（Tier 0+1+2）。
	Policy *CompositePolicy
	// Gate 有状态时序门控（可能为 nil，当 ReplyProbability<=0 时）。
	Gate *TimingGate
	// RateLimit 限流规则引用（供 Stage 调用 Consume）。
	RateLimit *RateLimitRule
}

// BuildFromConfig 从 config.EngagementConfig 一次性构建全部 engagement 组件。
//
// judge 为 nil 且 cfg.LLMJudgeEnabled 为 true 时，跳过 Tier 2（不调用 LLM）。
// 调用方应在构建时注入 SimpleJudge 实例。
//
// 示例：
//
//	cfg := builder.GetEngagementConfig()
//	result := engagement.BuildFromConfig(cfg, botUserID, judge)
//	stage := engagement.NewEngagementStage("engagement", result.Policy, stageCfg, tp, logger)
//	if result.Gate != nil {
//	    stage.WithTimingGate(result.Gate)
//	}
func BuildFromConfig(cfg config.EngagementConfig, botUserID string, judge LLMJudge) PolicyBuildResult {
	checker := BuildWritableChecker(cfg)
	rules, rateLimit := BuildRuleEngine(cfg, botUserID)

	opts := []PolicyOption{WithRules(rules)}
	if cfg.LLMJudgeEnabled && judge != nil {
		opts = append(opts, WithJudge(judge))
	}

	policy := NewCompositePolicy(checker, opts...)

	var gate *TimingGate
	if cfg.ReplyProbability > 0 {
		gate = NewTimingGate(policy, BuildTimingGateConfig(cfg))
	}

	return PolicyBuildResult{
		Policy:    policy,
		Gate:      gate,
		RateLimit: rateLimit,
	}
}
