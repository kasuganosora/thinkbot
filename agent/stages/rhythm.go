package stages

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// RhythmPolicy 已按「平台 + 会话类型」解析后的有效节奏策略。
type RhythmPolicy struct {
	// Apply 是否套用节奏。web 平台、或平台/会话类型关闭时均为 false。
	Apply bool
	// QuietWait 防抖静默秒：频道内两次入站消息间隔小于此值视为连发，合并跳过回复。
	QuietWait int
	// SpeakTendency 发言倾向 0..1；<1 时按概率决定是否发言（用于群聊降频）。
	SpeakTendency float64
	// MaxConsecutive 连续发言上限（0=不限）。
	MaxConsecutive int
	// InterruptWindow 连续发言计数的有效窗口（秒）。
	// 距上次回复超过此时长则连续计数归零。0 表示不启用连续中断。
	// 独立于 QuietWait，避免「改限流间隔意外放大沉默时长」。
	InterruptWindow int
}

// RhythmPolicyProvider 给定 platform + chatType 返回有效策略。
// 由 api 层从 config store 解析（含 web 硬禁用、单聊关闭等规则）后注入，保持本包与 api 解耦。
type RhythmPolicyProvider func(platform, chatType string) RhythmPolicy

// RhythmStage 按「平台 + 会话类型」应用聊天节奏控制：
//   - web 平台硬禁用（永不套用节奏）。
//   - 真人显式 @ 提及一律放行（不受节奏限制，见 isHumanMention）。
//   - 单聊(private)默认关闭节奏（即时回复，不受控）。
//   - 群聊/频道默认受控：入站限流(QuietWait) + 发言倾向概率 + 连续发言中断。
//
// 命中抑制时设置 core.KVSuppressReply，由 LLMStage 跳过对外发送（但仍思考/记忆）。
//
// 注意：本 Stage 只做「该不该说」的节流，不负责「能不能说」（后者是潜水/权限）。
type RhythmStage struct {
	name     string
	logger   *zap.SugaredLogger
	provider RhythmPolicyProvider
	// rnd 独立随机源，避免与其他模块共享全局 rand 状态。由 mu 保护。
	rnd *rand.Rand

	mu        sync.Mutex
	lastMsg   map[string]time.Time // channel -> 最近一次入站消息时间
	lastReply map[string]time.Time // channel -> 最近一次「允许回复」时间
	consec    map[string]int       // channel -> 当前连续发言计数
	lastSeen  map[string]time.Time // channel -> 最近一次访问时间（用于过期清理，防内存泄漏）
}

// rhythmStateTTL 空闲超过此时长的 channel 状态会被清理，避免 map 无界增长。
const rhythmStateTTL = 6 * time.Hour

// NewRhythmStage 创建节奏控制 stage。
func NewRhythmStage(name string, provider RhythmPolicyProvider, logger *zap.SugaredLogger) *RhythmStage {
	if name == "" {
		name = "chat-rhythm"
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &RhythmStage{
		name:      name,
		logger:    logger,
		provider:  provider,
		rnd:       rand.New(rand.NewSource(time.Now().UnixNano())),
		lastMsg:   map[string]time.Time{},
		lastReply: map[string]time.Time{},
		consec:    map[string]int{},
		lastSeen:  map[string]time.Time{},
	}
}

// Name 实现 core.Stage。
func (s *RhythmStage) Name() string { return s.name }

// platformForMessage 从消息解析平台类型（复用 engagement 的口径：优先 Metadata.channel_type，回退 Source）。
func platformForMessage(msg core.Message) string {
	if msg.Metadata != nil {
		if ct, ok := msg.Metadata["channel_type"].(string); ok && ct != "" {
			return ct
		}
	}
	return msg.Source
}

// normalizeChatType 归一化会话类型：supergroup 归入 group。
func normalizeChatType(ct string) string {
	switch ct {
	case core.ChatPrivate:
		return core.ChatPrivate
	case core.ChatGroup, core.ChatSupergroup:
		return core.ChatGroup
	case core.ChatChannel:
		return core.ChatChannel
	default:
		return ""
	}
}

func getBool(env *core.Envelope, key string) bool {
	v, ok := env.Get(key)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// isHumanMention 判断这条消息是否为「真人显式 @ 了 Bot」。
//
// 为什么不能只看 Message.Mentioned：
// EngagementStage 在判定「该主动参与」时会**把 Mentioned 改写为 true**
// （agent/engagement/stage.go: `env.Message.Mentioned = true` + `engagement.proactive`），
// 用来让下游按「被提及」路径处理主动搭话。若节奏只看 Mentioned 就放行，
// 则开启 engagement 后所有能发言的消息都成了「被 @」→ 节奏彻底失效（退化成空壳）。
//
// 因此判据是：Mentioned 为真 **且** 不是 engagement 升级出来的伪提及。
// 真人 @ 走 engagement 的直接放行分支（只设 engagement.evaluated=false，不设 proactive）。
func isHumanMention(env *core.Envelope) bool {
	if !env.Message.Mentioned {
		return false
	}
	return !getBool(env, "engagement.proactive")
}

// cleanupLocked 清理空闲超过 rhythmStateTTL 的 channel 状态。调用方须持有 s.mu。
// 目的：lastMsg/lastReply/consec 以 channel 为 key，若不清理会随 channel 数缓慢泄漏。
func (s *RhythmStage) cleanupLocked(now time.Time) {
	for ch, seen := range s.lastSeen {
		if now.Sub(seen) > rhythmStateTTL {
			delete(s.lastSeen, ch)
			delete(s.lastMsg, ch)
			delete(s.lastReply, ch)
			delete(s.consec, ch)
		}
	}
}

// Process 实现 core.Stage。
func (s *RhythmStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	// 潜水模式不发言，节奏无关
	if getBool(env, core.KVLurkMode) {
		return env, nil
	}

	// 真人显式 @ 了 Bot → 一律放行，节奏不得吞掉。
	// 这与 engagement 的「被 @ 直接放行」语义保持一致：用户明确叫名，
	// 若还按概率静默丢弃，表现等同 Bot 失联（这是本 Stage 最初的严重缺陷）。
	if isHumanMention(env) {
		return env, nil
	}

	msg := env.Message
	platform := platformForMessage(msg)
	chatType := normalizeChatType(msg.ChatType)

	// web 硬规则：永不套用节奏（前端也禁止配置）
	if platform == "web" {
		return env, nil
	}

	if s.provider == nil {
		return env, nil
	}
	policy := s.provider(platform, chatType)
	if !policy.Apply {
		return env, nil
	}

	ch := msg.Channel
	now := time.Now()

	s.mu.Lock()
	s.lastSeen[ch] = now
	s.cleanupLocked(now)

	// 1) 入站限流：距上一条入站消息不足 QuietWait 秒 → 视为连发，本条不回复。
	//    语义是 rate-limit（每 QuietWait 秒最多回一条），**不是**延迟合并：
	//    窗口内的后续消息只思考、不回复，不会攒起来最后统一回。
	last, hasLast := s.lastMsg[ch]
	s.lastMsg[ch] = now
	if hasLast && policy.QuietWait > 0 && now.Sub(last) < time.Duration(policy.QuietWait)*time.Second {
		s.mu.Unlock()
		s.suppress(env, "rhythm_rate_limit",
			"platform", platform, "chat_type", chatType, "quiet_wait", policy.QuietWait)
		return env, nil
	}

	// 2) 连续发言中断：短窗口内连续回复达上限后强制让出话头，避免 Bot 独占对话。
	//    窗口用独立的 InterruptWindow（由 provider 给出），不再与 QuietWait 耦合。
	conc := 1
	if lr, ok := s.lastReply[ch]; ok {
		window := time.Duration(policy.InterruptWindow) * time.Second
		if window > 0 && now.Sub(lr) < window {
			conc = s.consec[ch] + 1
		}
	}
	if policy.MaxConsecutive > 0 && conc > policy.MaxConsecutive {
		// 触发中断即重置计数并让冷却从此刻重新计时，避免「说满 N 句后长期全哑」：
		// 不重置的话窗口内每条消息都会命中中断，表现为 Bot 突然长时间沉默。
		s.consec[ch] = 0
		delete(s.lastReply, ch)
		s.mu.Unlock()
		s.suppress(env, "rhythm_interrupt",
			"platform", platform, "chat_type", chatType, "consecutive", conc, "max", policy.MaxConsecutive)
		return env, nil
	}

	// 3) 发言倾向概率（群聊降频核心）
	if policy.SpeakTendency < 1.0 && s.rnd.Float64() > policy.SpeakTendency {
		s.mu.Unlock()
		s.suppress(env, "rhythm_speak_tendency",
			"platform", platform, "chat_type", chatType, "tendency", policy.SpeakTendency)
		return env, nil
	}

	// 允许回复：更新状态
	s.lastReply[ch] = now
	s.consec[ch] = conc
	s.mu.Unlock()
	return env, nil
}

// suppress 标记本轮不对外发送，并记录可观测日志。
func (s *RhythmStage) suppress(env *core.Envelope, reason string, kv ...any) {
	env.Set(core.KVSuppressReply, true)
	env.Set(core.KVSuppressReplyReason, reason)
	if s.logger != nil {
		s.logger.Infow("rhythm: reply suppressed",
			append([]any{"reason", reason, "message_id", env.Message.ID}, kv...)...)
	}
}
