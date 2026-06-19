package engagement

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ReenqueueFunc 在 BurstBuffer 消息成熟后重新投递消息的回调类型。
// EngagementStage 的调用方（Pipeline）应提供此回调以重新处理成熟的消息。
type ReenqueueFunc func(env *core.Envelope)

// ============================================================================
// EngagementStage — Pipeline 主动参与 Stage
// ============================================================================

// EngagementStage 是 Pipeline Stage，决定时间线帖子是否升级为可回复消息。
//
// 工作流程（参考 MaiBot 的 Timing Gate + 频率模型）：
//  1. 被直接 @ 的消息（Mentioned=true）直接放行，不做 engagement 判断
//  2. 对时间线帖子（Mentioned=false），先经过 TimingGate 时序门控：
//     a. 突发检测（debounce）→ 等连发结束再评估
//     b. 退避检测（backoff）→ 连续不参与后指数退避
//     c. 概率频率（talk_value）→ 按配置概率决定是否评估
//  3. 时序门控通过后，运行 EngagementPolicy 评估
//  4. 评估通过 → 设置 Mentioned=true + engagement 元数据，让下游正常处理
//  5. 评估未通过 → 不修改，消息继续流转
//
// Pipeline 位置：Order=40（在 Filter(20) 之后、Session(50) 之前）
type EngagementStage struct {
	name      string
	policy    EngagementPolicy
	gate      *TimingGate // 有状态时序门控（可选，nil 则跳过时序控制）
	burstBuf  *BurstBuffer // 突发缓冲器（可选，nil 则不缓冲）
	config    StageConfig
	tracer    trace.Tracer
	logger    *zap.SugaredLogger
}

// StageConfig 配置 EngagementStage。
type StageConfig struct {
	// ConsumeTokenOnEngage 在决定参与时是否消耗限流令牌。
	// 默认 true——参与的帖子才消耗令牌。
	ConsumeTokenOnEngage bool
}

// DefaultStageConfig 返回默认 Stage 配置。
func DefaultStageConfig() StageConfig {
	return StageConfig{
		ConsumeTokenOnEngage: true,
	}
}

// NewEngagementStage 创建 Engagement Pipeline Stage。
//
// policy 为 nil 时，stage 对所有非 @ 消息直接放行（不做 engagement）。
// gate 为 nil 时，跳过时序门控，直接评估每条消息。
func NewEngagementStage(
	name string,
	policy EngagementPolicy,
	config StageConfig,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *EngagementStage {
	if name == "" {
		name = "engagement"
	}
	return &EngagementStage{
		name:   name,
		policy: policy,
		config: config,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/engagement"),
		logger: logger.With("component", "engagement_stage"),
	}
}

// WithTimingGate 注入有状态的时序门控。
// 调用后，Stage 在评估每条消息前先经过 TimingGate 检查。
func (s *EngagementStage) WithTimingGate(gate *TimingGate) *EngagementStage {
	s.gate = gate
	return s
}

// WithBurstBuffer 注入突发缓冲器。
// 调用后，Stage 在处理每条消息前先经过 BurstBuffer 检查：
//   - 突发期间的消息被缓存，Stage 直接返回（不评估）
//   - 突发结束后最后一条消息通过 reenqueue 重新投递评估
//
// 这实现了 MaiBot 的 wait-and-settle 策略：
//   评估突发后的最后一条消息（通常包含最完整上下文）。
func (s *EngagementStage) WithBurstBuffer(buf *BurstBuffer, reenqueue ReenqueueFunc) *EngagementStage {
	s.burstBuf = buf
	if buf != nil && reenqueue != nil {
		buf.SetOnMature(func(channelKey string, msg *core.Message) {
			// 突发结束后，重新投递成熟的消息
			env := core.NewEnvelope(*msg)
			s.logger.Debugw("burst buffer matured, re-enqueueing",
				"message_id", msg.ID,
				"channel_key", channelKey)
			reenqueue(env)
		})
	}
	return s
}

// Name 返回 Stage 名称。
func (s *EngagementStage) Name() string { return s.name }

// Process 执行 engagement 评估。
func (s *EngagementStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	// 无策略配置 → 直接放行（engagement 功能未启用）
	if s.policy == nil {
		return env, nil
	}

	ctx, span := s.tracer.Start(ctx, "stage.engagement.process",
		trace.WithAttributes(
			attribute.String("message.id", env.Message.ID),
			attribute.String("message.source", env.Message.Source),
			attribute.String("trace.id", traceid.FromContext(ctx)),
		))
	defer span.End()

	// 派生携带 trace_id 的 logger，使所有日志可通过 trace_id 关联
	logger := traceid.WithLoggerFrom(ctx, s.logger)

	// 1. 被直接 @ 的消息直接放行——不需要 engagement 决策
	//    参考 MaiBot 的 _arm_force_next_timing_continue：@ 消息直接跳过 Timing Gate
	if env.Message.Mentioned {
		// 被 @ 的消息也刷新 BurstBuffer（清除突发缓存）
		if s.burstBuf != nil {
			s.burstBuf.Flush(channelKeyForMessage(&env.Message))
		}
		span.SetAttributes(attribute.Bool("engagement.skipped", true))
		env.Set("engagement.evaluated", false)
		return env, nil
	}

	// 1.5 BurstBuffer 突发缓冲（可选）
	//     参考 MaiBot 的 wait-and-settle 策略：
	//     突发期间缓存消息，突发结束后通过 onMature 回调重新投递最后一条。
	if s.burstBuf != nil {
		matured := s.burstBuf.Push(&env.Message)
		if matured == nil {
			// 当前消息被缓存（突发中），不评估
			env.Set("engagement.evaluated", false)
			env.Set("engagement.burst_buffered", true)
			span.SetAttributes(attribute.Bool("engagement.burst_buffered", true))
			logger.Debugw("burst buffer: message buffered",
				"message_id", env.Message.ID)
			return env, nil
		}
		// 有成熟消息——用它替换当前消息进行评估
		// （成熟消息是突发前最后一条，当前消息将被后续 Push 处理）
		env.Message = *matured
	}

	// 2. TimingGate 时序门控（可选）
	var decision Decision
	if s.gate != nil {
		td := s.gate.Evaluate(ctx, &env.Message)

		// 时序门控跳过了 policy 评估
		if td.Action != ActionContinue {
			env.Set("engagement.evaluated", true)
			env.Set("engagement.engage", false)
			env.Set("engagement.action", string(td.Action))
			env.Set("engagement.reason", td.Reason)
			if td.IsBurst {
				env.Set("engagement.is_burst", true)
			}
			if td.IsBackoff {
				env.Set("engagement.is_backoff", true)
			}
			if td.IsProbabilitySkip {
				env.Set("engagement.is_probability_skip", true)
			}

			span.SetAttributes(
				attribute.String("engagement.action", string(td.Action)),
				attribute.String("engagement.reason", td.Reason),
				attribute.Bool("engagement.is_burst", td.IsBurst),
				attribute.Bool("engagement.is_backoff", td.IsBackoff),
				attribute.Bool("engagement.is_probability_skip", td.IsProbabilitySkip),
			)

			logger.Debugw("engagement: timing gate deferred",
				"message_id", env.Message.ID,
				"action", td.Action,
				"reason", td.Reason)
			return env, nil
		}

		// 时序门控通过，获取 policy 的原始决策
		decision = td.PolicyDecision
	} else {
		// 无时序门控，直接评估
		decision = s.policy.Evaluate(ctx, &env.Message)
	}

	env.Set("engagement.evaluated", true)
	env.Set("engagement.engage", decision.Engage)
	env.Set("engagement.action", string(decision.Action))
	env.Set("engagement.reason", decision.Reason)
	env.Set("engagement.tier", string(decision.Tier))

	span.SetAttributes(
		attribute.Bool("engagement.engage", decision.Engage),
		attribute.String("engagement.action", string(decision.Action)),
		attribute.String("engagement.reason", decision.Reason),
		attribute.String("engagement.tier", string(decision.Tier)),
	)

	// 3. 评估未通过 → 消息继续流转（不修改 Mentioned）
	if !decision.Engage {
		logger.Debugw("engagement declined",
			"message_id", env.Message.ID,
			"action", decision.Action,
			"tier", decision.Tier,
			"reason", decision.Reason)
		return env, nil
	}

	// 4. 评估通过 → 升级为主动参与
	env.Message.Mentioned = true
	env.Set("engagement.proactive", true)

	// 确保 reply_target 存在（Bot 回复时需要知道回复到哪条帖子）
	// Misskey 等 channel 已在 metadata 中设置了 reply_target，
	// 但其他 channel 可能需要在这里补充
	if _, ok := env.Message.Metadata["reply_target"]; !ok {
		// 使用消息 ID 作为 reply_target（channel outbound 自行解释）
		if env.Message.ID != "" {
			env.Message.Metadata["reply_target"] = env.Message.ID
		}
	}

	// 消耗限流令牌
	if s.config.ConsumeTokenOnEngage {
		if cp, ok := s.policy.(*CompositePolicy); ok && cp.rules != nil {
			for _, rule := range cp.rules.rules {
				if rl, ok := rule.(*RateLimitRule); ok {
					rl.Consume()
				}
			}
		}
	}

	// 旁路事件
	emitter := outbound.EmitterFromContext(ctx)
	emitter.Emit(ctx, "engagement.engaged", env.Message.TraceID, map[string]any{
		"message_id": env.Message.ID,
		"reason":     decision.Reason,
		"tier":       string(decision.Tier),
		"user_id":    env.Message.UserID,
	})

	logger.Infow("engagement: proactive engagement",
		"message_id", env.Message.ID,
		"tier", decision.Tier,
		"reason", decision.Reason,
		"user_id", env.Message.UserID)

	return env, nil
}

// ============================================================================
// Envelope 辅助函数
// ============================================================================

// IsProactiveEngagement 检查 Envelope 是否标记为主动参与。
func IsProactiveEngagement(env *core.Envelope) bool {
	if v, ok := env.Get("engagement.proactive"); ok {
		if proactive, ok := v.(bool); ok {
			return proactive
		}
	}
	return false
}

// EngagementDecision 获取 Envelope 中的 engagement 评估结果。
// 如果未经过评估，返回零值。
func EngagementDecision(env *core.Envelope) Decision {
	var engaged bool
	if v, ok := env.Get("engagement.engage"); ok {
		engaged, _ = v.(bool)
	}
	var reason string
	if v, ok := env.Get("engagement.reason"); ok {
		reason, _ = v.(string)
	}
	var tier string
	if v, ok := env.Get("engagement.tier"); ok {
		tier, _ = v.(string)
	}
	return Decision{
		Engage: engaged,
		Reason: reason,
		Tier:   Tier(tier),
	}
}

// WasEvaluated 检查消息是否经过了 engagement 评估。
func WasEvaluated(env *core.Envelope) bool {
	v, ok := env.Get("engagement.evaluated")
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// EngagementSummary 返回决策的人类可读摘要（用于日志/debug）。
func EngagementSummary(env *core.Envelope) string {
	d := EngagementDecision(env)
	if !WasEvaluated(env) {
		return "not evaluated (mentioned or no policy)"
	}
	if d.Engage {
		return fmt.Sprintf("engaged (tier=%s, reason=%s)", d.Tier, d.Reason)
	}
	return fmt.Sprintf("declined (tier=%s, reason=%s)", d.Tier, d.Reason)
}
