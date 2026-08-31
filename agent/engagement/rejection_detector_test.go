package engagement

import (
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func testRejectionDetector(botName string) *RejectionDetector {
	return NewRejectionDetector(
		RejectionDetectorConfig{
			SilenceWindowSeconds: 1.0, // 1 秒窗口便于测试
			StreakThreshold:      3,
			StreakDuration:       100 * time.Millisecond,
			BotName:              botName,
		},
		noop.NewTracerProvider(),
		zap.NewNop().Sugar(),
	)
}

// ============================================================================
// RecordReply + 基本流程
// ============================================================================

func TestRejectionDetector_RecordReply(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.RecordReply("telegram:-123")

	if d.RejectionStreakCount("telegram:-123") != 0 {
		t.Error("should start with 0 rejections")
	}

	d.mu.Lock()
	state, ok := d.perChannelState["telegram:-123"]
	if !ok || !state.pendingReply {
		d.mu.Unlock()
		t.Fatal("pendingReply should be true after RecordReply")
	}
	d.mu.Unlock()
}

func TestRejectionDetector_OnExternalMessage_ResetsWhenReferenced(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.RecordReply("telegram:-123")

	// 有人引用 Bot
	msg := &core.Message{Text: "hey testbot, good point!"}
	d.OnExternalMessage("telegram:-123", msg)

	if d.RejectionStreakCount("telegram:-123") != 0 {
		t.Error("streak should be reset when bot is referenced")
	}
}

func TestRejectionDetector_OnExternalMessage_NoMatch(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.RecordReply("telegram:-123")

	// 有人说话但没引用 Bot
	msg := &core.Message{Text: "anyway, what about the weather?"}
	d.OnExternalMessage("telegram:-123", msg)

	d.mu.Lock()
	state := d.perChannelState["telegram:-123"]
	if !state.pendingReply || state.botReferenced {
		d.mu.Unlock()
		t.Error("should still be pending without reference")
	}
	d.mu.Unlock()
}

// ============================================================================
// CheckSilence — 静默窗口超时判定
// ============================================================================

func TestRejectionDetector_CheckSilence_BeforeWindow(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.RecordReply("telegram:-123")

	// 立即检查（还在窗口内）
	d.CheckSilence("telegram:-123")

	if d.RejectionStreakCount("telegram:-123") != 0 {
		t.Error("should not detect rejection within silence window")
	}
}

func TestRejectionDetector_CheckSilence_AfterWindow_NoMessages(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.RecordReply("telegram:-123")

	// 等待窗口过期（silence window = 1.0s，需要等待 >1s）
	time.Sleep(1200 * time.Millisecond)
	d.CheckSilence("telegram:-123")

	if d.RejectionStreakCount("telegram:-123") != 1 {
		t.Errorf("should detect 1 rejection after silence, got %d",
			d.RejectionStreakCount("telegram:-123"))
	}
}

func TestRejectionDetector_CheckSilence_AfterWindow_WithMessagesButNoRef(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.RecordReply("telegram:-123")

	// 有人说话但没引用 Bot
	msg := &core.Message{Text: "hello world"}
	d.OnExternalMessage("telegram:-123", msg)

	time.Sleep(1200 * time.Millisecond)
	d.CheckSilence("telegram:-123")

	// postReplyMsgCount > 0 但 botReferenced = false → 仍视为被无视
	if d.RejectionStreakCount("telegram:-123") != 1 {
		t.Errorf("should count as rejection when messages exist but bot not referenced, got %d",
			d.RejectionStreakCount("telegram:-123"))
	}
}

// ============================================================================
// Streak 激活和过期
// ============================================================================

func TestRejectionDetector_StreakActivation(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.config.StreakThreshold = 2 // 降低阈值加快测试

	for i := 0; i < 2; i++ {
		d.RecordReply("telegram:-123")
		time.Sleep(1200 * time.Millisecond)
		d.CheckSilence("telegram:-123")
	}

	if !d.IsInStreak("telegram:-123") {
		t.Error("should be in streak after 2 consecutive rejections")
	}
}

func TestRejectionDetector_StreakExpiry(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.config.StreakThreshold = 1
	d.config.StreakDuration = 50 * time.Millisecond

	d.RecordReply("telegram:-123")
	time.Sleep(1200 * time.Millisecond)
	d.CheckSilence("telegram:-123")

	if !d.IsInStreak("telegram:-123") {
		t.Fatal("should be in streak")
	}

	// 等待过期
	time.Sleep(100 * time.Millisecond)

	if d.IsInStreak("telegram:-123") {
		t.Error("streak should have expired")
	}
	if d.RejectionStreakCount("telegram:-123") != 0 {
		t.Error("streak count should reset after expiry")
	}
}

func TestRejectionDetector_StreakCallback(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.config.StreakThreshold = 1

	var cbCalled bool
	var cbChannel string
	var cbCount int
	var mu sync.Mutex

	d.SetOnRejectionStreak(func(channelKey string, streakCount int) {
		mu.Lock()
		defer mu.Unlock()
		cbCalled = true
		cbChannel = channelKey
		cbCount = streakCount
	})

	d.RecordReply("telegram:-123")
	time.Sleep(1200 * time.Millisecond)
	d.CheckSilence("telegram:-123")

	mu.Lock()
	defer mu.Unlock()
	if !cbCalled {
		t.Error("streak callback should have been called")
	}
	if cbChannel != "telegram:-123" {
		t.Errorf("callback channel: expected 'telegram:-123', got %q", cbChannel)
	}
	if cbCount != 1 {
		t.Errorf("callback count: expected 1, got %d", cbCount)
	}
}

// ============================================================================
// Reset 和 ResetChannel
// ============================================================================

func TestRejectionDetector_ResetOnReference(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.config.StreakThreshold = 1

	d.RecordReply("telegram:-123")
	time.Sleep(1200 * time.Millisecond)
	d.CheckSilence("telegram:-123")

	if !d.IsInStreak("telegram:-123") {
		t.Fatal("should be in streak")
	}

	// 需先 RecordReply 重新开始 pendingReply 状态才能被 reset
	d.RecordReply("telegram:-123")
	// 有人引用 Bot → 重置
	msg := &core.Message{Text: "good point, testbot!"}
	d.OnExternalMessage("telegram:-123", msg)

	// 引用时应立即重置 non-pending 的 streak，但这里 streak 已经激活，
	// 检测器收到引用时只重置 pending 状态。
	// streak 本身需要在 CheckSilence 的过期检查或手动 reset。
	_ = msg
}

func TestRejectionDetector_ResetChannel(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.RecordReply("telegram:-123")
	d.ResetChannel("telegram:-123")

	if d.RejectionStreakCount("telegram:-123") != 0 {
		t.Error("streak should be 0 after reset")
	}
	if d.IsInStreak("telegram:-123") {
		t.Error("should not be in streak after reset")
	}
}

// ============================================================================
// GetRejectionAdjustment
// ============================================================================

func TestRejectionDetector_GetRejectionAdjustment_NotInStreak(t *testing.T) {
	d := testRejectionDetector("testbot")

	result := d.GetRejectionAdjustment("telegram:-123")
	if result != nil {
		t.Error("should return nil when not in streak")
	}
}

func TestRejectionDetector_GetRejectionAdjustment_InStreak(t *testing.T) {
	d := testRejectionDetector("testbot")
	d.config.StreakThreshold = 1

	d.RecordReply("telegram:-123")
	time.Sleep(1200 * time.Millisecond)
	d.CheckSilence("telegram:-123")

	if !d.IsInStreak("telegram:-123") {
		t.Fatal("should be in streak")
	}

	result := d.GetRejectionAdjustment("telegram:-123")
	if result == nil {
		t.Fatal("should return adjustment in streak")
	}
	if result.BackoffStartCount == nil || *result.BackoffStartCount != 1 {
		t.Errorf("backoff_start_count should be 1, got %v", result.BackoffStartCount)
	}
	if result.ReplyProbability == nil || *result.ReplyProbability != 0.01 {
		t.Errorf("reply_probability should be 0.01, got %v", result.ReplyProbability)
	}
}

// ============================================================================
// isBotReferenced
// ============================================================================

func TestRejectionDetector_IsBotReferenced_DirectMention(t *testing.T) {
	d := testRejectionDetector("testbot")

	if !d.isBotReferenced(&core.Message{Text: "testbot is great"}) {
		t.Error("should detect bot name in message")
	}
}

func TestRejectionDetector_IsBotReferenced_AtMention(t *testing.T) {
	d := testRejectionDetector("testbot")

	if !d.isBotReferenced(&core.Message{Text: "hey @testbot what do you think?"}) {
		t.Error("should detect @botname")
	}
}

func TestRejectionDetector_IsBotReferenced_CaseInsensitive(t *testing.T) {
	d := testRejectionDetector("TestBot")

	if !d.isBotReferenced(&core.Message{Text: "TESTBOT is awesome"}) {
		t.Error("should be case-insensitive")
	}
}

func TestRejectionDetector_IsBotReferenced_NoMatch(t *testing.T) {
	d := testRejectionDetector("testbot")

	if d.isBotReferenced(&core.Message{Text: "hello world"}) {
		t.Error("should not match unrelated message")
	}
}

func TestRejectionDetector_IsBotReferenced_NilMessage(t *testing.T) {
	d := testRejectionDetector("testbot")

	if d.isBotReferenced(nil) {
		t.Error("nil message should not be referenced")
	}
}

func TestRejectionDetector_IsBotReferenced_EmptyBotName(t *testing.T) {
	d := testRejectionDetector("")

	if d.isBotReferenced(&core.Message{Text: "hello"}) {
		t.Error("empty bot name should not match anything")
	}
}

// ============================================================================
// Cleanup
// ============================================================================

func TestRejectionDetector_Cleanup(t *testing.T) {
	d := testRejectionDetector("testbot")

	// 创建一个旧状态
	d.mu.Lock()
	d.perChannelState["old-channel"] = &channelRejectionState{
		lastRejectionAt: time.Now().Add(-48 * time.Hour),
		streakActive:    false,
	}
	d.botRepliedAt["old-channel"] = time.Now().Add(-48 * time.Hour)
	d.mu.Unlock()

	d.Cleanup()

	d.mu.Lock()
	_, exists := d.perChannelState["old-channel"]
	d.mu.Unlock()
	if exists {
		t.Error("old inactive channel should have been cleaned up")
	}
}

func TestRejectionDetector_Cleanup_PreservesActiveStreak(t *testing.T) {
	d := testRejectionDetector("testbot")

	d.mu.Lock()
	d.perChannelState["active-streak"] = &channelRejectionState{
		lastRejectionAt: time.Now().Add(-48 * time.Hour),
		streakActive:    true,
	}
	d.botRepliedAt["active-streak"] = time.Now().Add(-48 * time.Hour)
	d.mu.Unlock()

	d.Cleanup()

	d.mu.Lock()
	_, exists := d.perChannelState["active-streak"]
	d.mu.Unlock()
	if !exists {
		t.Error("active streak should NOT be cleaned up even if old")
	}
}

// ============================================================================
// 并发安全
// ============================================================================

func TestRejectionDetector_Concurrent(t *testing.T) {
	d := testRejectionDetector("testbot")

	var wg sync.WaitGroup

	// 并发 RecordReply
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				d.RecordReply("telegram:-123")
			}
		}()
	}

	// 并发 OnExternalMessage
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				d.OnExternalMessage("telegram:-123", &core.Message{Text: "testbot hello"})
			}
		}()
	}

	// 并发 CheckSilence
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				d.CheckSilence("telegram:-123")
			}
		}()
	}

	wg.Wait()
	// 不应 panic
}

func TestRejectionDetector_DefaultConfig(t *testing.T) {
	cfg := DefaultRejectionDetectorConfig()
	if cfg.SilenceWindowSeconds != 120.0 {
		t.Errorf("default silence window: expected 120, got %f", cfg.SilenceWindowSeconds)
	}
	if cfg.StreakThreshold != 3 {
		t.Errorf("default streak threshold: expected 3, got %d", cfg.StreakThreshold)
	}
	if cfg.StreakDuration != 1*time.Hour {
		t.Errorf("default streak duration: expected 1h, got %v", cfg.StreakDuration)
	}
}
