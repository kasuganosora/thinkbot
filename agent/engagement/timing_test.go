package engagement

import (
	"context"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// TimingGate tests
// ============================================================================

func TestTimingGate_AllowFirstMessage(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability: 1.0, // 100% 概率，不做频率门控
	})

	msg := timelineMsg("hello world", "user1", "misskey")
	td := gate.Evaluate(context.Background(), msg)

	if td.Action != ActionContinue {
		t.Errorf("first message should pass gate, got action=%s", td.Action)
	}
}

func TestTimingGate_BurstDetection(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 10.0, // 10 秒内的消息视为突发
	})

	// 第一条消息通过
	msg1 := timelineMsg("message 1", "user1", "misskey")
	td1 := gate.Evaluate(context.Background(), msg1)
	if td1.Action != ActionContinue {
		t.Fatalf("first message should pass, got %s", td1.Action)
	}

	// 第二条消息（在突发窗口内，但 lastMsgAt 更新了）
	// 由于 policy 也通过了，应该 continue
	msg2 := timelineMsg("message 2", "user1", "misskey")
	td2 := gate.Evaluate(context.Background(), msg2)
	// 第二条在 burst window 内 → ShouldEvaluate 返回 false → no_action
	if td2.Action != ActionNoAction {
		t.Errorf("burst message should be no_action, got %s", td2.Action)
	}
	if !td2.IsBurst {
		t.Error("should be marked as burst")
	}
}

func TestTimingGate_BackoffAfterDeclines(t *testing.T) {
	// policy 总是拒绝（只允许特定关键词，但消息不包含）
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("nonexistent_keyword_xyz"),
		)),
	)
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 0, // 禁用突发检测，确保每条都评估
		BackoffStartCount:    2,
		BackoffBaseSeconds:   1.0,
		BackoffCapSeconds:    60.0,
	})

	// 前两次拒绝不退避（但第 2 次会设置退避，因为 consecutiveDecline=2 >= startCount=2）
	msg1 := timelineMsg("no keyword here a", "user1", "misskey")
	td1 := gate.Evaluate(context.Background(), msg1)
	if td1.Action != ActionNoAction {
		t.Errorf("decline 1 should be no_action, got %s", td1.Action)
	}
	if td1.IsBackoff {
		t.Error("decline 1 should not be backoff")
	}

	msg2 := timelineMsg("no keyword here b", "user1", "misskey")
	td2 := gate.Evaluate(context.Background(), msg2)
	if td2.Action != ActionNoAction {
		t.Errorf("decline 2 should be no_action, got %s", td2.Action)
	}

	// 第 2 次 decline 后，backoff 被设置（consecutiveDecline=2, startCount=2）
	// 第 3 条消息会被退避阻挡
	_, inBackoff, _ := gate.GetChannelState("ch-misskey")
	if !inBackoff {
		t.Error("should be in backoff after 2nd consecutive decline (startCount=2)")
	}

	msg3 := timelineMsg("still no keyword", "user1", "misskey")
	td3 := gate.Evaluate(context.Background(), msg3)
	if td3.Action != ActionNoAction {
		t.Errorf("decline 3 should be no_action, got %s", td3.Action)
	}
	if !td3.IsBackoff {
		t.Error("3rd message should be blocked by backoff")
	}
}

func TestTimingGate_BackoffResetOnEngage(t *testing.T) {
	// 使用一个能控制的 policy
	allow := true
	policy := &controllablePolicy{allow: &allow}
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 0, // 禁用突发检测
		BackoffStartCount:    5, // 高阈值，测试中不触发退避
		BackoffBaseSeconds:   1.0,
		EngagedResetDecline:  true,
	})

	// 让 policy 拒绝几次
	allow = false
	for i := 0; i < 3; i++ {
		msg := timelineMsg("msg "+string(rune('a'+i)), "user1", "misskey")
		gate.Evaluate(context.Background(), msg)
	}

	decline, _, _ := gate.GetChannelState("ch-misskey")
	if decline < 3 {
		t.Errorf("expected decline >= 3, got %d", decline)
	}

	// 让 policy 通过
	allow = true
	msg := timelineMsg("now allowed", "user1", "misskey")
	td := gate.Evaluate(context.Background(), msg)
	if td.Action != ActionContinue {
		t.Errorf("should continue when allowed, got %s", td.Action)
	}

	// decline 应该被重置
	decline, inBackoff, _ := gate.GetChannelState("ch-misskey")
	if decline != 0 {
		t.Errorf("decline should be reset after engage, got %d", decline)
	}
	if inBackoff {
		t.Error("should not be in backoff after engage")
	}
}

func TestTimingGate_ProbabilityGating(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability: 0.1, // 约 10 条消息才评估一次
		BurstIntervalSeconds: 0.1,
	})

	// 前 9 条应该被概率门控跳过
	skipped := 0
	for i := 0; i < 9; i++ {
		msg := timelineMsg("msg "+string(rune('a'+i)), "user1", "misskey")
		td := gate.Evaluate(context.Background(), msg)
		if td.IsProbabilitySkip {
			skipped++
		}
	}

	if skipped == 0 {
		t.Error("at least some messages should be skipped by probability gating")
	}
}

func TestTimingGate_WaitState(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 0.1,
		WaitTimeoutSeconds:   0.1, // 很短的超时
	})

	// 手动设置 wait 状态
	gate.mu.Lock()
	state := gate.getOrCreateState("ch-misskey")
	state.waitUntil = time.Now().Add(1 * time.Second)
	gate.mu.Unlock()

	msg := timelineMsg("during wait", "user1", "misskey")
	td := gate.Evaluate(context.Background(), msg)

	if td.Action != ActionWait {
		t.Errorf("should be in wait state, got %s", td.Action)
	}
}

func TestTimingGate_ResetChannel(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, DefaultTimingGateConfig())

	// 产生一些状态
	msg := timelineMsg("hello", "user1", "misskey")
	gate.Evaluate(context.Background(), msg)

	// Reset
	gate.ResetChannel("ch-misskey")

	decline, _, _ := gate.GetChannelState("ch-misskey")
	if decline != 0 {
		t.Errorf("decline should be 0 after reset, got %d", decline)
	}
}

// controllablePolicy 用于测试的可控 policy
type controllablePolicy struct {
	allow *bool
}

func (p *controllablePolicy) Evaluate(_ context.Context, _ *core.Message) Decision {
	if *p.allow {
		return Decision{Engage: true, Action: ActionContinue, Reason: "allowed", Tier: TierPass}
	}
	return Decision{Engage: false, Action: ActionNoAction, Reason: "denied", Tier: TierRule}
}

// ============================================================================
// ProbabilityRule tests
// ============================================================================

func TestProbabilityRule_AlwaysAllow(t *testing.T) {
	rule := NewProbabilityRule(1.0)
	msg := timelineMsg("hello", "user1", "misskey")
	allow, _ := rule.Allow(msg)
	if !allow {
		t.Error("probability=1.0 should always allow")
	}
}

func TestProbabilityRule_AlwaysDeny(t *testing.T) {
	rule := NewProbabilityRule(0.0)
	msg := timelineMsg("hello", "user1", "misskey")
	allow, _ := rule.Allow(msg)
	if allow {
		t.Error("probability=0.0 should always deny")
	}
}

func TestProbabilityRule_Deterministic(t *testing.T) {
	rule := NewProbabilityRule(0.5)
	msg := timelineMsg("hello", "user1", "misskey")

	// 同一条消息应该得到一致结果
	result1, _ := rule.Allow(msg)
	result2, _ := rule.Allow(msg)
	if result1 != result2 {
		t.Error("same message should get deterministic result")
	}
}

func TestProbabilityRule_Distribution(t *testing.T) {
	rule := NewProbabilityRule(0.3)
	allowed := 0
	total := 1000

	for i := 0; i < total; i++ {
		msg := &core.Message{
			ID:        "msg-" + string(rune(i%256)),
			Text:      "test message",
			Channel:   "ch-misskey",
			Source:    "misskey",
			UserID:    "user1",
			CreatedAt: time.Now(),
		}
		a, _ := rule.Allow(msg)
		if a {
			allowed++
		}
	}

	ratio := float64(allowed) / float64(total)
	// 允许较大偏差（统计波动）
	if ratio < 0.2 || ratio > 0.4 {
		t.Errorf("probability=0.3 should yield ~30%% allow, got %.1f%%", ratio*100)
	}
}

// ============================================================================
// BurstBuffer tests
// ============================================================================

func TestBurstBuffer_FirstMessage(t *testing.T) {
	buf := NewBurstBuffer(5 * time.Second)
	msg := timelineMsg("hello", "user1", "misskey")

	matured := buf.Push(msg)
	if matured != nil {
		t.Error("first push should return nil (no matured message)")
	}
}

func TestBurstBuffer_BurstReplacement(t *testing.T) {
	buf := NewBurstBuffer(5 * time.Second)

	now := time.Now()
	msg1 := timelineMsgWithTime("first", "user1", "misskey", now)
	msg2 := timelineMsgWithTime("second", "user1", "misskey", now.Add(1*time.Second))

	buf.Push(msg1)
	matured := buf.Push(msg2)

	// 在突发窗口内，旧消息被替换，没有成熟的
	if matured != nil {
		t.Error("burst message should not mature old message")
	}

	// Flush 应该返回最新的
	flushed := buf.Flush("ch-misskey")
	if flushed == nil || flushed.Text != "second" {
		t.Error("flush should return the latest burst message")
	}
}

func TestBurstBuffer_MatureAfterWindow(t *testing.T) {
	buf := NewBurstBuffer(5 * time.Second)

	now := time.Now()
	msg1 := timelineMsgWithTime("first", "user1", "misskey", now)
	msg2 := timelineMsgWithTime("second", "user1", "misskey", now.Add(10*time.Second))

	buf.Push(msg1)
	matured := buf.Push(msg2)

	if matured == nil {
		t.Fatal("old message should mature after window")
	}
	if matured.Text != "first" {
		t.Errorf("matured message should be 'first', got %q", matured.Text)
	}
}

func TestBurstBuffer_FlushAll(t *testing.T) {
	buf := NewBurstBuffer(5 * time.Second)

	buf.Push(timelineMsg("msg1", "user1", "misskey"))
	buf.Push(timelineMsg("msg2", "user2", "telegram"))

	all := buf.FlushAll()
	if len(all) != 2 {
		t.Errorf("flush all should return 2 messages, got %d", len(all))
	}
}

func TestBurstBuffer_FlushEmpty(t *testing.T) {
	buf := NewBurstBuffer(5 * time.Second)
	result := buf.Flush("nonexistent")
	if result != nil {
		t.Error("flush of empty channel should return nil")
	}
}

// ============================================================================
// Action type tests
// ============================================================================

func TestAction_String(t *testing.T) {
	tests := []struct {
		action Action
		want   string
	}{
		{ActionContinue, "continue"},
		{ActionNoAction, "no_action"},
		{ActionWait, "wait"},
	}

	for _, tt := range tests {
		if string(tt.action) != tt.want {
			t.Errorf("Action(%q).String() = %q, want %q", tt.action, string(tt.action), tt.want)
		}
	}
}

func TestDecision_ActionDerivation(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))

	// misskey 源通过 → ActionContinue
	d2 := policy.Evaluate(context.Background(), timelineMsg("hello", "user1", "misskey"))
	if d2.Action != ActionContinue {
		t.Errorf("engage=true should derive ActionContinue, got %s", d2.Action)
	}

	// rss 源被拒 → ActionNoAction
	d3 := policy.Evaluate(context.Background(), timelineMsg("hello", "user1", "rss"))
	if d3.Action != ActionNoAction {
		t.Errorf("engage=false should derive ActionNoAction, got %s", d3.Action)
	}
}

// ============================================================================
// Integration: TimingGate + EngagementStage
// ============================================================================

func TestEngagementStage_WithTimingGate(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("golang"),
		)),
	)
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 0.1,
		BackoffStartCount:    10, // 高阈值，测试中不触发退避
	})

	stage := newTestStage(policy).WithTimingGate(gate)

	// 第一条匹配关键词的消息应该通过
	msg := *timelineMsg("Let's discuss golang", "user1", "misskey")
	env := newEnvelope(msg)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Message.Mentioned {
		t.Error("should engage on first matching message")
	}
}

func TestEngagementStage_TimingGateBurstDebounce(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 60.0, // 很长，确保第二条被 debounce
	})

	stage := newTestStage(policy).WithTimingGate(gate)

	// 第一条通过
	msg1 := *timelineMsg("message 1", "user1", "misskey")
	env1 := newEnvelope(msg1)
	result1, _ := stage.Process(context.Background(), env1)
	if !result1.Message.Mentioned {
		t.Fatal("first message should engage")
	}

	// 第二条被 burst debounce
	msg2 := *timelineMsg("message 2", "user1", "misskey")
	env2 := newEnvelope(msg2)
	result2, _ := stage.Process(context.Background(), env2)
	if result2.Message.Mentioned {
		t.Error("second message in burst window should be debounced")
	}

	// 应该标记 is_burst
	if v, ok := env2.Get("engagement.is_burst"); ok {
		if isBurst, _ := v.(bool); !isBurst {
			t.Error("should be marked as is_burst")
		}
	}
}

// ============================================================================
// helpers
// ============================================================================

func timelineMsgWithTime(text, userID, source string, ts time.Time) *core.Message {
	msg := timelineMsg(text, userID, source)
	msg.CreatedAt = ts
	return msg
}

// =====================================================================
// Backoff bypass tests (Fix 1: 退避绕过机制)
// =====================================================================

func TestTimingGate_BackoffBypass(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("nonexistent_keyword_xyz"),
		)),
	)
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:          1.0,
		BurstIntervalSeconds:      0, // 禁用突发检测
		BackoffStartCount:         2,
		BackoffBaseSeconds:        600, // 10 分钟退避
		BackoffCapSeconds:         600,
		BackoffBypassPendingCount: 3, // 3 条待处理消息绕过退避
	})

	// 触发退避
	msg := timelineMsg("no keyword a", "user1", "misskey")
	gate.Evaluate(context.Background(), msg)
	msg = timelineMsg("no keyword b", "user1", "misskey")
	gate.Evaluate(context.Background(), msg)

	_, inBackoff, _ := gate.GetChannelState("ch-misskey")
	if !inBackoff {
		t.Fatal("should be in backoff")
	}

	// 发送 3 条消息，第 3 条应绕过退避
	for i := 0; i < 3; i++ {
		msg = timelineMsg("no keyword c"+string(rune('0'+i)), "user1", "misskey")
		td := gate.Evaluate(context.Background(), msg)
		if i == 2 {
			// 第 3 条绕过了退避
			if td.IsBackoff {
				t.Error("3rd pending message should bypass backoff")
			}
		}
	}

	// 退避应被清除
	_, inBackoff, _ = gate.GetChannelState("ch-misskey")
	if inBackoff {
		t.Error("backoff should be cleared after bypass")
	}
}

func TestTimingGate_BackoffBypassDisabled(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("nonexistent_keyword_xyz"),
		)),
	)
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:          1.0,
		BurstIntervalSeconds:      0,
		BackoffStartCount:         2,
		BackoffBaseSeconds:        600,
		BackoffCapSeconds:         600,
		BackoffBypassPendingCount: 0, // 禁用绕过
	})

	// 触发退避
	msg := timelineMsg("no keyword a", "user1", "misskey")
	gate.Evaluate(context.Background(), msg)
	msg = timelineMsg("no keyword b", "user1", "misskey")
	gate.Evaluate(context.Background(), msg)

	// 发送 10 条消息，都不应绕过
	for i := 0; i < 10; i++ {
		msg = timelineMsg("no keyword c"+string(rune('0'+i%10)), "user1", "misskey")
		td := gate.Evaluate(context.Background(), msg)
		if !td.IsBackoff {
			t.Errorf("message %d should be blocked by backoff", i)
		}
	}
}

// =====================================================================
// Private chat backoff exemption (Fix 4: 群聊/私聊区分)
// =====================================================================

func TestTimingGate_PrivateChatNoBackoff(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("telegram"),
		WithRules(NewRuleEngine(
			NewKeywordRule("nonexistent_keyword_xyz"),
		)),
	)
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 0,
		BackoffStartCount:    2,
		BackoffBaseSeconds:   600,
		BackoffCapSeconds:    600,
	})

	// 私聊消息连续被拒绝，不应触发退避
	for i := 0; i < 5; i++ {
		msg := timelineMsg("no keyword "+string(rune('a'+i)), "user1", "telegram")
		msg.ChatType = "private"
		td := gate.Evaluate(context.Background(), msg)
		if td.IsBackoff {
			t.Errorf("private chat message %d should not be in backoff", i)
		}
	}

	decline, inBackoff, _ := gate.GetChannelState("ch-telegram")
	if inBackoff {
		t.Error("private chat should not enter backoff")
	}
	_ = decline
}

func TestTimingGate_GroupChatBackoff(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("telegram"),
		WithRules(NewRuleEngine(
			NewKeywordRule("nonexistent_keyword_xyz"),
		)),
	)
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 0,
		BackoffStartCount:    2,
		BackoffBaseSeconds:   600,
		BackoffCapSeconds:    600,
	})

	// 群聊消息连续被拒绝，应触发退避
	for i := 0; i < 3; i++ {
		msg := timelineMsg("no keyword "+string(rune('a'+i)), "user1", "telegram")
		msg.ChatType = "group"
		gate.Evaluate(context.Background(), msg)
	}

	_, inBackoff, _ := gate.GetChannelState("ch-telegram")
	if !inBackoff {
		t.Error("group chat should enter backoff after repeated declines")
	}
}

// =====================================================================
// Frequency adjustment (Fix 5: 运行时频率调整)
// =====================================================================

func TestTimingGate_AdjustFrequency(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     0.1, // 低概率
		BurstIntervalSeconds: 0.1,
	})

	// 正常概率下，前几条消息很多被跳过
	skippedBefore := 0
	for i := 0; i < 10; i++ {
		msg := timelineMsg("msg "+string(rune('A'+i%26)), "user1", "misskey")
		td := gate.Evaluate(context.Background(), msg)
		if td.IsProbabilitySkip {
			skippedBefore++
		}
	}
	if skippedBefore == 0 {
		t.Error("expected some skips with low probability")
	}

	// 重置频道状态
	gate.ResetChannel("ch-misskey")

	// 提高频率倍率到 10 倍 → 等效概率 1.0
	gate.AdjustFrequency(10.0)

	// 现在所有消息都应通过概率门控
	skippedAfter := 0
	for i := 0; i < 10; i++ {
		msg := timelineMsg("msg "+string(rune('A'+i%26)), "user1", "misskey")
		td := gate.Evaluate(context.Background(), msg)
		if td.IsProbabilitySkip {
			skippedAfter++
		}
	}
	if skippedAfter > 0 {
		t.Errorf("with 10x multiplier, should not skip any, got %d skips", skippedAfter)
	}
}

// =====================================================================
// Idle compensation fallback (Fix 7: 空闲补偿回退策略)
// =====================================================================

func TestTimingGate_IdleCompensationFallbackNoSamples(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:            0.1, // 阈值 ~10
		BurstIntervalSeconds:        0.1,
		IdleCompensationMinInterval: 1.0, // 1 秒，方便测试
		IdleCompensationWindow:      30 * time.Minute,
	})

	// 发送几条消息（但间隔很小，会被 burst 过滤，不会产生 interval 样本）
	for i := 0; i < 5; i++ {
		msg := timelineMsg("burst msg "+string(rune('A'+i)), "user1", "misskey")
		gate.Evaluate(context.Background(), msg)
	}

	// 等待空闲时间足够长（超过 minInterval * threshold）
	// 手动设置 lastExternalMsgAt 为很久以前
	gate.mu.Lock()
	state := gate.getOrCreateState("ch-misskey")
	state.lastExternalMsgAt = time.Now().Add(-30 * time.Second) // 30 秒前
	gate.mu.Unlock()

	// 现在发一条消息——空闲补偿应该触发（回退到 minInterval）
	msg := timelineMsg("after idle", "user1", "misskey")
	td := gate.Evaluate(context.Background(), msg)

	// 应该通过（不是因为概率，而是因为空闲补偿）
	if td.IsProbabilitySkip {
		t.Error("idle compensation should trigger with fallback when no interval samples")
	}
}

// =====================================================================
// Wait timer tests (Fix 2: Wait 超时再评估)
// =====================================================================

func TestTimingGate_WaitExpiredCallback(t *testing.T) {
	policy := &controllablePolicy{allow: new(bool)}

	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 0.1,
		WaitTimeoutSeconds:   0.1, // 100ms 超时
	})

	callbackCalled := make(chan string, 1)
	gate.SetWaitExpiredCallback(func(channelKey string) {
		callbackCalled <- channelKey
	})

	// 设置一个 wait 状态（通过 metadata）
	allow := true // 先让 policy 通过一次建立状态
	*policy.allow = allow

	msg1 := timelineMsg("first", "user1", "misskey")
	gate.Evaluate(context.Background(), msg1)

	// 现在手动设置 wait
	gate.mu.Lock()
	state := gate.getOrCreateState("ch-misskey")
	state.waitUntil = time.Now().Add(100 * time.Millisecond)
	gate.startWaitTimer("ch-misskey", state.waitUntil)
	gate.mu.Unlock()

	// 等待回调
	select {
	case key := <-callbackCalled:
		if key != "ch-misskey" {
			t.Errorf("callback channel key = %q, want ch-misskey", key)
		}
	case <-time.After(2 * time.Second):
		t.Error("wait expired callback should be called")
	}

	gate.Close()
}

// =====================================================================
// BurstBuffer timer tests (Fix 3/6: BurstBuffer 集成)
// =====================================================================

func TestBurstBuffer_OnMatureCallback(t *testing.T) {
	buf := NewBurstBuffer(100 * time.Millisecond)

	matured := make(chan *core.Message, 1)
	buf.SetOnMature(func(_ string, msg *core.Message) {
		matured <- msg
	})

	now := time.Now()
	msg := timelineMsgWithTime("burst msg", "user1", "misskey", now)
	buf.Push(msg)

	// 等待 mature 回调
	select {
	case m := <-matured:
		if m.Text != "burst msg" {
			t.Errorf("matured message = %q, want 'burst msg'", m.Text)
		}
	case <-time.After(2 * time.Second):
		t.Error("onMature should be called after window")
	}

	buf.Close()
}

func TestBurstBuffer_OnMatureAfterBurstReplacement(t *testing.T) {
	buf := NewBurstBuffer(100 * time.Millisecond)

	matured := make(chan string, 1)
	buf.SetOnMature(func(_ string, msg *core.Message) {
		matured <- msg.Text
	})

	now := time.Now()
	msg1 := timelineMsgWithTime("first", "user1", "misskey", now)
	buf.Push(msg1)

	// 30ms 后推入第二条（突发替换）
	msg2 := timelineMsgWithTime("second", "user1", "misskey", now.Add(30*time.Millisecond))
	buf.Push(msg2)

	// 应该收到 "second"（最后一条）
	select {
	case text := <-matured:
		if text != "second" {
			t.Errorf("matured message = %q, want 'second'", text)
		}
	case <-time.After(2 * time.Second):
		t.Error("onMature should be called after burst settles")
	}

	buf.Close()
}

func TestEngagementStage_WithBurstBuffer(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))

	// 用于接收重新投递的消息
	reenqueued := make(chan *core.Envelope, 1)
	reenqueue := func(env *core.Envelope) {
		reenqueued <- env
	}

	stage := newTestStage(policy)
	buf := NewBurstBuffer(100 * time.Millisecond)
	stage.WithBurstBuffer(buf, reenqueue)

	// 第一条消息应被缓存
	msg := *timelineMsg("burst message", "user1", "misskey")
	env := newEnvelope(msg)
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Message.Mentioned {
		t.Error("burst message should not be evaluated immediately")
	}

	// 等待成熟后重新投递
	select {
	case reenv := <-reenqueued:
		if reenv.Message.Text != "burst message" {
			t.Errorf("reenqueued message = %q, want 'burst message'", reenv.Message.Text)
		}
	case <-time.After(2 * time.Second):
		t.Error("burst message should be re-enqueued after maturing")
	}

	buf.Close()
}
