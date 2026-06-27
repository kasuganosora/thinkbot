package engagement

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

func syncerTestLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

func syncerTestTracerProvider() trace.TracerProvider {
	return noop.NewTracerProvider()
}

func newTestSyncer(globalEnabled bool, channels []string) *AdaptiveEngagementSyncer {
	return NewAdaptiveEngagementSyncer(
		SyncerConfig{
			BotID:           "test-bot",
			InitialTraits:   DefaultBotProfileTraits(),
			GlobalEnabled:   globalEnabled,
			EnabledChannels: channels,
		},
		syncerTestTracerProvider(),
		syncerTestLogger(),
	)
}

// ============================================================================
// Traits 更新
// ============================================================================

func TestAdaptiveEngagementSyncer_UpdateTraits(t *testing.T) {
	s := newTestSyncer(true, []string{"telegram"})
	old := s.GetTraits()

	newTraits := BotProfileTraits{
		EnergyLevel: 0.9,
		Patience:    0.8,
		Verbosity:   0.7,
		Personality: "enthusiastic helper",
		Confidence:  0.8,
	}
	s.UpdateTraits(newTraits)

	updated := s.GetTraits()
	if updated.EnergyLevel != 0.9 {
		t.Errorf("EnergyLevel: expected 0.9, got %f", updated.EnergyLevel)
	}
	if updated.Patience != 0.8 {
		t.Errorf("Patience: expected 0.8, got %f", updated.Patience)
	}
	if updated.Personality != "enthusiastic helper" {
		t.Errorf("Personality: expected 'enthusiastic helper', got %q", updated.Personality)
	}
	if old.EnergyLevel == updated.EnergyLevel {
		t.Error("old and new should differ")
	}
}

func TestAdaptiveEngagementSyncer_GetTraits_ThreadSafe(t *testing.T) {
	s := newTestSyncer(true, nil)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.UpdateTraits(BotProfileTraits{EnergyLevel: float64(i) / 100})
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		_ = s.GetTraits()
	}
	<-done
}

// ============================================================================
// Channel 开关管理
// ============================================================================

func TestAdaptiveEngagementSyncer_ChannelEnabled(t *testing.T) {
	s := newTestSyncer(true, []string{"telegram"})

	if !s.IsChannelEnabled("telegram", "") {
		t.Error("telegram should be enabled")
	}
	if s.IsChannelEnabled("misskey", "") {
		t.Error("misskey should NOT be enabled (not in channels)")
	}
}

func TestAdaptiveEngagementSyncer_ChannelEnabled_Hierarchy(t *testing.T) {
	s := newTestSyncer(true, []string{"telegram", "telegram:-123456"})

	// 类型级启用
	if !s.IsChannelEnabled("telegram", "") {
		t.Error("telegram type should be enabled")
	}
	// 具体群聊级启用
	if !s.IsChannelEnabled("telegram", "-123456") {
		t.Error("telegram:-123456 should be enabled")
	}
	// 未配置的具体群聊回退到类型级
	if !s.IsChannelEnabled("telegram", "-999999") {
		t.Error("telegram:-999999 should inherit from telegram type")
	}
}

func TestAdaptiveEngagementSyncer_GlobalDisabled(t *testing.T) {
	s := newTestSyncer(false, []string{"telegram"})

	if s.IsChannelEnabled("telegram", "") {
		t.Error("should be disabled when global is off")
	}
}

func TestAdaptiveEngagementSyncer_SetChannelEnabled(t *testing.T) {
	s := newTestSyncer(true, nil)

	s.SetChannelEnabled("telegram", true)
	if !s.IsChannelEnabled("telegram", "") {
		t.Error("telegram should now be enabled")
	}

	s.SetChannelEnabled("telegram", false)
	if s.IsChannelEnabled("telegram", "") {
		t.Error("telegram should now be disabled")
	}
}

func TestAdaptiveEngagementSyncer_SetGlobalEnabled(t *testing.T) {
	s := newTestSyncer(true, []string{"telegram"})

	s.SetGlobalEnabled(false)
	if s.IsChannelEnabled("telegram", "") {
		t.Error("should be disabled after global toggle")
	}

	s.SetGlobalEnabled(true)
	if !s.IsChannelEnabled("telegram", "") {
		t.Error("should be re-enabled after global toggle")
	}
}

// ============================================================================
// GetTimingConfigOverride
// ============================================================================

func TestAdaptiveEngagementSyncer_GetTimingConfigOverride_GlobalDisabled(t *testing.T) {
	s := newTestSyncer(false, []string{"telegram"})

	result := s.GetTimingConfigOverride("telegram", "")
	if result != nil {
		t.Error("should return nil when globally disabled")
	}
}

func TestAdaptiveEngagementSyncer_GetTimingConfigOverride_ChannelDisabled(t *testing.T) {
	s := newTestSyncer(true, []string{"misskey"})

	result := s.GetTimingConfigOverride("telegram", "")
	if result != nil {
		t.Error("should return nil when channel not enabled")
	}
}

func TestAdaptiveEngagementSyncer_GetTimingConfigOverride_ReturnsProfileValues(t *testing.T) {
	s := newTestSyncer(true, []string{"telegram"})
	s.UpdateTraits(BotProfileTraits{
		EnergyLevel: 0.9,
		Patience:    0.8,
		Verbosity:   0.7,
	})

	result := s.GetTimingConfigOverride("telegram", "")
	if result == nil {
		t.Fatal("result should not be nil")
	}

	if result.ReplyProbability == nil {
		t.Error("ReplyProbability should be set")
	}
	if result.BackoffBaseSeconds == nil {
		t.Error("BackoffBaseSeconds should be set")
	}
	if result.BackoffStartCount == nil {
		t.Error("BackoffStartCount should be set")
	}
}

// ============================================================================
// per-channel override
// ============================================================================

func TestAdaptiveEngagementSyncer_ChannelOverride(t *testing.T) {
	s := newTestSyncer(true, []string{"telegram"})

	prob := 0.99
	s.SetChannelOverride("telegram:-123456", channelEngagementOverride{
		ReplyProbability: &prob,
	})

	result := s.GetTimingConfigOverride("telegram", "-123456")
	if result == nil || result.ReplyProbability == nil || *result.ReplyProbability != 0.99 {
		t.Errorf("override should apply: got %v", result)
	}
}

func TestAdaptiveEngagementSyncer_ChannelOverride_Inheritance(t *testing.T) {
	s := newTestSyncer(true, []string{"telegram"})

	typeProb := 0.10
	chatProb := 0.80
	s.SetChannelOverride("telegram", channelEngagementOverride{
		ReplyProbability: &typeProb,
	})
	s.SetChannelOverride("telegram:-123456", channelEngagementOverride{
		ReplyProbability: &chatProb,
	})

	// 具体群聊级覆盖优先
	result := s.GetTimingConfigOverride("telegram", "-123456")
	if result == nil || *result.ReplyProbability != 0.80 {
		t.Errorf("chat-level override should win: got %v", result)
	}

	// 未覆盖的群聊继承类型级
	result2 := s.GetTimingConfigOverride("telegram", "-999999")
	if result2 == nil || *result2.ReplyProbability != 0.10 {
		t.Errorf("should inherit type-level override: got %v", result2)
	}
}

func TestAdaptiveEngagementSyncer_RemoveChannelOverride(t *testing.T) {
	s := newTestSyncer(true, []string{"telegram"})

	prob := 0.99
	s.SetChannelOverride("telegram", channelEngagementOverride{
		ReplyProbability: &prob,
	})
	s.RemoveChannelOverride("telegram")

	result := s.GetTimingConfigOverride("telegram", "")
	if result != nil && result.ReplyProbability != nil && *result.ReplyProbability == 0.99 {
		t.Error("override should have been removed")
	}
}

// ============================================================================
// ChannelKey helpers
// ============================================================================

func TestChannelKeyForType(t *testing.T) {
	if got := ChannelKeyForType("telegram"); got != "telegram" {
		t.Errorf("expected 'telegram', got %q", got)
	}
}

func TestChannelKeyForChat(t *testing.T) {
	if got := ChannelKeyForChat("telegram", "-123456"); got != "telegram:-123456" {
		t.Errorf("expected 'telegram:-123456', got %q", got)
	}
}

func TestChannelKeyForChat_NoChatID(t *testing.T) {
	if got := ChannelKeyForChat("telegram", ""); got != "telegram" {
		t.Errorf("expected 'telegram', got %q", got)
	}
}

// ============================================================================
// WriteProfileToManager (no-op, ensure no panic)
// ============================================================================

func TestAdaptiveEngagementSyncer_WriteProfileNoPanic(t *testing.T) {
	s := newTestSyncer(true, nil)
	// Should not panic
	s.WriteProfileToManager(context.TODO())
}
