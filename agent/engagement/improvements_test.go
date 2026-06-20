package engagement

import (
	"context"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/config"
)

// ============================================================================
// P0: ScoredJudge tests
// ============================================================================

func TestParseScoredResponse_ValidScore(t *testing.T) {
	tests := []struct {
		input     string
		wantScore int
		wantEng   bool
	}{
		{"85 interesting topic about golang", 85, true},
		{"85: interesting topic about golang", 85, true},
		{"42 not very interesting", 42, false},
		{"100 perfect match", 100, true},
		{"0 completely irrelevant", 0, false},
		{"50 borderline case", 50, true},
		{"49 just below threshold", 49, false},
		{"Score: 75 - good topic", 75, true},
		{"评分: 90 非常好的话题", 90, true},
	}

	for _, tt := range tests {
		result := ParseScoredResponse(tt.input)
		if result.Score != tt.wantScore {
			t.Errorf("ParseScoredResponse(%q).Score = %d, want %d", tt.input, result.Score, tt.wantScore)
		}
		if result.Engage != tt.wantEng {
			t.Errorf("ParseScoredResponse(%q).Engage = %v, want %v", tt.input, result.Engage, tt.wantEng)
		}
		if result.Reason == "" {
			t.Errorf("ParseScoredResponse(%q).Reason should not be empty", tt.input)
		}
	}
}

func TestParseScoredResponse_FallbackToBinary(t *testing.T) {
	// 无前导数字时应回退到 YES/NO 解析
	result := ParseScoredResponse("YES this is great")
	if result.Score != 0 {
		t.Errorf("fallback should have Score=0, got %d", result.Score)
	}
	if !result.Engage {
		t.Error("fallback should parse YES as Engage=true")
	}

	result = ParseScoredResponse("NO boring")
	if result.Engage {
		t.Error("fallback should parse NO as Engage=false")
	}
}

func TestParseScoredResponse_InvalidScore(t *testing.T) {
	result := ParseScoredResponse("999 way too high")
	if result.Engage {
		t.Error("score > 100 should be rejected")
	}
	if result.Score != 0 {
		t.Errorf("invalid score should have Score=0, got %d", result.Score)
	}
}

func TestSimpleJudge_ScoredMode(t *testing.T) {
	client := &mockLLMClient{response: "85 excellent discussion about golang"}
	judge := NewScoredSimpleJudge(client, DefaultPromptConfig())

	msg := timelineMsg("Let's discuss golang concurrency", "user1", "misskey")
	result, err := judge.Judge(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Score != 85 {
		t.Errorf("Score = %d, want 85", result.Score)
	}
	if !result.Engage {
		t.Error("score 85 should be Engage=true (>=50)")
	}
}

func TestSimpleJudge_ScoredMode_LowScore(t *testing.T) {
	client := &mockLLMClient{response: "25 not interesting at all"}
	judge := NewScoredSimpleJudge(client, DefaultPromptConfig())

	msg := timelineMsg("boring content", "user1", "misskey")
	result, err := judge.Judge(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Score != 25 {
		t.Errorf("Score = %d, want 25", result.Score)
	}
	if result.Engage {
		t.Error("score 25 should be Engage=false (<50)")
	}
}

func TestCompositePolicy_ScoredThreshold(t *testing.T) {
	// Score 80, threshold 75 → pass
	client := &mockLLMClient{response: "80 good topic"}
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine()),
		WithJudge(NewScoredSimpleJudge(client, DefaultPromptConfig())),
		WithEngagementThreshold(75),
	)

	msg := timelineMsg("interesting post", "user1", "misskey")
	decision := policy.Evaluate(context.Background(), msg)
	if !decision.Engage {
		t.Error("score 80 >= threshold 75 should engage")
	}
	if decision.Tier != TierPass {
		t.Errorf("tier = %v, want %v", decision.Tier, TierPass)
	}
}

func TestCompositePolicy_ScoredThreshold_BelowThreshold(t *testing.T) {
	// Score 60, threshold 75 → reject
	client := &mockLLMClient{response: "60 somewhat interesting"}
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine()),
		WithJudge(NewScoredSimpleJudge(client, DefaultPromptConfig())),
		WithEngagementThreshold(75),
	)

	msg := timelineMsg("mildly interesting post", "user1", "misskey")
	decision := policy.Evaluate(context.Background(), msg)
	if decision.Engage {
		t.Error("score 60 < threshold 75 should not engage")
	}
	if decision.Tier != TierLLM {
		t.Errorf("tier = %v, want %v", decision.Tier, TierLLM)
	}
	// Verify metadata contains score info
	if score, ok := decision.Metadata["score"].(int); !ok || score != 60 {
		t.Errorf("metadata should contain score=60, got %v", decision.Metadata["score"])
	}
}

func TestCompositePolicy_ScoredThreshold_Disabled(t *testing.T) {
	// Threshold = 0 → use traditional YES/NO mode
	client := &mockLLMClient{response: "NO boring"}
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine()),
		WithJudge(NewSimpleJudge(client, DefaultPromptConfig())), // non-scored
		WithEngagementThreshold(0),                               // disabled
	)

	msg := timelineMsg("some post", "user1", "misskey")
	decision := policy.Evaluate(context.Background(), msg)
	if decision.Engage {
		t.Error("non-scored judge with NO should not engage")
	}
}

func TestCompositePolicy_BackwardCompat_BinaryJudge(t *testing.T) {
	// Existing YES/NO judge should still work without threshold
	client := &mockLLMClient{response: "YES interesting"}
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine()),
		WithJudge(NewSimpleJudge(client, DefaultPromptConfig())), // traditional mode
		// No WithEngagementThreshold → threshold = 0
	)

	msg := timelineMsg("golang discussion", "user1", "misskey")
	decision := policy.Evaluate(context.Background(), msg)
	if !decision.Engage {
		t.Error("YES from binary judge should engage")
	}
	if decision.Tier != TierPass {
		t.Errorf("tier = %v, want %v", decision.Tier, TierPass)
	}
}

// ============================================================================
// P1: EngagementProfile tests
// ============================================================================

func TestBuiltinProfiles_AllExist(t *testing.T) {
	expected := []string{"observer", "lurker", "moderator", "active"}
	for _, name := range expected {
		if _, ok := BuiltinProfiles[name]; !ok {
			t.Errorf("built-in profile %q should exist", name)
		}
	}
}

func TestApplyProfile(t *testing.T) {
	cfg := config.EngagementConfig{
		ReplyProbability: 0.5,
	}

	ok := ApplyProfile(&cfg, "observer")
	if !ok {
		t.Fatal("ApplyProfile should succeed for 'observer'")
	}

	if cfg.ReplyProbability != 0.05 {
		t.Errorf("ReplyProbability = %v, want 0.05", cfg.ReplyProbability)
	}
	if cfg.EngagementThreshold != 85 {
		t.Errorf("EngagementThreshold = %v, want 85", cfg.EngagementThreshold)
	}
	if cfg.BackoffStartCount != 2 {
		t.Errorf("BackoffStartCount = %v, want 2", cfg.BackoffStartCount)
	}
	if cfg.RateLimitCapacity != 2 {
		t.Errorf("RateLimitCapacity = %v, want 2", cfg.RateLimitCapacity)
	}
}

func TestApplyProfile_UnknownProfile(t *testing.T) {
	cfg := config.EngagementConfig{ReplyProbability: 0.5}
	ok := ApplyProfile(&cfg, "nonexistent")
	if ok {
		t.Error("ApplyProfile should return false for unknown profile")
	}
	if cfg.ReplyProbability != 0.5 {
		t.Error("config should be unchanged when profile not found")
	}
}

func TestApplyProfile_AllProfiles(t *testing.T) {
	// Each profile should set different values
	cfg := config.EngagementConfig{}
	ApplyProfile(&cfg, "lurker")
	lurkerProb := cfg.ReplyProbability

	ApplyProfile(&cfg, "active")
	activeProb := cfg.ReplyProbability

	ApplyProfile(&cfg, "observer")
	observerProb := cfg.ReplyProbability

	// observer should be lowest, active should be highest
	if observerProb >= lurkerProb {
		t.Errorf("observer (%v) should have lower probability than lurker (%v)", observerProb, lurkerProb)
	}
	if activeProb <= lurkerProb {
		t.Errorf("active (%v) should have higher probability than lurker (%v)", activeProb, lurkerProb)
	}
}

func TestBuildFromConfig_WithProfile(t *testing.T) {
	cfg := config.EngagementConfig{
		Enabled:  true,
		Profile:  "observer",
		Channels: []string{"misskey"},
	}

	result := BuildFromConfig(cfg, "bot-123", nil)
	if result.Policy == nil {
		t.Fatal("expected non-nil policy")
	}

	// Profile "observer" sets ReplyProbability to 0.05 > 0, so gate should exist
	if result.Gate == nil {
		t.Fatal("expected non-nil gate (observer profile sets ReplyProbability > 0)")
	}
}

func TestProfileNames(t *testing.T) {
	names := ProfileNames()
	if len(names) != 4 {
		t.Errorf("expected 4 profiles, got %d", len(names))
	}
}

// ============================================================================
// P2: AutoAdjustFrequency tests
// ============================================================================

func TestTimingGate_AutoAdjustActiveGroup(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     0.1,
		BurstIntervalSeconds: 0.1,
		AutoAdjustFrequency:  true,
		// Small bounds to see change clearly
		MinFrequencyMultiplier: 0.2,
		MaxFrequencyMultiplier: 5.0,
	})

	// Simulate an active group: many messages with short intervals
	// Need at least 3 interval samples for autoAdjust to kick in
	for i := 0; i < 10; i++ {
		msg := timelineMsg("msg "+string(rune('A'+i%26)), "user1", "misskey")
		msg.CreatedAt = time.Now()
		// Add small delay between messages by setting lastExternalMsgAt
		gate.mu.Lock()
		state := gate.getOrCreateState("ch-misskey")
		state.lastExternalMsgAt = msg.CreatedAt.Add(-15 * time.Second) // 15s interval
		gate.mu.Unlock()
		gate.Evaluate(context.Background(), msg)
	}

	// In an active group, frequency should have been adjusted
	gate.mu.Lock()
	multiplier := gate.config.FrequencyMultiplier
	gate.mu.Unlock()

	// Active group (15s intervals ≈ 4 msgs/min) should trend toward > 1.0
	if multiplier <= 0.99 {
		t.Errorf("active group should increase frequency multiplier, got %v", multiplier)
	}
}

func TestTimingGate_AutoAdjustBounded(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:       1.0, // always evaluate
		BurstIntervalSeconds:   0,
		BackoffStartCount:      100, // prevent backoff
		AutoAdjustFrequency:    true,
		MinFrequencyMultiplier: 0.5,
		MaxFrequencyMultiplier: 2.0,
	})

	// Feed many messages to trigger multiple autoAdjust calls
	for i := 0; i < 20; i++ {
		msg := timelineMsg("msg "+string(rune('A'+i%26)), "user1", "misskey")
		msg.CreatedAt = time.Now()
		gate.mu.Lock()
		state := gate.getOrCreateState("ch-misskey")
		state.lastExternalMsgAt = msg.CreatedAt.Add(-10 * time.Second)
		// Record interval
		if !state.lastExternalMsgAt.IsZero() {
			interval := msg.CreatedAt.Sub(state.lastExternalMsgAt).Seconds()
			if interval >= 0.1 {
				state.recentIntervals = append(state.recentIntervals, interval)
			}
		}
		gate.mu.Unlock()
		gate.Evaluate(context.Background(), msg)
	}

	gate.mu.Lock()
	multiplier := gate.config.FrequencyMultiplier
	gate.mu.Unlock()

	if multiplier < 0.5 {
		t.Errorf("multiplier should be >= min (0.5), got %v", multiplier)
	}
	if multiplier > 2.0 {
		t.Errorf("multiplier should be <= max (2.0), got %v", multiplier)
	}
}

func TestTimingGate_AutoAdjustDisabled(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 0,
		BackoffStartCount:    100,
		AutoAdjustFrequency:  false, // disabled
	})

	originalMult := 1.0
	for i := 0; i < 10; i++ {
		msg := timelineMsg("msg "+string(rune('A'+i%26)), "user1", "misskey")
		gate.Evaluate(context.Background(), msg)
	}

	gate.mu.Lock()
	multiplier := gate.config.FrequencyMultiplier
	gate.mu.Unlock()

	if multiplier != originalMult {
		t.Errorf("with AutoAdjustFrequency disabled, multiplier should stay at %v, got %v", originalMult, multiplier)
	}
}

// ============================================================================
// P3: ConversationPhase tests
// ============================================================================

func TestInferPhase_Idle(t *testing.T) {
	now := time.Now()
	// Last message was 10 minutes ago
	lastMsg := now.Add(-10 * time.Minute)
	phase := inferPhase([]float64{5, 10, 8}, now, lastMsg)
	if phase != PhaseIdle {
		t.Errorf("expected PhaseIdle, got %s", phase)
	}
}

func TestInferPhase_Divergent(t *testing.T) {
	now := time.Now()
	lastMsg := now.Add(-5 * time.Second)
	// Short intervals (< 30s)
	intervals := []float64{10, 15, 12, 8, 14}
	phase := inferPhase(intervals, now, lastMsg)
	if phase != PhaseDivergent {
		t.Errorf("expected PhaseDivergent for active conversation, got %s", phase)
	}
}

func TestInferPhase_Convergent(t *testing.T) {
	now := time.Now()
	lastMsg := now.Add(-5 * time.Second)
	// Increasing intervals (conversation cooling down)
	intervals := []float64{5, 8, 15, 30, 60, 120}
	phase := inferPhase(intervals, now, lastMsg)
	if phase != PhaseConvergent {
		t.Errorf("expected PhaseConvergent for cooling conversation, got %s", phase)
	}
}

func TestInferPhase_NotEnoughSamples(t *testing.T) {
	now := time.Now()
	lastMsg := now.Add(-5 * time.Second)
	phase := inferPhase([]float64{10}, now, lastMsg)
	// < 3 samples → default to Divergent
	if phase != PhaseDivergent {
		t.Errorf("expected PhaseDivergent for insufficient samples, got %s", phase)
	}
}

func TestFrequencyMultiplierForPhase(t *testing.T) {
	tests := []struct {
		phase ConversationPhase
		min   float64
		max   float64
	}{
		{PhaseIdle, 0.0, 0.6},
		{PhaseDivergent, 1.0, 2.0},
		{PhaseConvergent, 0.5, 0.8},
	}

	for _, tt := range tests {
		mult := frequencyMultiplierForPhase(tt.phase)
		if mult < tt.min || mult > tt.max {
			t.Errorf("frequencyMultiplierForPhase(%s) = %v, want [%v, %v]", tt.phase, mult, tt.min, tt.max)
		}
	}
}

func TestTimingGate_GetPhase(t *testing.T) {
	policy := NewCompositePolicy(NewSourceAllowlist("misskey"))
	gate := NewTimingGate(policy, TimingGateConfig{
		ReplyProbability:     1.0,
		BurstIntervalSeconds: 0,
		BackoffStartCount:    100,
	})

	// No state yet → idle
	phase := gate.GetPhase("ch-misskey")
	if phase != PhaseIdle {
		t.Errorf("new channel should be PhaseIdle, got %s", phase)
	}

	// Feed messages with short intervals → should be divergent
	gate.mu.Lock()
	state := gate.getOrCreateState("ch-misskey")
	state.lastMsgAt = time.Now()
	state.lastExternalMsgAt = time.Now()
	state.recentIntervals = []float64{10, 15, 12, 8, 14}
	gate.mu.Unlock()

	phase = gate.GetPhase("ch-misskey")
	if phase != PhaseDivergent {
		t.Errorf("active conversation should be PhaseDivergent, got %s", phase)
	}
}

// ============================================================================
// Integration: BuildFromConfig with all new features
// ============================================================================

func TestBuildFromConfig_AllFeatures(t *testing.T) {
	cfg := config.EngagementConfig{
		Enabled:             true,
		Channels:            []string{"misskey"},
		LLMJudgeEnabled:     true,
		EngagementThreshold: 75,
		Profile:             "moderator",
		AutoAdjustFrequency: true,
	}

	// Create a scored judge
	client := &mockLLMClient{response: "80 good topic"}
	judge := NewScoredSimpleJudge(client, DefaultPromptConfig())

	result := BuildFromConfig(cfg, "bot-123", judge)

	if result.Policy == nil {
		t.Fatal("expected non-nil policy")
	}
	if result.Gate == nil {
		t.Fatal("expected non-nil gate")
	}

	// Verify the gate has AutoAdjustFrequency enabled
	result.Gate.mu.Lock()
	autoAdjust := result.Gate.config.AutoAdjustFrequency
	result.Gate.mu.Unlock()
	if !autoAdjust {
		t.Error("gate should have AutoAdjustFrequency enabled")
	}

	// Verify policy has threshold set (moderator profile sets threshold=60,
	// but explicit config sets 75 — profile overrides because it runs first
	// in BuildFromConfig, then the explicit EngagementThreshold from config
	// was already consumed by ApplyProfile)
	cp := result.Policy // already *CompositePolicy
	if cp.engagementThreshold != 60 {
		t.Errorf("threshold should be 60 (from moderator profile), got %d", cp.engagementThreshold)
	}
}

func TestBuildFromConfig_ScoredJudgeTriggered(t *testing.T) {
	cfg := config.EngagementConfig{
		Enabled:             true,
		Channels:            []string{"misskey"},
		LLMJudgeEnabled:     true,
		EngagementThreshold: 75,
	}

	// Score 80 should pass threshold 75
	client := &mockLLMClient{response: "80 great discussion"}
	judge := NewScoredSimpleJudge(client, DefaultPromptConfig())

	result := BuildFromConfig(cfg, "bot-123", judge)

	msg := &core.Message{
		ID:      "msg-1",
		Source:  "misskey",
		Channel: "ch-misskey",
		UserID:  "user1",
		Text:    "interesting golang topic",
	}
	decision := result.Policy.Evaluate(context.Background(), msg)
	if !decision.Engage {
		t.Error("score 80 >= threshold 75 should engage")
	}
}

// Ensure mockLLMClient from engagement_test.go is reused correctly
var _ SimpleLLMClient = (*mockLLMClient)(nil)
