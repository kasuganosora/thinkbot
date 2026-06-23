package engagement

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func testLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

func testTracerProvider() trace.TracerProvider {
	return noop_trace.NewTracerProvider()
}

func newTestStage(policy EngagementPolicy) *EngagementStage {
	return NewEngagementStage(
		"engagement",
		policy,
		DefaultStageConfig(),
		testTracerProvider(),
		testLogger(),
	)
}

func newEnvelope(msg core.Message) *core.Envelope {
	return core.NewEnvelope(msg)
}

// ============================================================================
// EngagementStage tests
// ============================================================================

func TestEngagementStage_NilPolicy(t *testing.T) {
	stage := NewEngagementStage(
		"engagement",
		nil,
		DefaultStageConfig(),
		testTracerProvider(),
		testLogger(),
	)

	msg := *timelineMsg("hello", "user1", "misskey")
	env := newEnvelope(msg)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("result should not be nil")
	}
	// Nil policy → message passes through unchanged
	if result.Message.Mentioned {
		t.Error("Mentioned should not be set with nil policy")
	}
}

func TestEngagementStage_MentionedPassThrough(t *testing.T) {
	// 被 @ 的消息直接放行，不做 engagement 判断
	policy := NewCompositePolicy(DenyAll{}) // 即使 deny all，被 @ 的也放行
	stage := newTestStage(policy)

	msg := *mentionedMsg("hello @bot", "user1", "misskey")
	env := newEnvelope(msg)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Message.Mentioned {
		t.Error("mentioned message should pass through with Mentioned=true")
	}
}

func TestEngagementStage_ReadOnlyChannel(t *testing.T) {
	// RSS 等只读渠道的消息不参与
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey", "telegram"),
	)
	stage := newTestStage(policy)

	msg := *timelineMsg("interesting RSS article", "user1", "rss")
	env := newEnvelope(msg)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 但消息继续流转
	if result == nil {
		t.Fatal("message should not be dropped")
	}

	// 只读渠道 → 不升级
	if result.Message.Mentioned {
		t.Error("should not engage on read-only channel")
	}

	// Envelope 应标记评估结果
	if !WasEvaluated(result) {
		t.Error("should be marked as evaluated")
	}
	d := EngagementDecision(result)
	if d.Engage {
		t.Error("should not engage on read-only channel")
	}
	if d.Tier != TierCapability {
		t.Errorf("tier = %v, want %v", d.Tier, TierCapability)
	}
}

func TestEngagementStage_RulesReject(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("golang"), // 只匹配 golang
		)),
	)
	stage := newTestStage(policy)

	msg := *timelineMsg("boring weather post", "user1", "misskey")
	env := newEnvelope(msg)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 规则未通过 → 不升级
	if result.Message.Mentioned {
		t.Error("should not engage when rules reject")
	}
	if EngagementDecision(result).Tier != TierRule {
		t.Error("should be rejected at rule tier")
	}
}

func TestEngagementStage_Engage(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("golang"),
		)),
	)
	stage := newTestStage(policy)

	msg := *timelineMsg("Let's discuss golang patterns", "user1", "misskey")
	env := newEnvelope(msg)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 所有层通过 → 升级为 Mentioned
	if !result.Message.Mentioned {
		t.Error("should be promoted to Mentioned on engage")
	}
	if !IsProactiveEngagement(result) {
		t.Error("should be marked as proactive engagement")
	}
	d := EngagementDecision(result)
	if !d.Engage {
		t.Error("decision should be engage=true")
	}
	if d.Tier != TierPass {
		t.Errorf("tier = %v, want %v", d.Tier, TierPass)
	}
}

func TestEngagementStage_SetsReplyTarget(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
	)
	stage := newTestStage(policy)

	msg := core.Message{
		ID:        "msg-1",
		BotID:     "bot-1",
		Source:    "misskey",
		Channel:   "ch-misskey",
		UserID:    "user1",
		Text:      "hello",
		Mentioned: false,
		Metadata:  map[string]any{}, // 故意不设 reply_target
		CreatedAt: time.Now(),
	}
	env := newEnvelope(msg)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Message.Mentioned {
		t.Fatal("should engage")
	}

	rt, ok := result.Message.Metadata["reply_target"]
	if !ok || rt == "" {
		t.Error("reply_target should be set after engagement")
	}
}

func TestEngagementStage_DoesNotOverwriteReplyTarget(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
	)
	stage := newTestStage(policy)

	msg := core.Message{
		ID:        "msg-1",
		BotID:     "bot-1",
		Source:    "misskey",
		Channel:   "ch-misskey",
		UserID:    "user1",
		Text:      "hello",
		Mentioned: false,
		Metadata: map[string]any{
			"reply_target": "note-abc123",
			"note_id":      "note-abc123",
		},
		CreatedAt: time.Now(),
	}
	env := newEnvelope(msg)

	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rt, _ := result.Message.Metadata["reply_target"].(string)
	if rt != "note-abc123" {
		t.Errorf("reply_target = %q, want 'note-abc123'", rt)
	}
}

func TestEngagementStage_ConsumeToken(t *testing.T) {
	bucket := NewTokenBucket(2, 1*time.Hour)
	rateRule := NewRateLimitRule(bucket)

	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("golang"),
			rateRule,
		)),
	)
	stage := newTestStage(policy)

	// 第一次参与 → 消耗 1 个令牌
	msg1 := *timelineMsg("golang discussion 1", "user1", "misskey")
	env1 := newEnvelope(msg1)
	_, _ = stage.Process(context.Background(), env1)

	// 容量为 2，消耗 1 个后剩余 ~1（浮点精度容忍）
	avail1 := bucket.Available()
	if avail1 < 0.99 || avail1 > 1.01 {
		t.Errorf("after 1 engagement, available = %v, want ~1", avail1)
	}

	// 第二次参与 → 消耗另 1 个令牌
	msg2 := *timelineMsg("golang discussion 2", "user2", "misskey")
	env2 := newEnvelope(msg2)
	_, _ = stage.Process(context.Background(), env2)

	// 消耗完后剩余 ~0
	if bucket.Available() > 0.01 {
		t.Errorf("after 2 engagements, available = %v, want ~0", bucket.Available())
	}

	// 第三次 → 限流拒绝
	msg3 := *timelineMsg("golang discussion 3", "user3", "misskey")
	env3 := newEnvelope(msg3)
	result, _ := stage.Process(context.Background(), env3)

	if result.Message.Mentioned {
		t.Error("should be rate limited on 3rd attempt")
	}
}

func TestEngagementStage_DeclinedDoesNotConsumeToken(t *testing.T) {
	bucket := NewTokenBucket(1, 1*time.Hour)
	rateRule := NewRateLimitRule(bucket)

	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("golang"), // 只匹配 golang
			rateRule,
		)),
	)
	stage := newTestStage(policy)

	// 不匹配关键词 → 被规则拒绝，不应消耗令牌
	msg := *timelineMsg("boring weather", "user1", "misskey")
	env := newEnvelope(msg)
	_, _ = stage.Process(context.Background(), env)

	if bucket.Available() != 1 {
		t.Errorf("declined message should not consume token, available = %v, want 1", bucket.Available())
	}
}

func TestEngagementStage_Tier2DeclineRefundsToken(t *testing.T) {
	// Tier 1 全部通过（RateLimitRule 预扣令牌），但 Tier 2 LLM Judge 拒绝
	// → 令牌应被退还，不泄漏
	bucket := NewTokenBucket(1, 1*time.Hour)
	rateRule := NewRateLimitRule(bucket)

	// mockJudge 总是拒绝
	mockJudge := &mockLLMJudge{engage: false, reason: "not interesting"}

	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("golang"),
			rateRule,
		)),
		WithJudge(mockJudge),
	)
	stage := newTestStage(policy)

	// 消息匹配关键词 + 有令牌 → Tier 1 通过，Tier 2 拒绝
	msg := *timelineMsg("golang is great", "user1", "misskey")
	env := newEnvelope(msg)
	_, _ = stage.Process(context.Background(), env)

	// 令牌应被退还
	if bucket.Available() != 1 {
		t.Errorf("Tier 2 decline should refund token, available = %v, want 1", bucket.Available())
	}
}

// mockLLMJudge implements LLMJudge for testing.
type mockLLMJudge struct {
	engage bool
	reason string
	score  int
}

func (m *mockLLMJudge) Judge(_ context.Context, _ *core.Message) (JudgeResult, error) {
	return JudgeResult{Engage: m.engage, Reason: m.reason, Score: m.score}, nil
}

func TestEngagementSummary(t *testing.T) {
	// Not evaluated
	env := newEnvelope(*mentionedMsg("hi", "user1", "misskey"))
	summary := EngagementSummary(env)
	if summary == "" {
		t.Error("summary should not be empty")
	}

	// Engaged
	env2 := newEnvelope(*timelineMsg("golang rocks", "user1", "misskey"))
	env2.Set("engagement.evaluated", true)
	env2.Set("engagement.engage", true)
	env2.Set("engagement.reason", "all passed")
	env2.Set("engagement.tier", "pass")
	summary2 := EngagementSummary(env2)
	if summary2 == "" {
		t.Error("summary should not be empty for engaged message")
	}

	// Declined
	env3 := newEnvelope(*timelineMsg("hello", "user1", "misskey"))
	env3.Set("engagement.evaluated", true)
	env3.Set("engagement.engage", false)
	env3.Set("engagement.reason", "not writable")
	env3.Set("engagement.tier", "capability")
	summary3 := EngagementSummary(env3)
	if summary3 == "" {
		t.Error("summary should not be empty for declined message")
	}
}

// ============================================================================
// Integration: Engagement → Session resolver compatibility
// ============================================================================

func TestEngagementThenSessionResolve(t *testing.T) {
	// 模拟完整的 Engagement → Session 流程：
	// 1. 时间线帖子进来（Mentioned=false）
	// 2. EngagementStage 评估并升级
	// 3. 升级后 Mentioned=true → Session resolver 会创建 session

	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
	)
	stage := newTestStage(policy)

	msg := *timelineMsg("interesting golang discussion", "user1", "misskey")
	env := newEnvelope(msg)

	// Engagement 阶段
	result, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 验证升级
	if !result.Message.Mentioned {
		t.Fatal("message should be promoted after engagement")
	}

	// 模拟 Session resolver 检查
	// MisskeyResolver: Mentioned=true → mk:channel:{channel}
	if !result.Message.Mentioned {
		t.Error("Mentioned should be true for session resolver to pick up")
	}
}
