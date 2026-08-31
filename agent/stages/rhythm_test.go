package stages

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// newRhythmEnv 构造一条测试消息的 Envelope。
func newRhythmEnv(platform, chatType, channel string, mentioned bool) *core.Envelope {
	msg := core.Message{
		ID:        "m1",
		Channel:   channel,
		ChatType:  chatType,
		Source:    platform,
		Mentioned: mentioned,
		Metadata:  map[string]any{"channel_type": platform},
	}
	return core.NewEnvelope(msg)
}

func isSuppressed(env *core.Envelope) bool {
	return getBool(env, core.KVSuppressReply)
}

// alwaysSuppressProvider 返回必定抑制的策略（tendency=0），
// 用于验证「是否被节奏拦下」这一条件本身。
func alwaysSuppressProvider(platform, chatType string) RhythmPolicy {
	return RhythmPolicy{Apply: true, SpeakTendency: 0}
}

// TestRhythm_HumanMentionAlwaysPasses 真人显式 @ 必须放行，
// 即使策略为「必定抑制」（tendency=0）。这是本 Stage 最初的严重缺陷：
// 群聊 @ Bot 会被概率静默吞掉。
func TestRhythm_HumanMentionAlwaysPasses(t *testing.T) {
	s := NewRhythmStage("chat-rhythm", alwaysSuppressProvider, nil)
	env := newRhythmEnv("telegram", core.ChatGroup, "c1", true)

	if _, err := s.Process(context.Background(), env); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if isSuppressed(env) {
		t.Fatal("真人 @ 消息被节奏抑制了，必须放行")
	}
}

// TestRhythm_EngagementProactiveIsNotHumanMention engagement 升级出的伪提及
// （Mentioned=true + engagement.proactive=true）**不得**当作真人 @ 放行，
// 否则开启 engagement 后节奏会彻底失效（退化成空壳）。
func TestRhythm_EngagementProactiveIsNotHumanMention(t *testing.T) {
	s := NewRhythmStage("chat-rhythm", alwaysSuppressProvider, nil)
	env := newRhythmEnv("telegram", core.ChatGroup, "c1", true)
	env.Set("engagement.proactive", true) // engagement 判定主动参与时会置 Mentioned=true

	if _, err := s.Process(context.Background(), env); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !isSuppressed(env) {
		t.Fatal("engagement 主动参与的伪提及应受节奏控制，不能按真人 @ 放行")
	}
}

// TestRhythm_WebAlwaysBypasses web 平台硬禁用节奏。
func TestRhythm_WebAlwaysBypasses(t *testing.T) {
	s := NewRhythmStage("chat-rhythm", alwaysSuppressProvider, nil)
	env := newRhythmEnv("web", core.ChatPrivate, "w1", false)

	if _, err := s.Process(context.Background(), env); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if isSuppressed(env) {
		t.Fatal("web 平台不得套用节奏")
	}
}

// TestRhythm_LurkModeBypasses 潜水模式不参与节奏（潜水本就不发言）。
func TestRhythm_LurkModeBypasses(t *testing.T) {
	s := NewRhythmStage("chat-rhythm", alwaysSuppressProvider, nil)
	env := newRhythmEnv("misskey", core.ChatGroup, "c1", false)
	env.Set(core.KVLurkMode, true)

	if _, err := s.Process(context.Background(), env); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if isSuppressed(env) {
		t.Fatal("潜水模式不应设置回复抑制（语义应由潜水分支处理）")
	}
}

// TestRhythm_PreservesHardPassiveGate 复现 2026-08-25 日志审计发现的回归：
// ingress 的 passive-speak enricher 已对「未被真人 @」的消息设硬权限门
// （KVSuppressReply=true + reason=passive_mode_unmentioned）。节奏 stage 若在 pipeline
// 内再调用 suppress()，会把 reason 改写成 rhythm_speak_tendency，致下游 reply-control 误判为
// 软节流、被模型 send:true 覆盖放行，被动 bot 于是对未 @ 消息发帖。
// 节奏必须原样保留硬门、不得改写其 reason。
func TestRhythm_PreservesHardPassiveGate(t *testing.T) {
	s := NewRhythmStage("chat-rhythm", alwaysSuppressProvider, nil)
	env := newRhythmEnv("misskey", core.ChatGroup, "c1", false)
	// 模拟 passive-speak enricher 在 ingress 阶段已设的硬门：
	env.Set(core.KVSuppressReply, true)
	env.Set(core.KVSuppressReplyReason, core.KVSuppressReasonPassive)
	// engagement 随后把 Mentioned 升成 true（伪提及），节奏本应按软门处理——
	// 但硬门优先，节奏不得触碰。
	env.Message.Mentioned = true
	env.Set("engagement.proactive", true)

	if _, err := s.Process(context.Background(), env); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !isSuppressed(env) {
		t.Fatal("硬 passive 门必须保持抑制")
	}
	if r, _ := env.Get(core.KVSuppressReplyReason); r != core.KVSuppressReasonPassive {
		t.Fatalf("节奏改写了硬门 reason：got %v, want %v", r, core.KVSuppressReasonPassive)
	}
}

// TestRhythm_PolicyNotApplyPasses 策略关闭（如单聊）时即时回复。
func TestRhythm_PolicyNotApplyPasses(t *testing.T) {
	s := NewRhythmStage("chat-rhythm", func(p, ct string) RhythmPolicy {
		return RhythmPolicy{Apply: false}
	}, nil)
	env := newRhythmEnv("telegram", core.ChatPrivate, "c1", false)

	if _, err := s.Process(context.Background(), env); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if isSuppressed(env) {
		t.Fatal("节奏关闭时不应抑制回复")
	}
}

// TestRhythm_RateLimitSuppressesBurst 连发第二条被限流抑制（rate-limit 语义）。
func TestRhythm_RateLimitSuppressesBurst(t *testing.T) {
	s := NewRhythmStage("chat-rhythm", func(p, ct string) RhythmPolicy {
		return RhythmPolicy{Apply: true, QuietWait: 3, SpeakTendency: 1.0}
	}, nil)

	first := newRhythmEnv("telegram", core.ChatGroup, "c1", false)
	if _, err := s.Process(context.Background(), first); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if isSuppressed(first) {
		t.Fatal("首条消息不应被限流")
	}

	second := newRhythmEnv("telegram", core.ChatGroup, "c1", false)
	if _, err := s.Process(context.Background(), second); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !isSuppressed(second) {
		t.Fatal("QuietWait 窗口内的连发应被限流抑制")
	}
	if v, _ := second.Get(core.KVSuppressReplyReason); v != "rhythm_rate_limit" {
		t.Fatalf("抑制原因应为 rhythm_rate_limit，实际 %v", v)
	}
}

// TestRhythm_InterruptResetsAfterTrigger 触发连续中断后计数与冷却应重置，
// 避免「说满 N 句后长时间全哑」。
func TestRhythm_InterruptResetsAfterTrigger(t *testing.T) {
	s := NewRhythmStage("chat-rhythm", func(p, ct string) RhythmPolicy {
		// QuietWait=0 关掉限流，单独验证中断逻辑
		return RhythmPolicy{Apply: true, QuietWait: 0, SpeakTendency: 1.0,
			MaxConsecutive: 2, InterruptWindow: 60}
	}, nil)
	ctx := context.Background()

	// 前两条允许（conc=1,2）
	for i := 0; i < 2; i++ {
		env := newRhythmEnv("telegram", core.ChatGroup, "c1", false)
		if _, err := s.Process(ctx, env); err != nil {
			t.Fatalf("Process error: %v", err)
		}
		if isSuppressed(env) {
			t.Fatalf("第 %d 条不应被抑制", i+1)
		}
	}

	// 第三条超过上限 → 中断
	third := newRhythmEnv("telegram", core.ChatGroup, "c1", false)
	if _, err := s.Process(ctx, third); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if !isSuppressed(third) {
		t.Fatal("超过连续上限应触发中断抑制")
	}

	// 中断已重置状态 → 下一条重新允许（不是继续全哑）
	fourth := newRhythmEnv("telegram", core.ChatGroup, "c1", false)
	if _, err := s.Process(ctx, fourth); err != nil {
		t.Fatalf("Process error: %v", err)
	}
	if isSuppressed(fourth) {
		t.Fatal("中断后应重置计数，下一条须重新允许发言，而非持续沉默")
	}
}

// TestRhythm_InterruptDisabledWhenMaxZero MaxConsecutive=0 表示不限（UI 关闭中断）。
func TestRhythm_InterruptDisabledWhenMaxZero(t *testing.T) {
	s := NewRhythmStage("chat-rhythm", func(p, ct string) RhythmPolicy {
		return RhythmPolicy{Apply: true, QuietWait: 0, SpeakTendency: 1.0,
			MaxConsecutive: 0, InterruptWindow: 60}
	}, nil)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		env := newRhythmEnv("telegram", core.ChatGroup, "c1", false)
		if _, err := s.Process(ctx, env); err != nil {
			t.Fatalf("Process error: %v", err)
		}
		if isSuppressed(env) {
			t.Fatalf("关闭连续中断后第 %d 条不应被抑制", i+1)
		}
	}
}
