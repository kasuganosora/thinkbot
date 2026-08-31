package engagement

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// RejectionDetector — "被无视"检测器
//
// 检测 Bot 是否被群友无视，产生 rejection_streak 短期状态。
//
// 判定逻辑（C 方案 — 双重判定）：
//   1. Bot 回复后 N 秒内同一 channel 无任何后续消息
//   2. 如果有后续消息，检测是否包含对 Bot 的引用/回应
//   3. 两者都不满足 → 视为被无视一次
//   4. 连续被无视达到阈值（默认 3 次）→ 触发 rejection_streak，写入 L0 事件
//
// 触发后效果（由 AdaptiveEngagementSyncer 读取）：
//   - reply_probability 下调 50%
//   - backoff_start_count 降为 1
//   - 持续 1 小时后自动恢复
//
// RejectionDetector 通过订阅 EventBus 的 outbound 回复事件来追踪状态。
// ============================================================================

// RejectionDetector 检测 Bot 被无视的情况。
type RejectionDetector struct {
	mu sync.Mutex

	// perChannelState per-channel 被无视状态追踪。
	perChannelState map[string]*channelRejectionState

	// config 检测配置。
	config RejectionDetectorConfig

	// onRejectionStreak 回调：当连续被无视达到阈值时触发。
	onRejectionStreak func(channelKey string, streakCount int)

	// botRepliedAt per-channel Bot 最近回复时间戳（外部调用 RecordReply 设置）。
	botRepliedAt map[string]time.Time

	tracer trace.Tracer
	logger *zap.SugaredLogger

	// 可观测性指标（原子计数器）
	totalReplies      atomic.Int64 // 总回复数
	totalRejections   atomic.Int64 // 总被无视次数
	totalStreaks      atomic.Int64 // 总自闭次数
	activeStreakCount atomic.Int64 // 当前活跃的自闭 channel 数
}

// channelRejectionState per-channel 被无视状态。
type channelRejectionState struct {
	// consecutiveRejections 连续被无视次数。
	consecutiveRejections int
	// lastRejectionAt 最近一次被无视的时间。
	lastRejectionAt time.Time
	// streakActive 是否处于自闭模式。
	streakActive bool
	// streakStartedAt 自闭开始时间。
	streakStartedAt time.Time
	// pendingReply 最近一次回复是否还在被无视判定窗口内。
	pendingReply bool
	// postReplyMsgCount Bot 回复后 channel 中的新消息数。
	postReplyMsgCount int
	// botReferenced 后续消息中是否有人引用/回应了 Bot。
	botReferenced bool
}

// RejectionDetectorConfig 配置被无视检测器。
type RejectionDetectorConfig struct {
	// SilenceWindowSeconds Bot 回复后等待的沉默窗口（秒）。
	// 默认 120 秒（2 分钟）。
	SilenceWindowSeconds float64

	// StreakThreshold 连续被无视多少次后触发 rejection_streak。
	// 默认 3。
	StreakThreshold int

	// StreakDuration 自闭模式持续时长。
	// 默认 1 小时。
	StreakDuration time.Duration

	// ChannelType channel 类型标识（如 "telegram"）。
	ChannelType string

	// BotName Bot 的显示名称（用于检测消息中是否引用 Bot）。
	BotName string
}

// DefaultRejectionDetectorConfig 返回默认配置。
func DefaultRejectionDetectorConfig() RejectionDetectorConfig {
	return RejectionDetectorConfig{
		SilenceWindowSeconds: 120.0,
		StreakThreshold:      3,
		StreakDuration:       1 * time.Hour,
	}
}

// NewRejectionDetector 创建被无视检测器。
func NewRejectionDetector(config RejectionDetectorConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *RejectionDetector {
	if config.SilenceWindowSeconds <= 0 {
		config.SilenceWindowSeconds = 120.0
	}
	if config.StreakThreshold <= 0 {
		config.StreakThreshold = 3
	}
	if config.StreakDuration <= 0 {
		config.StreakDuration = 1 * time.Hour
	}
	return &RejectionDetector{
		perChannelState: make(map[string]*channelRejectionState),
		botRepliedAt:    make(map[string]time.Time),
		config:          config,
		tracer:          tp.Tracer("github.com/kasuganosora/thinkbot/agent/engagement/rejection_detector"),
		logger:          logger.With("component", "rejection_detector"),
	}
}

// Metrics 返回当前可观测性指标快照。
type RejectionMetrics struct {
	TotalReplies    int64 `json:"total_replies"`
	TotalRejections int64 `json:"total_rejections"`
	TotalStreaks    int64 `json:"total_streaks"`
	ActiveStreaks   int64 `json:"active_streaks"`
	ActiveChannels  int   `json:"active_channels"`
}

// Metrics 返回当前可观测性指标。
func (d *RejectionDetector) Metrics() RejectionMetrics {
	d.mu.Lock()
	active := len(d.perChannelState)
	d.mu.Unlock()
	return RejectionMetrics{
		TotalReplies:    d.totalReplies.Load(),
		TotalRejections: d.totalRejections.Load(),
		TotalStreaks:    d.totalStreaks.Load(),
		ActiveStreaks:   d.activeStreakCount.Load(),
		ActiveChannels:  active,
	}
}

// SetOnRejectionStreak 设置 rejection_streak 触发回调。
func (d *RejectionDetector) SetOnRejectionStreak(cb func(channelKey string, streakCount int)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onRejectionStreak = cb
}

// ============================================================================
// 公共 API
// ============================================================================

// RecordReply 记录 Bot 在某 channel 发送了一条回复。
// 应在 ChannelReplyHandler 成功发送消息后调用。
func (d *RejectionDetector) RecordReply(channelKey string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	state := d.getOrCreateState(channelKey)
	state.pendingReply = true
	state.postReplyMsgCount = 0
	state.botReferenced = false
	d.botRepliedAt[channelKey] = time.Now()
	d.totalReplies.Add(1)
}

// OnExternalMessage 通知检测器某 channel 收到一条外部（非 Bot）消息。
// 用于判断 Bot 回复后是否有人继续说话，以及是否有人引用 Bot。
func (d *RejectionDetector) OnExternalMessage(channelKey string, msg *core.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()

	state := d.getOrCreateState(channelKey)
	if !state.pendingReply {
		return
	}

	state.postReplyMsgCount++

	// 检测是否有人引用/回应 Bot
	if d.isBotReferenced(msg) {
		state.botReferenced = true
		// 有人回应了 → 清除被无视计数
		d.resetRejection(channelKey, state)
	}
}

// CheckSilence 检查指定 channel 是否静默超时（Bot 回复后无人说话）。
// 应在超时后由定时器或外部周期性调用。
func (d *RejectionDetector) CheckSilence(channelKey string) {
	var cb func(string, int)
	var cbChannelKey string
	var cbStreakCount int

	d.mu.Lock()

	state := d.getOrCreateState(channelKey)
	if !state.pendingReply {
		d.mu.Unlock()
		return
	}

	repliedAt, ok := d.botRepliedAt[channelKey]
	if !ok {
		d.mu.Unlock()
		return
	}

	silenceWindow := time.Duration(d.config.SilenceWindowSeconds * float64(time.Second))
	if time.Since(repliedAt) < silenceWindow {
		d.mu.Unlock()
		return
	}

	// 沉默窗口已过
	state.pendingReply = false

	// 判定：既无后续消息，又无人引用 Bot
	if state.postReplyMsgCount == 0 || !state.botReferenced {
		cb, cbChannelKey, cbStreakCount = d.recordRejection(channelKey, state)
	}

	d.mu.Unlock()

	// 在锁外调用回调，避免死锁
	if cb != nil {
		cb(cbChannelKey, cbStreakCount)
	}
}

// IsInStreak 返回指定 channel 是否处于自闭模式。
func (d *RejectionDetector) IsInStreak(channelKey string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	state, ok := d.perChannelState[channelKey]
	if !ok {
		return false
	}

	// 检查自闭是否已过期
	if state.streakActive && time.Since(state.streakStartedAt) > d.config.StreakDuration {
		state.streakActive = false
		state.consecutiveRejections = 0
		d.activeStreakCount.Add(-1)
		return false
	}

	return state.streakActive
}

// RejectionStreakCount 返回指定 channel 当前的连续被无视次数。
func (d *RejectionDetector) RejectionStreakCount(channelKey string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, ok := d.perChannelState[channelKey]
	if !ok {
		return 0
	}
	return state.consecutiveRejections
}

// ResetChannel 手动重置指定 channel 的被无视状态。
func (d *RejectionDetector) ResetChannel(channelKey string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.perChannelState, channelKey)
	delete(d.botRepliedAt, channelKey)
}

// ============================================================================
// AdaptiveEngagement 集成 — 返回动态调整值
// ============================================================================

// GetRejectionAdjustment 返回因被无视导致的参数临时调整。
// 当处于自闭模式时，返回降低后的参数偏移量。
func (d *RejectionDetector) GetRejectionAdjustment(channelKey string) *channelEngagementOverride {
	d.mu.Lock()
	defer d.mu.Unlock()

	state, ok := d.perChannelState[channelKey]
	if !ok || !state.streakActive {
		return nil
	}

	// 检查是否已过期
	if time.Since(state.streakStartedAt) > d.config.StreakDuration {
		state.streakActive = false
		state.consecutiveRejections = 0
		d.activeStreakCount.Add(-1)
		return nil
	}

	// 自闭模式：reply_probability 设为一个极低值（TimingGate 会封顶到 0.05），
	// backoff_start_count 降为 1 使退避更快触发。
	lowProb := 0.01
	minBackoff := 1
	return &channelEngagementOverride{
		ReplyProbability:  &lowProb,
		BackoffStartCount: &minBackoff,
	}
}

// ============================================================================
// 内部方法
// ============================================================================

func (d *RejectionDetector) getOrCreateState(channelKey string) *channelRejectionState {
	state, ok := d.perChannelState[channelKey]
	if !ok {
		state = &channelRejectionState{}
		d.perChannelState[channelKey] = state
	}
	return state
}

// recordRejection 记录一次被无视。调用方必须持有 d.mu。
// 返回值：如果触发了 streak，返回 (callback, channelKey, streakCount)，调用方应在锁外调用回调。
func (d *RejectionDetector) recordRejection(channelKey string, state *channelRejectionState) (func(string, int), string, int) {
	state.consecutiveRejections++
	state.lastRejectionAt = time.Now()
	d.totalRejections.Add(1)

	d.logger.Warnw("rejection detected",
		"channel", channelKey,
		"streak", state.consecutiveRejections,
		"threshold", d.config.StreakThreshold,
		"total_rejections", d.totalRejections.Load())

	if state.consecutiveRejections >= d.config.StreakThreshold {
		d.activateStreak(channelKey, state)
		if d.onRejectionStreak != nil {
			return d.onRejectionStreak, channelKey, state.consecutiveRejections
		}
	}
	return nil, "", 0
}

func (d *RejectionDetector) activateStreak(channelKey string, state *channelRejectionState) {
	if state.streakActive {
		return
	}
	state.streakActive = true
	state.streakStartedAt = time.Now()
	d.totalStreaks.Add(1)
	d.activeStreakCount.Add(1)

	d.logger.Warnw("rejection streak activated — bot entering cautious mode",
		"channel", channelKey,
		"streak_count", state.consecutiveRejections,
		"duration_seconds", d.config.StreakDuration.Seconds(),
		"total_streaks", d.totalStreaks.Load(),
		"active_streaks", d.activeStreakCount.Load(),
		"total_rejections", d.totalRejections.Load())
}

func (d *RejectionDetector) resetRejection(channelKey string, state *channelRejectionState) {
	if state.streakActive {
		d.activeStreakCount.Add(-1)
	}
	d.logger.Infow("rejection reset — bot was referenced",
		"channel", channelKey,
		"was_streak", state.streakActive,
		"had_rejections", state.consecutiveRejections)
	state.consecutiveRejections = 0
	state.streakActive = false
	state.streakStartedAt = time.Time{}
	state.pendingReply = false
}

func (d *RejectionDetector) isBotReferenced(msg *core.Message) bool {
	if msg == nil || d.config.BotName == "" {
		return false
	}
	// 简单检查：消息文本中是否包含 Bot 名称或 @提及
	text := msg.Text
	if text == "" {
		return false
	}
	lowerText := strings.ToLower(text)
	lowerName := strings.ToLower(d.config.BotName)
	return strings.Contains(lowerText, lowerName) ||
		strings.Contains(lowerText, "@"+lowerName)
}

// ============================================================================
// 清理
// ============================================================================

// Cleanup 清理超过 24 小时无活动的 channel 状态。
func (d *RejectionDetector) Cleanup() {
	d.mu.Lock()
	defer d.mu.Unlock()

	cutoff := time.Now().Add(-24 * time.Hour)
	for key, state := range d.perChannelState {
		if state.lastRejectionAt.Before(cutoff) && !state.streakActive {
			delete(d.perChannelState, key)
			delete(d.botRepliedAt, key)
		}
	}
}
