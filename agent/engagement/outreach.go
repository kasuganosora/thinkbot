package engagement

import (
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// OutreachBreaker — 一次出价：没接住就是 No
//
// 只作用于 engagement.proactive。真人 @ / 私聊 / 对 bot 发言的表态（点赞）永远视为接住。
//
// 主动回复是相邻对的第一句。对方不接（talk-past 或冷场）即视为已完成的拒绝，
// 不得对该人再发同一话轮。冷却后再敲是追问–回避，这里不做。
//
//   open ──Send 成功──► awaiting
//   awaiting ──talk-past / 冷场──► declined（立刻，不补枪）
//   awaiting / declined ──对方 @、私聊、或点赞/反应──► open
//   declined 且超过情节边界 ──► 可参与房间，但禁止 reply_target 点名此人
//
// TimingGate 的随机噪声 / 概率门不能绕过：EngagementStage 在升级前硬查。
// ============================================================================

// OutreachBreaker 按 (channel, user) 追踪未回应的主动出击。
type OutreachBreaker struct {
	mu       sync.Mutex
	channels map[string]map[string]*userOutreachState // channelKey → userID → state
	config   OutreachBreakerConfig
	logger   *zap.SugaredLogger
	nowFn    func() time.Time // 测试可注入；nil 则 time.Now
}

type userOutreachState struct {
	pending      bool
	repliedAt    time.Time
	declined     bool
	declinedAt   time.Time
	lastActiveAt time.Time
}

// OutreachBreakerConfig 一次出价熔断参数。
type OutreachBreakerConfig struct {
	// SilenceWindow 主动回复后，无人回应即视为拒绝的等待时长。
	// 默认 3 分钟。超时只把 pending 收成 declined，不会补发。
	SilenceWindow time.Duration

	// EpisodeBoundary declined 之后，允许对房间再说话、但仍禁止点名此人的等待时长。
	// 默认 24 小时。完全恢复（可再对该人出价）只在对方 @ / 私聊开口。
	EpisodeBoundary time.Duration
}

// DefaultOutreachBreakerConfig 返回一次出价熔断的默认参数。
func DefaultOutreachBreakerConfig() OutreachBreakerConfig {
	return OutreachBreakerConfig{
		SilenceWindow:   3 * time.Minute,
		EpisodeBoundary: 24 * time.Hour,
	}
}

func (c *OutreachBreakerConfig) normalize() {
	d := DefaultOutreachBreakerConfig()
	if c.SilenceWindow <= 0 {
		c.SilenceWindow = d.SilenceWindow
	}
	if c.EpisodeBoundary <= 0 {
		c.EpisodeBoundary = d.EpisodeBoundary
	}
}

// NewOutreachBreaker 创建按人熔断器。
func NewOutreachBreaker(cfg OutreachBreakerConfig, logger *zap.SugaredLogger) *OutreachBreaker {
	cfg.normalize()
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &OutreachBreaker{
		channels: make(map[string]map[string]*userOutreachState),
		config:   cfg,
		logger:   logger.With("component", "outreach_breaker"),
	}
}

func (b *OutreachBreaker) now() time.Time {
	if b.nowFn != nil {
		return b.nowFn()
	}
	return time.Now()
}

// RecordProactiveReply 记录一次对 userID 的主动出击（必须在出站 Send 成功后调用）。
// 已 declined 时不再记 pending：情节边界后的房间级发言不是对该人的第二次出价。
func (b *OutreachBreaker) RecordProactiveReply(channelKey, userID string) {
	if channelKey == "" || userID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	st := b.getOrCreateLocked(channelKey, userID)
	now := b.now()
	st.lastActiveAt = now
	if st.declined {
		return
	}
	st.pending = true
	st.repliedAt = now
}

// OnInbound 处理一条入站消息：惰性结算过期冷场，并识别 talk-past / 回应。
// 应在 EngagementStage 升级 Mentioned 之前调用，以便读到渠道原始的「是否真人 @」。
func (b *OutreachBreaker) OnInbound(msg *core.Message) {
	if msg == nil {
		return
	}
	channelKey := channelKeyForMessage(msg)
	if channelKey == "" {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	userID := msg.UserID

	if core.IsReactionAck(msg) {
		// 点赞/反应 = 接住了。Misskey 反应的 Channel 是 userID，而时间线出价记在
		// misskey:timeline，所以按 user 清掉所有 channel 上的 pending/declined。
		for _, uid := range responseUserIDs(msg) {
			b.resetUserAllChannelsLocked(uid)
		}
		b.settleExpiredLocked(channelKey)
		return
	}

	if isUserResponse(msg) && userID != "" {
		b.resetLocked(channelKey, userID)
		b.settleExpiredLocked(channelKey)
		return
	}

	b.settleExpiredLocked(channelKey)

	if userID == "" {
		return
	}
	st := b.lookupLocked(channelKey, userID)
	if st == nil || !st.pending {
		return
	}
	// talk-past：目标用户继续说话但没有回应 bot → 明确拒绝，立刻 declined
	b.declineLocked(channelKey, userID, st, "talk-past")
}

// ShouldSuppress 升级为主动参与前的硬查。true 时不得把 Mentioned 升成 true。
// awaiting（pending）：禁止对该人再出第二枪。
// declined 且仍在情节边界内：对该人完全闭嘴。
// 越过情节边界后返回 false（允许房间级参与），但仍应剥掉 reply_target。
func (b *OutreachBreaker) ShouldSuppress(channelKey, userID string) (bool, string) {
	if channelKey == "" || userID == "" {
		return false, ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	st := b.lookupLocked(channelKey, userID)
	if st == nil {
		return false, ""
	}
	if st.pending {
		return true, core.KVSuppressReasonUnanswered
	}
	if !st.declined {
		return false, ""
	}
	if b.pastEpisodeLocked(st) {
		return false, ""
	}
	return true, core.KVSuppressReasonUnanswered
}

// ShouldStripReplyTarget 为 true 时，即使升级参与也不得把回复钉到该用户的帖子上。
// declined 直到对方发起都成立（含情节边界之后的房间级参与）。
func (b *OutreachBreaker) ShouldStripReplyTarget(channelKey, userID string) bool {
	if channelKey == "" || userID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.lookupLocked(channelKey, userID)
	return st != nil && st.declined
}

// IsDeclined 返回是否处于 declined（测试 / 观测）。情节边界不影响此标志。
func (b *OutreachBreaker) IsDeclined(channelKey, userID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.lookupLocked(channelKey, userID)
	return st != nil && st.declined
}

// IsPending 返回是否仍在等待对方接住这一枪（测试 / 观测）。
func (b *OutreachBreaker) IsPending(channelKey, userID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.lookupLocked(channelKey, userID)
	return st != nil && st.pending
}

// NotifyProactiveSent 在出站成功后根据 Action.Metadata 记账。
// 仅 engagement.proactive 的 ActionReply 会记录；发送失败的调用方不应调用本函数。
func NotifyProactiveSent(b *OutreachBreaker, action core.Action) {
	if b == nil || action.Type != core.ActionReply {
		return
	}
	if action.Metadata == nil {
		return
	}
	proactive, _ := action.Metadata[core.MetaEngagementProactive].(bool)
	if !proactive {
		return
	}
	channelKey, _ := action.Metadata[core.MetaEngagementChannel].(string)
	if channelKey == "" {
		channelKey = action.Channel
	}
	if channelKey == "" || action.UserID == "" {
		return
	}
	b.RecordProactiveReply(channelKey, action.UserID)
}

func isUserResponse(msg *core.Message) bool {
	if msg.Mentioned {
		return true
	}
	return msg.ChatType == core.ChatPrivate
}

func responseUserIDs(msg *core.Message) []string {
	seen := make(map[string]struct{}, 4)
	var ids []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(msg.UserID)
	if msg.Metadata == nil {
		return ids
	}
	switch v := msg.Metadata[core.MetaReactorIDs].(type) {
	case []string:
		for _, id := range v {
			add(id)
		}
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				add(s)
			}
		}
	}
	return ids
}

func (b *OutreachBreaker) lookupLocked(channelKey, userID string) *userOutreachState {
	users, ok := b.channels[channelKey]
	if !ok {
		return nil
	}
	return users[userID]
}

func (b *OutreachBreaker) getOrCreateLocked(channelKey, userID string) *userOutreachState {
	if len(b.channels) > 1000 {
		b.gcLocked()
	}
	users, ok := b.channels[channelKey]
	if !ok {
		users = make(map[string]*userOutreachState)
		b.channels[channelKey] = users
	}
	st, ok := users[userID]
	if !ok {
		st = &userOutreachState{}
		users[userID] = st
	}
	return st
}

func (b *OutreachBreaker) resetLocked(channelKey, userID string) {
	st := b.lookupLocked(channelKey, userID)
	if st == nil {
		return
	}
	if st.declined || st.pending {
		b.logger.Infow("outreach breaker reset — user responded",
			"channel", channelKey,
			"user_id", userID,
			"was_declined", st.declined)
	}
	st.pending = false
	st.declined = false
	st.declinedAt = time.Time{}
	st.lastActiveAt = b.now()
}

func (b *OutreachBreaker) resetUserAllChannelsLocked(userID string) {
	if userID == "" {
		return
	}
	for ch := range b.channels {
		b.resetLocked(ch, userID)
	}
}

func (b *OutreachBreaker) settleExpiredLocked(channelKey string) {
	users, ok := b.channels[channelKey]
	if !ok {
		return
	}
	now := b.now()
	for uid, st := range users {
		if !st.pending {
			continue
		}
		if now.Sub(st.repliedAt) < b.config.SilenceWindow {
			continue
		}
		b.declineLocked(channelKey, uid, st, "silence")
	}
}

func (b *OutreachBreaker) declineLocked(channelKey, userID string, st *userOutreachState, reason string) {
	st.pending = false
	st.declined = true
	st.declinedAt = b.now()
	st.lastActiveAt = st.declinedAt
	b.logger.Infow("unanswered outreach — declined, no second bid",
		"channel", channelKey,
		"user_id", userID,
		"reason", reason)
}

func (b *OutreachBreaker) pastEpisodeLocked(st *userOutreachState) bool {
	if st.declinedAt.IsZero() {
		return false
	}
	return b.now().Sub(st.declinedAt) >= b.config.EpisodeBoundary
}

func (b *OutreachBreaker) gcLocked() {
	now := b.now()
	cutoff := now.Add(-7 * 24 * time.Hour)
	for ch, users := range b.channels {
		for uid, st := range users {
			if st.pending {
				continue
			}
			// declined 要保留到对方发起，才能继续剥 reply_target；过久无活动才回收
			if st.declined && now.Sub(st.declinedAt) < 90*24*time.Hour {
				continue
			}
			if st.lastActiveAt.Before(cutoff) {
				delete(users, uid)
			}
		}
		if len(users) == 0 {
			delete(b.channels, ch)
		}
	}
}
