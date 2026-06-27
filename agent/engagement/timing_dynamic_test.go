package engagement

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// TimingGate + DynamicConfigFunc 集成测试
// ============================================================================

func TestTimingGate_DynamicConfig_AppliesOverride(t *testing.T) {
	policy := newAlwaysEngagePolicy()
	gate := NewTimingGate(policy, DefaultTimingGateConfig())

	callCount := 0
	gate.SetDynamicConfig(func(channelType, chatID string) *channelEngagementOverride {
		callCount++
		prob := 1.0 // 强制 100% 参与
		return &channelEngagementOverride{
			ReplyProbability: &prob,
		}
	})

	msg := core.Message{
		ID:       "msg-1",
		Text:     "hello",
		Channel:  "telegram:-123",
		ChatType: "group",
		Metadata: map[string]any{
			"channel_type": "telegram",
			"chat_id":      "-123",
		},
	}

	shouldEval, td := gate.ShouldEvaluate(&msg)
	if !shouldEval {
		t.Errorf("dynamic config should make shouldEval=true, got false, reason=%s", td.Reason)
	}
	if callCount != 1 {
		t.Errorf("dynamic config callback should be called once, got %d", callCount)
	}
}

func TestTimingGate_DynamicConfig_ReturnsNil(t *testing.T) {
	policy := newAlwaysEngagePolicy()
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability: 0.01, // 极低概率 → 大概率跳过
	})

	// 动态配置返回 nil → 使用静态默认配置
	gate.SetDynamicConfig(func(channelType, chatID string) *channelEngagementOverride {
		return nil
	})

	msg := core.Message{
		ID:       "msg-99",
		Text:     "hello",
		Channel:  "telegram:-123",
		ChatType: "group",
		Metadata: map[string]any{
			"channel_type": "telegram",
			"chat_id":      "-123",
		},
	}

	shouldEval, _ := gate.ShouldEvaluate(&msg)
	// 概率极低 → 大概率 false（具体取决于概率门控）
	// 这里只验证不会 panic 且行为由静态配置控制
	_ = shouldEval
}

func TestTimingGate_DynamicConfig_HighEnergy(t *testing.T) {
	policy := newAlwaysEngagePolicy()
	gate := NewTimingGate(policy, DefaultTimingGateConfig())

	// 模拟高精力画像 → 高参与概率
	gate.SetDynamicConfig(func(channelType, chatID string) *channelEngagementOverride {
		prob := 1.0 // 100% 参与
		count := 5
		return &channelEngagementOverride{
			ReplyProbability:  &prob,
			BackoffStartCount: &count,
		}
	})

	msg := core.Message{
		ID:       "msg-energetic",
		Text:     "hello",
		Channel:  "telegram:-123",
		ChatType: "group",
		Metadata: map[string]any{
			"channel_type": "telegram",
			"chat_id":      "-123",
		},
	}

	shouldEval, _ := gate.ShouldEvaluate(&msg)
	if !shouldEval {
		t.Error("high energy dynamic config should always evaluate")
	}
}

func TestTimingGate_DynamicConfig_LowPatience(t *testing.T) {
	policy := newAlwaysEngagePolicy()
	// 直接使用静态配置模拟低耐心场景（RecordDecision 使用 g.config 而非动态回调）
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:   1.0,
		BackoffStartCount:  1,    // 从第1次就开始退避
		BackoffBaseSeconds: 60.0, // 高退避基准
	})

	// 注入动态配置回调验证 ShouldEvaluate 使用动态配置
	gate.SetDynamicConfig(func(channelType, chatID string) *channelEngagementOverride {
		prob := 1.0
		return &channelEngagementOverride{
			ReplyProbability: &prob,
		}
	})

	declineMsg := timelineMsg("next", "user2", "telegram")

	// RecordDecision 记录不参与 → low patience BackoffStartCount=1 → 退避
	gate.RecordDecision(declineMsg, Decision{Engage: false, Reason: "test decline"})

	// 检查是否进入了退避
	_, inBackoff, _ := gate.GetChannelState("ch-telegram")
	if !inBackoff {
		t.Error("low patience with backoff_start_count=1 should trigger backoff after 1 decline")
	}
}

func TestTimingGate_RejectionDetector_Integration(t *testing.T) {
	policy := newAlwaysEngagePolicy()
	gate := NewTimingGate(policy, DefaultTimingGateConfig())
	gate.SetRandomNoiseRate(0) // 关闭随机噪声

	detector := NewRejectionDetector(RejectionDetectorConfig{
		SilenceWindowSeconds: 120.0,
		StreakThreshold:      1,
		StreakDuration:       1,
		BotName:              "testbot",
	}, noop.NewTracerProvider(), zap.NewNop().Sugar())
	gate.SetRejectionDetector(detector)

	// 模拟被无视1次触发自闭
	detector.RecordReply("telegram:-123")
	// 等待后检查（需要 > silence window）
	// 注意：这里不能 sleep too long in unit tests...
	// 实际集成：TimingGate.ShouldEvaluate 会调用 detector.IsInStreak
	// 这里只验证 detector 注入后不会 panic
	msg := core.Message{
		ID:       "msg-detector",
		Text:     "hello",
		Channel:  "telegram:-123",
		ChatType: "group",
	}

	_, td := gate.ShouldEvaluate(&msg)
	// 验证不 panic
	_ = td
}

func TestTimingGate_RandomNoise_SkipsProfile(t *testing.T) {
	policy := newAlwaysEngagePolicy()
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability: 0.0, // 概率为0，不会主动参与
	})
	gate.SetRandomNoiseRate(1.0) // 100% 噪声 → 每次都跳过画像

	msg := core.Message{
		ID:       "msg-noise",
		Text:     "hello",
		Channel:  "telegram:-123",
		ChatType: "group",
	}

	shouldEval, td := gate.ShouldEvaluate(&msg)
	if !shouldEval {
		t.Errorf("random noise should force evaluation, got false, reason=%s", td.Reason)
	}
	if td.Reason != "random noise injection — spontaneous participation" {
		t.Errorf("expected noise reason, got %q", td.Reason)
	}
}

func TestTimingGate_RandomNoise_Disabled(t *testing.T) {
	policy := newAlwaysEngagePolicy()
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability: 0.001, // 极低概率但有门控
	})
	gate.SetRandomNoiseRate(0.0) // 关闭噪声

	msg := core.Message{
		ID:       "msg-no-noise",
		Text:     "hello",
		Channel:  "telegram:-123",
		ChatType: "group",
	}

	shouldEval, td := gate.ShouldEvaluate(&msg)
	// 概率极低 + 无噪声 → 大概率被概率门控跳过
	if shouldEval {
		t.Logf("evaluated despite low probability (reason=%s) — this is probabilistic, retry if flaky", td.Reason)
	}
	// 不强制要求 false，因为概率判定有随机性
}

// ============================================================================
// helpers
// ============================================================================

func newAlwaysEngagePolicy() EngagementPolicy {
	j := &mockAlwaysEngageJudge{}
	checker := NewSourceAllowlist("telegram")
	return NewCompositePolicy(checker, WithJudge(j))
}

type mockAlwaysEngageJudge struct{}

func (m *mockAlwaysEngageJudge) Judge(ctx context.Context, msg *core.Message) (JudgeResult, error) {
	return JudgeResult{Engage: true, Reason: "always engage"}, nil
}
