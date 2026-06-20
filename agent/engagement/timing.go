package engagement

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Action — 三状态决策（参考 MaiBot Timing Gate）
// ============================================================================

// Action 是 Timing Gate 的三状态决策。
//
// 参考 MaiBot 的 Timing Gate 设计：
//   - Continue: 参与交互，进入完整 Pipeline
//   - NoAction: 保持静默，等待新消息（本条不值得回应）
//   - Wait: 刻意暂停，设定一个延时后重新评估（对话还在进行，但此刻插嘴不合适）
type Action string

const (
	// ActionContinue 参与交互。
	ActionContinue Action = "continue"
	// ActionNoAction 保持静默。
	ActionNoAction Action = "no_action"
	// ActionWait 刻意暂停，稍后再评估。
	ActionWait Action = "wait"
)

// ============================================================================
// TimingGate — 有状态的时序门控（参考 MaiBot 的 Timing Gate + 频率模型）
// ============================================================================

// TimingGate 在 EngagementPolicy 之上增加有状态的时序控制：
//
//   - 概率频率门控（talk_value）：随机决定是否评估，模拟"每 N 条消息参与一次"
//   - 连续 no_action 退避：连续 N 次不参与后指数退避，避免对安静频道反复评估
//   - 消息突发检测（debounce）：连发消息只评估最后一条
//   - Wait 状态超时重评估：ActionWait 后设定计时，超时后重新允许评估
//
// TimingGate 是线程安全的。
type TimingGate struct {
	policy EngagementPolicy
	config TimingGateConfig

	mu sync.Mutex
	// per-channel state
	channelStates map[string]*channelTimingState
	// onWaitExpired 在 wait 超时后被调用（可选）。
	// 调用方可在此重新投递一条合成消息触发重新评估。
	// 参考 MaiBot 的 _schedule_wait_timeout 回调。
	onWaitExpired  func(channelKey string)
	waitTimers     map[string]*time.Timer
}

type channelTimingState struct {
	// consecutiveDecline 连续不参与次数（用于指数退避）
	consecutiveDecline int
	// backoffUntil 退避到期时间
	backoffUntil time.Time
	// lastEvalAt 最近一次评估时间
	lastEvalAt time.Time
	// lastMsgAt 最近一条消息时间（用于突发检测）
	lastMsgAt time.Time
	// waitUntil wait 状态的到期时间
	waitUntil time.Time
	// msgCountSinceLastEngage 自上次参与以来的消息计数（用于频率门控）
	msgCountSinceLastEngage int
	// recentIntervals 最近消息间隔样本（用于空闲补偿）
	recentIntervals []float64
	// lastExternalMsgAt 最近外部消息时间
	lastExternalMsgAt time.Time
	// pendingMsgCount 待处理（被退避/突发跳过）的消息计数
	// 用于退避绕过机制
	pendingMsgCount int
}

// TimingGateConfig 配置 TimingGate 的时序控制参数。
type TimingGateConfig struct {
	// ReplyProbability 主动参与概率（0.0~1.0）。
	// 参考 MaiBot 的 talk_value。
	// 0.1 = 大约每 10 条消息尝试参与一次。
	// 0 表示禁用主动参与。
	// 默认 0.15。
	ReplyProbability float64

	// BackoffBaseSeconds no_action 退避基准秒数。
	// 连续 N 次不参与后，退避 = base * 2^(N-startCount)。
	BackoffBaseSeconds float64

	// BackoffCapSeconds 退避上限秒数。
	BackoffCapSeconds float64

	// BackoffStartCount 从第几次连续 decline 开始退避。
	// 默认 3——前 3 次不参与不退避，第 4 次开始。
	BackoffStartCount int

	// BurstIntervalSeconds 消息突发检测窗口。
	// 两条消息间隔小于此值视为同一突发。
	// 默认 5 秒。
	BurstIntervalSeconds float64

	// WaitTimeoutSeconds ActionWait 的默认超时。
	// 默认 30 秒。
	WaitTimeoutSeconds float64

	// IdleCompensationMinInterval 空闲补偿使用的最小平均间隔（秒）。
	// 避免高频对话把沉默时间折算成过多等效消息。
	// 默认 30 秒。
	IdleCompensationMinInterval float64

	// IdleCompensationWindow 空闲补偿统计窗口。
	// 默认 30 分钟。
	IdleCompensationWindow time.Duration

	// EngagedResetDecline 在成功参与后重置连续 decline 计数。
	// 默认 true。
	EngagedResetDecline bool

	// BackoffBypassPendingCount 待处理消息数绕过退避的阈值。
	// 当同一渠道的待处理消息数达到此值时，绕过退避立即评估。
	// 0 表示禁用绕过。
	// 参考 MaiBot 的 _no_action_backoff_bypass_pending_count。
	BackoffBypassPendingCount int

	// FrequencyMultiplier 运行时频率倍率（默认 1.0）。
	// 通过 AdjustFrequency 动态调整。
	// 最终概率 = ReplyProbability * FrequencyMultiplier。
	// 参考 MaiBot 的 adjust_talk_frequency。
	FrequencyMultiplier float64
}

// DefaultTimingGateConfig 返回默认配置。
func DefaultTimingGateConfig() TimingGateConfig {
	return TimingGateConfig{
		ReplyProbability:            0.15,
		BackoffBaseSeconds:          10.0,
		BackoffCapSeconds:           300.0,
		BackoffStartCount:           3,
		BurstIntervalSeconds:        5.0,
		WaitTimeoutSeconds:          30.0,
		IdleCompensationMinInterval: 30.0,
		IdleCompensationWindow:      30 * time.Minute,
		EngagedResetDecline:         true,
		BackoffBypassPendingCount:   0,
		FrequencyMultiplier:         1.0,
	}
}

// NewTimingGate 创建有状态的时序门控。
func NewTimingGate(policy EngagementPolicy, config TimingGateConfig) *TimingGate {
	config.normalize()
	return &TimingGate{
		policy:        policy,
		config:        config,
		channelStates: make(map[string]*channelTimingState),
		waitTimers:    make(map[string]*time.Timer),
	}
}

// normalize 确保 TimingGateConfig 中未设置的字段使用合理默认值。
func (c *TimingGateConfig) normalize() {
	if c.ReplyProbability < 0 {
		c.ReplyProbability = 0
	}
	if c.ReplyProbability > 1 {
		c.ReplyProbability = 1
	}
	if c.BackoffStartCount < 0 {
		c.BackoffStartCount = 3
	}
	if c.BurstIntervalSeconds < 0 {
		c.BurstIntervalSeconds = 5.0
	}
	if c.BackoffBaseSeconds < 0 {
		c.BackoffBaseSeconds = 10.0
	}
	if c.BackoffCapSeconds <= 0 {
		c.BackoffCapSeconds = 300.0
	}
	if c.WaitTimeoutSeconds <= 0 {
		c.WaitTimeoutSeconds = 30.0
	}
	if c.IdleCompensationMinInterval <= 0 {
		c.IdleCompensationMinInterval = 30.0
	}
	if c.IdleCompensationWindow <= 0 {
		c.IdleCompensationWindow = 30 * time.Minute
	}
	if c.BackoffBypassPendingCount < 0 {
		c.BackoffBypassPendingCount = 0
	}
	if c.FrequencyMultiplier <= 0 {
		c.FrequencyMultiplier = 1.0
	}
}

// TimingDecision 是 TimingGate 的完整决策结果。
type TimingDecision struct {
	// Action 三状态决策。
	Action Action
	// PolicyDecision 底层 policy 的原始决策（如果经过了评估）。
	PolicyDecision Decision
	// Reason 决策原因。
	Reason string
	// IsBurst 是否因为突发检测跳过了本次评估。
	IsBurst bool
	// IsBackoff 是否因为退避跳过了本次评估。
	IsBackoff bool
	// IsProbabilitySkip 是否因为概率门控跳过了本次评估。
	IsProbabilitySkip bool
	// WaitDuration 如果 Action=wait，建议的等待时长。
	WaitDuration time.Duration
}

// ShouldEvaluate 判断是否应该对这条消息运行 policy 评估。
// 这是 TimingGate 的核心方法，在 policy 之前执行。
func (g *TimingGate) ShouldEvaluate(msg *core.Message) (shouldEval bool, td TimingDecision) {
	channelKey := channelKeyForMessage(msg)
	now := time.Now()

	g.mu.Lock()
	state := g.getOrCreateState(channelKey)

	// 记录消息间隔
	if !state.lastExternalMsgAt.IsZero() {
		interval := now.Sub(state.lastExternalMsgAt).Seconds()
		if interval >= g.config.BurstIntervalSeconds {
			g.recordInterval(state, now, interval)
		}
	}
	state.lastExternalMsgAt = now
	state.msgCountSinceLastEngage++

	// --- Check 1: Wait 状态 ---
	if !state.waitUntil.IsZero() && now.Before(state.waitUntil) {
		g.mu.Unlock()
		return false, TimingDecision{
			Action:  ActionWait,
			Reason:  "in wait state, will re-evaluate after timeout",
			IsBurst: false,
			WaitDuration: state.waitUntil.Sub(now),
		}
	}
	// wait 已过期，清除
	if !state.waitUntil.IsZero() && now.After(state.waitUntil) {
		state.waitUntil = time.Time{}
	}

	// --- Check 2: 退避状态 ---
	// 私聊不退避（参考 MaiBot：no_action backoff 只对群聊生效）
	if !state.backoffUntil.IsZero() && now.Before(state.backoffUntil) && msg.ChatType != "private" {
		// 先增加待处理计数
		state.pendingMsgCount++
		// 检查退避绕过：待处理消息数达到阈值时绕过退避
		// 参考 MaiBot 的 _no_action_backoff_bypass_pending_count
		if g.config.BackoffBypassPendingCount > 0 && state.pendingMsgCount >= g.config.BackoffBypassPendingCount {
			// 绕过退避，重置状态
			state.backoffUntil = time.Time{}
			state.consecutiveDecline = 0
			state.pendingMsgCount = 0
		} else {
			g.mu.Unlock()
			return false, TimingDecision{
				Action:    ActionNoAction,
				Reason:    "in backoff period",
				IsBackoff: true,
			}
		}
	}
	// backoff 已过期，清除
	if !state.backoffUntil.IsZero() && now.After(state.backoffUntil) {
		state.backoffUntil = time.Time{}
	}

	// --- Check 3: 消息突发检测（debounce） ---
	if !state.lastMsgAt.IsZero() {
		sinceLast := now.Sub(state.lastMsgAt).Seconds()
		if sinceLast < g.config.BurstIntervalSeconds {
			state.lastMsgAt = now
			state.pendingMsgCount++
			g.mu.Unlock()
			return false, TimingDecision{
				Action:  ActionNoAction,
				Reason:  "message burst detected, debouncing",
				IsBurst: true,
			}
		}
	}
	state.lastMsgAt = now

	// --- Check 4: 概率频率门控 ---
	effectiveProb := g.config.ReplyProbability * g.config.FrequencyMultiplier
	if effectiveProb > 1.0 {
		effectiveProb = 1.0
	}
	if effectiveProb > 0 && effectiveProb < 1.0 {
		// 计算触发阈值：ceil(1.0 / probability)
		// 等效于 MaiBot 的 _get_message_trigger_threshold
		threshold := int(math.Ceil(1.0 / effectiveProb))
		if state.msgCountSinceLastEngage < threshold {
			// 尝试空闲补偿
			if !g.shouldTriggerByIdleCompensation(state, now, threshold) {
				state.pendingMsgCount++
				g.mu.Unlock()
				return false, TimingDecision{
					Action:            ActionNoAction,
					Reason:            "probability threshold not met",
					IsProbabilitySkip: true,
				}
			}
		}
	}

	state.lastEvalAt = now
	g.mu.Unlock()
	return true, TimingDecision{}
}

// RecordDecision 记录一次 policy 评估结果，更新内部状态。
func (g *TimingGate) RecordDecision(msg *core.Message, decision Decision) {
	channelKey := channelKeyForMessage(msg)
	now := time.Now()

	g.mu.Lock()
	defer g.mu.Unlock()

	state := g.getOrCreateState(channelKey)

	// 清除待处理计数（消息已被评估）
	state.pendingMsgCount = 0

	if decision.Engage {
		// 成功参与 → 重置退避
		if g.config.EngagedResetDecline {
			state.consecutiveDecline = 0
			state.backoffUntil = time.Time{}
		}
		state.msgCountSinceLastEngage = 0
		return
	}

	// 私聊不退避（参考 MaiBot：no_action backoff 只对群聊生效）
	if msg.ChatType == "private" {
		state.msgCountSinceLastEngage = 0
		return
	}

	// 不参与 → 增加退避计数
	state.consecutiveDecline++

	// 计算退避
	backoff := g.calculateBackoff(state.consecutiveDecline)
	if backoff > 0 {
		state.backoffUntil = now.Add(time.Duration(backoff * float64(time.Second)))
	}

	// 如果底层 policy 返回了 wait 相关信息，设置 wait 状态
	if action, ok := decision.Metadata["action"].(string); ok && action == string(ActionWait) {
		waitSec := g.config.WaitTimeoutSeconds
		if d, ok := decision.Metadata["wait_seconds"].(float64); ok && d > 0 {
			waitSec = d
		}
		state.waitUntil = now.Add(time.Duration(waitSec * float64(time.Second)))
		state.consecutiveDecline-- // wait 不算 decline

		// 启动 wait 超时定时器，到期后回调通知
		// 参考 MaiBot 的 _schedule_wait_timeout
		if g.onWaitExpired != nil {
			g.startWaitTimer(channelKey, state.waitUntil)
		}
	}
}

// Evaluate 是 ShouldEvaluate + policy.Evaluate + RecordDecision 的组合便捷方法。
func (g *TimingGate) Evaluate(ctx context.Context, msg *core.Message) TimingDecision {
	shouldEval, preDecision := g.ShouldEvaluate(msg)
	if !shouldEval {
		return preDecision
	}

	policyDecision := g.policy.Evaluate(ctx, msg)
	g.RecordDecision(msg, policyDecision)

	return TimingDecision{
		Action:         g.decisionToAction(policyDecision),
		PolicyDecision: policyDecision,
		Reason:         policyDecision.Reason,
	}
}

// SetWaitExpiredCallback 注册 wait 超时回调。
//
// 当 ActionWait 到期后，回调将被调用，参数为渠道 key。
// 调用方可在此重新投递合成消息或触发重新评估。
// 参考 MaiBot 的 _schedule_wait_timeout 自动投递 timeout 触发新一轮评估。
func (g *TimingGate) SetWaitExpiredCallback(cb func(channelKey string)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onWaitExpired = cb
}

// startWaitTimer 启动 wait 超时定时器。调用者需持有锁。
func (g *TimingGate) startWaitTimer(channelKey string, waitUntil time.Time) {
	// 取消已有定时器
	if old, ok := g.waitTimers[channelKey]; ok {
		old.Stop()
	}
	duration := time.Until(waitUntil)
	if duration <= 0 {
		return
	}
	timer := time.AfterFunc(duration, func() {
		g.mu.Lock()
		state, ok := g.channelStates[channelKey]
		if ok {
			state.waitUntil = time.Time{}
		}
		delete(g.waitTimers, channelKey)
		cb := g.onWaitExpired
		g.mu.Unlock()

		if cb != nil {
			cb(channelKey)
		}
	})
	g.waitTimers[channelKey] = timer
}

// AdjustFrequency 运行时调整回复频率倍率。
//
// multiplier > 1.0 提高回复频率，< 1.0 降低。
// 最终概率 = ReplyProbability * multiplier（封顶 1.0）。
// 参考 MaiBot 的 adjust_talk_frequency。
func (g *TimingGate) AdjustFrequency(multiplier float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if multiplier < 0 {
		multiplier = 0
	}
	g.config.FrequencyMultiplier = multiplier
}

// Close 清理所有定时器资源。
func (g *TimingGate) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, t := range g.waitTimers {
		t.Stop()
	}
	g.waitTimers = make(map[string]*time.Timer)
}

// ResetChannel 重置指定渠道的时序状态（例如手动恢复后）。
func (g *TimingGate) ResetChannel(channelKey string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t, ok := g.waitTimers[channelKey]; ok {
		t.Stop()
		delete(g.waitTimers, channelKey)
	}
	delete(g.channelStates, channelKey)
}

// GetChannelState 返回指定渠道的当前状态快照（用于调试/监控）。
func (g *TimingGate) GetChannelState(channelKey string) (consecutiveDecline int, inBackoff bool, inWait bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state, ok := g.channelStates[channelKey]
	if !ok {
		return 0, false, false
	}
	now := time.Now()
	return state.consecutiveDecline,
		!state.backoffUntil.IsZero() && now.Before(state.backoffUntil),
		!state.waitUntil.IsZero() && now.Before(state.waitUntil)
}

// ============================================================================
// internal helpers
// ============================================================================

// channelKeyForMessage 从消息中提取渠道 key（channel 优先，回退到 source）。
// 不同渠道独立维护状态。
func channelKeyForMessage(msg *core.Message) string {
	if msg.Channel != "" {
		return msg.Channel
	}
	return msg.Source
}

func (g *TimingGate) getOrCreateState(key string) *channelTimingState {
	state, ok := g.channelStates[key]
	if !ok {
		state = &channelTimingState{}
		g.channelStates[key] = state
	}
	return state
}

func (g *TimingGate) calculateBackoff(consecutiveDecline int) float64 {
	if consecutiveDecline < g.config.BackoffStartCount {
		return 0
	}
	exponent := consecutiveDecline - g.config.BackoffStartCount
	backoff := g.config.BackoffBaseSeconds * math.Pow(2, float64(exponent))
	if backoff > g.config.BackoffCapSeconds {
		return g.config.BackoffCapSeconds
	}
	return backoff
}

func (g *TimingGate) recordInterval(state *channelTimingState, _ time.Time, interval float64) {
	// 保留最近 100 个样本
	if len(state.recentIntervals) > 100 {
		state.recentIntervals = state.recentIntervals[len(state.recentIntervals)-100:]
	}
	state.recentIntervals = append(state.recentIntervals, interval)
}

func (g *TimingGate) shouldTriggerByIdleCompensation(state *channelTimingState, now time.Time, threshold int) bool {
	// 至少需要 1 条真实消息
	if state.msgCountSinceLastEngage < 1 {
		return false
	}

	var avgInterval float64
	if len(state.recentIntervals) == 0 {
		// 没有间隔样本时，使用最小平均间隔作为保守估计
		// 参考 MaiBot：当所有间隔被 burst 过滤后，回退到 IDLE_COMPENSATION_MIN_AVERAGE_INTERVAL_SECONDS
		avgInterval = g.config.IdleCompensationMinInterval
	} else {
		// 计算平均间隔
		total := 0.0
		for _, v := range state.recentIntervals {
			total += v
		}
		avgInterval = total / float64(len(state.recentIntervals))
	}
	if avgInterval < g.config.IdleCompensationMinInterval {
		avgInterval = g.config.IdleCompensationMinInterval
	}

	// 计算空闲时间折算为等效消息
	idleSeconds := 0.0
	if !state.lastExternalMsgAt.IsZero() {
		idleSeconds = now.Sub(state.lastExternalMsgAt).Seconds()
	}
	// 折算量封顶到 threshold - 1：确保至少需要 1 条真实消息
	maxEquivalent := float64(threshold - 1)
	if maxEquivalent < 0 {
		maxEquivalent = 0
	}
	idleEquivalent := idleSeconds / avgInterval
	if idleEquivalent > maxEquivalent {
		idleEquivalent = maxEquivalent
	}

	equivalentCount := float64(state.msgCountSinceLastEngage) + idleEquivalent
	return equivalentCount >= float64(threshold)
}

func (g *TimingGate) decisionToAction(d Decision) Action {
	if d.Engage {
		return ActionContinue
	}
	// 检查 metadata 中是否有 wait 标记
	if action, ok := d.Metadata["action"].(string); ok {
		if action == string(ActionWait) {
			return ActionWait
		}
	}
	return ActionNoAction
}

// ============================================================================
// ProbabilityRule — 将概率模型适配为 Tier 1 规则
// ============================================================================

// ProbabilityRule 以指定概率放行消息。
//
// 参考 MaiBot 的 talk_value 概念：
//   - probability=1.0 → 所有消息都放行
//   - probability=0.3 → 约 30% 的消息通过
//   - probability=0.0 → 全部拒绝
//
// 概率判定是确定性的——基于 msg.ID 的哈希，
// 同一条消息永远得到同一个结果（避免重复处理时行为不一致）。
type ProbabilityRule struct {
	probability float64
}

// NewProbabilityRule 创建概率规则。
func NewProbabilityRule(probability float64) *ProbabilityRule {
	if probability < 0 {
		probability = 0
	}
	if probability > 1 {
		probability = 1
	}
	return &ProbabilityRule{probability: probability}
}

// Allow 实现 Rule。
func (r *ProbabilityRule) Allow(msg *core.Message) (bool, string) {
	if r.probability >= 1.0 {
		return true, "probability=100%"
	}
	if r.probability <= 0 {
		return false, "probability=0%"
	}

	// 基于消息 ID + 概率做确定性判定
	// 使用 FNV hash 保证同一消息结果稳定
	hash := fnvHash(msg.ID + msg.Text)
	threshold := uint32(r.probability * float64(math.MaxUint32))
	if hash < threshold {
		return true, "probability passed"
	}
	return false, "probability rejected"
}

// fnvHash 简单的 FNV-1a 32-bit 哈希。
func fnvHash(s string) uint32 {
	const (
		offsetBasis32 = 2166136261
		prime32       = 16777619
	)
	hash := uint32(offsetBasis32)
	for _, c := range s {
		hash ^= uint32(c)
		hash *= prime32
	}
	return hash
}

// ============================================================================
// BurstBuffer — 消息突发缓冲器
// ============================================================================

// BurstBuffer 收集同一渠道的连发消息，只保留最后一条进行评估。
//
// 当消息间隔小于 Window 时视为突发，新消息替换旧消息。
// 突发结束后（Window 内无新消息），通过 onMature 回调通知最后一条消息成熟。
//
// 这实现了 MaiBot 的 wait-and-settle 策略：
// 突发期间不评估，突发结束后评估最后一条（包含完整上下文）。
type BurstBuffer struct {
	mu       sync.Mutex
	window   time.Duration
	pending  map[string]*core.Message // channelKey → last message
	timers   map[string]*time.Timer
	onMature func(channelKey string, msg *core.Message) // 突发结束后回调
}

// NewBurstBuffer 创建突发缓冲器。
func NewBurstBuffer(window time.Duration) *BurstBuffer {
	return &BurstBuffer{
		window:  window,
		pending: make(map[string]*core.Message),
		timers:  make(map[string]*time.Timer),
	}
}

// Push 推入一条消息。如果距离上一条消息超过 Window，
// 返回上一条消息（已"成熟"，可以评估），同时缓存新的。
// 如果在 Window 内，替换缓存的消息（突发），返回 nil。
func (b *BurstBuffer) Push(msg *core.Message) (matured *core.Message) {
	key := msg.Channel
	if key == "" {
		key = msg.Source
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	old, exists := b.pending[key]

	if !exists {
		// 第一条消息，缓存并启动突发定时器
		b.pending[key] = msg
		b.startBurstTimer(key)
		return nil
	}

	// 检查间隔
	interval := msg.CreatedAt.Sub(old.CreatedAt)
	if interval < b.window {
		// 突发：替换，重置定时器
		b.pending[key] = msg
		b.resetBurstTimer(key)
		return nil
	}

	// 间隔足够，旧消息成熟
	delete(b.pending, key)
	b.stopBurstTimer(key)

	// 缓存新消息并启动新的突发定时器
	b.pending[key] = msg
	b.startBurstTimer(key)
	return old
}

// Flush 返回并清除指定渠道的所有待处理消息。
func (b *BurstBuffer) Flush(channelKey string) *core.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopBurstTimer(channelKey)
	msg, ok := b.pending[channelKey]
	if ok {
		delete(b.pending, channelKey)
	}
	return msg
}

// FlushAll 返回所有渠道的待处理消息。
func (b *BurstBuffer) FlushAll() map[string]*core.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 停止所有定时器
	for _, t := range b.timers {
		t.Stop()
	}
	b.timers = make(map[string]*time.Timer)
	result := make(map[string]*core.Message, len(b.pending))
	for k, v := range b.pending {
		result[k] = v
	}
	b.pending = make(map[string]*core.Message)
	return result
}

// SetOnMature 设置突发结束后回调。调用者持有锁。
func (b *BurstBuffer) SetOnMature(cb func(channelKey string, msg *core.Message)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onMature = cb
}

// startBurstTimer 启动突发定时器。调用者需持有锁。
// 突发窗口结束后，如果消息仍被缓存，通过 onMature 回调通知。
func (b *BurstBuffer) startBurstTimer(key string) {
	// 取消已有定时器
	b.stopBurstTimer(key)
	if b.onMature == nil {
		return
	}
	timer := time.AfterFunc(b.window, func() {
		b.mu.Lock()
		msg, ok := b.pending[key]
		if ok {
			delete(b.pending, key)
		}
		delete(b.timers, key)
		cb := b.onMature
		b.mu.Unlock()
		if ok && cb != nil {
			cb(key, msg)
		}
	})
	b.timers[key] = timer
}

// resetBurstTimer 重置突发定时器。调用者需持有锁。
func (b *BurstBuffer) resetBurstTimer(key string) {
	b.stopBurstTimer(key)
	b.startBurstTimer(key)
}

// stopBurstTimer 停止定时器并从 map 移除。调用者需持有锁。
func (b *BurstBuffer) stopBurstTimer(key string) {
	if t, ok := b.timers[key]; ok {
		t.Stop()
		delete(b.timers, key)
	}
}

// Close 清理所有定时器资源。
func (b *BurstBuffer) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.timers {
		t.Stop()
	}
	b.timers = make(map[string]*time.Timer)
	b.pending = make(map[string]*core.Message)
}

// 确保 ProbabilityRule 实现 Rule 接口
var _ Rule = (*ProbabilityRule)(nil)
