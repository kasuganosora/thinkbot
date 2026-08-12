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
}

// RhythmPolicyProvider 给定 platform + chatType 返回有效策略。
// 由 api 层从 config store 解析（含 web 硬禁用、单聊关闭等规则）后注入，保持本包与 api 解耦。
type RhythmPolicyProvider func(platform, chatType string) RhythmPolicy

// RhythmStage 按「平台 + 会话类型」应用聊天节奏控制：
//   - web 平台硬禁用（永不套用节奏）。
//   - 单聊(private)默认关闭节奏（即时回复，不受控）。
//   - 群聊/频道默认受控：防抖合并连发 + 发言倾向概率 + 连续发言中断。
//
// 命中抑制时设置 core.KVSuppressReply，由 LLMStage 跳过对外发送（但仍思考/记忆）。
type RhythmStage struct {
	name     string
	logger   *zap.SugaredLogger
	provider RhythmPolicyProvider

	mu        sync.Mutex
	lastMsg   map[string]time.Time // channel -> 最近一次入站消息时间
	lastReply map[string]time.Time // channel -> 最近一次「允许回复」时间
	consec    map[string]int       // channel -> 当前连续发言计数
}

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
		lastMsg:   map[string]time.Time{},
		lastReply: map[string]time.Time{},
		consec:    map[string]int{},
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

// Process 实现 core.Stage。
func (s *RhythmStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	// 潜水模式不发言，节奏无关
	if getBool(env, core.KVLurkMode) {
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

	// 1) 防抖：频道内最近一次消息在 QuietWait 内 → 视为连发，合并跳过
	last, hasLast := s.lastMsg[ch]
	s.lastMsg[ch] = now
	if hasLast && policy.QuietWait > 0 && now.Sub(last) < time.Duration(policy.QuietWait)*time.Second {
		s.mu.Unlock()
		s.suppress(env, "rhythm_debounce",
			"platform", platform, "chat_type", chatType, "quiet_wait", policy.QuietWait)
		return env, nil
	}

	// 2) 连续发言中断：基于「最近一次允许回复」是否在短窗口内累计
	conc := 1
	if lr, ok := s.lastReply[ch]; ok {
		window := time.Duration(policy.QuietWait*2+10) * time.Second
		if now.Sub(lr) < window {
			conc = s.consec[ch] + 1
		}
	}
	if policy.MaxConsecutive > 0 && conc > policy.MaxConsecutive {
		s.mu.Unlock()
		s.suppress(env, "rhythm_interrupt",
			"platform", platform, "chat_type", chatType, "consecutive", conc, "max", policy.MaxConsecutive)
		return env, nil
	}

	// 3) 发言倾向概率（群聊降频核心）
	if policy.SpeakTendency < 1.0 && rand.Float64() > policy.SpeakTendency {
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
