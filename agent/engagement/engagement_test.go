package engagement

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// helpers
// ============================================================================

func timelineMsg(text, userID, source string) *core.Message {
	return &core.Message{
		ID:        "msg-" + text,
		TraceID:   "trace-1",
		BotID:     "bot-1",
		Source:    source,
		Channel:   "ch-" + source,
		UserID:    userID,
		Text:      text,
		Mentioned: false,
		Metadata: map[string]any{
			"note_id":      "note-" + text,
			"reply_target": "note-" + text,
		},
		CreatedAt: time.Now(),
	}
}

func mentionedMsg(text, userID, source string) *core.Message {
	m := timelineMsg(text, userID, source)
	m.Mentioned = true
	return m
}

// ============================================================================
// SourceAllowlist tests
// ============================================================================

func TestSourceAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		source  string
		want    bool
	}{
		{"allowed source", []string{"misskey", "telegram"}, "misskey", true},
		{"allowed telegram", []string{"misskey", "telegram"}, "telegram", true},
		{"blocked rss", []string{"misskey"}, "rss", false},
		{"empty allowlist blocks all", []string{}, "misskey", false},
		{"unknown source blocked", []string{"misskey"}, "unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := NewSourceAllowlist(tt.allowed...)
			msg := timelineMsg("hello", "user1", tt.source)
			if got := checker.IsWritable(msg); got != tt.want {
				t.Errorf("IsWritable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowAll(t *testing.T) {
	checker := AllowAll{}
	msg := timelineMsg("hello", "user1", "rss")
	if !checker.IsWritable(msg) {
		t.Error("AllowAll should always return true")
	}
}

func TestDenyAll(t *testing.T) {
	checker := DenyAll{}
	msg := timelineMsg("hello", "user1", "misskey")
	if checker.IsWritable(msg) {
		t.Error("DenyAll should always return false")
	}
}

// ============================================================================
// Rule tests
// ============================================================================

func TestKeywordRule(t *testing.T) {
	rule := NewKeywordRule("golang", "编程", "rust")

	tests := []struct {
		text string
		want bool
	}{
		{"I love golang programming", true},
		{"今天学了点编程", true},
		{"rust is awesome", true},
		{"今天天气不错", false},
		{"", false},
	}

	for _, tt := range tests {
		msg := timelineMsg(tt.text, "user1", "misskey")
		allow, _ := rule.Allow(msg)
		if allow != tt.want {
			t.Errorf("KeywordRule.Allow(%q) = %v, want %v", tt.text, allow, tt.want)
		}
	}
}

func TestKeywordRule_EmptyKeywords(t *testing.T) {
	rule := NewKeywordRule()
	msg := timelineMsg("anything", "user1", "misskey")
	allow, _ := rule.Allow(msg)
	if !allow {
		t.Error("KeywordRule with no keywords should allow all")
	}
}

func TestBlocklistRule(t *testing.T) {
	rule := NewBlocklistRule(
		[]string{"spammer", "bot123"},
		[]string{"rss"},
	)

	tests := []struct {
		name   string
		userID string
		source string
		want   bool
	}{
		{"normal user", "user1", "misskey", true},
		{"blocked user", "spammer", "misskey", false},
		{"blocked user 2", "bot123", "telegram", false},
		{"blocked source", "user1", "rss", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := timelineMsg("hello", tt.userID, tt.source)
			allow, _ := rule.Allow(msg)
			if allow != tt.want {
				t.Errorf("BlocklistRule.Allow() = %v, want %v", allow, tt.want)
			}
		})
	}
}

func TestLengthRule(t *testing.T) {
	rule := NewLengthRule(3, 200)

	tests := []struct {
		text string
		want bool
	}{
		{"hello world", true},
		{"hi", false},  // too short (2 < 3)
		{"你好世界", true}, // 4 runes >= 3
		{"好", false},   // too short (1 < 3)
	}

	for _, tt := range tests {
		msg := timelineMsg(tt.text, "user1", "misskey")
		allow, _ := rule.Allow(msg)
		if allow != tt.want {
			t.Errorf("LengthRule.Allow(%q) = %v, want %v", tt.text, allow, tt.want)
		}
	}
}

func TestSelfExclusionRule(t *testing.T) {
	rule := NewSelfExclusionRule("bot-self")

	msg := timelineMsg("hello", "user1", "misskey")
	allow, _ := rule.Allow(msg)
	if !allow {
		t.Error("should allow non-self message")
	}

	msg = timelineMsg("self post", "bot-self", "misskey")
	allow, _ = rule.Allow(msg)
	if allow {
		t.Error("should block self message")
	}

	// 空 botUserID 不应阻止任何消息
	emptyRule := NewSelfExclusionRule("")
	msg = timelineMsg("anything", "anyone", "misskey")
	allow, _ = emptyRule.Allow(msg)
	if !allow {
		t.Error("empty botUserID should allow all messages")
	}
}

func TestSelfExclusionRuleFunc(t *testing.T) {
	// 模拟共享 SelfIDSet 的行为
	knownIDs := map[string]bool{"bot-123": true, "bot-tg": true}
	checker := func(userID string) bool { return knownIDs[userID] }

	rule := NewSelfExclusionRuleFunc(checker)

	// 非 bot 用户 → 放行
	msg := timelineMsg("hello", "user-1", "misskey")
	allow, _ := rule.Allow(msg)
	if !allow {
		t.Error("should allow non-self message")
	}

	// bot 用户 → 阻止
	msg = timelineMsg("self post", "bot-123", "misskey")
	allow, _ = rule.Allow(msg)
	if allow {
		t.Error("should block self message (bot-123)")
	}

	// 另一个已注册 bot ID → 阻止
	msg = timelineMsg("tg post", "bot-tg", "telegram")
	allow, _ = rule.Allow(msg)
	if allow {
		t.Error("should block self message (bot-tg)")
	}

	// 运行时动态注册新 ID（模拟 Channel Start 后注册）
	knownIDs["bot-new"] = true
	msg = timelineMsg("new bot", "bot-new", "misskey")
	allow, _ = rule.Allow(msg)
	if allow {
		t.Error("should block dynamically registered self message")
	}
}

func TestRenoteExclusionRule(t *testing.T) {
	rule := NewRenoteExclusionRule()

	// 正常帖子
	msg := timelineMsg("hello world", "user1", "misskey")
	allow, _ := rule.Allow(msg)
	if !allow {
		t.Error("should allow normal message")
	}

	// 纯转发（有 renote_id 但无文本）
	msg = timelineMsg("", "user1", "misskey")
	msg.Metadata["renote_id"] = "note-abc"
	allow, _ = rule.Allow(msg)
	if allow {
		t.Error("should block pure renote")
	}

	// 带评论的转发（有 renote_id 但有文本）
	msg = timelineMsg("this is interesting", "user1", "misskey")
	msg.Metadata["renote_id"] = "note-abc"
	allow, _ = rule.Allow(msg)
	if !allow {
		t.Error("should allow renote with comment")
	}
}

func TestCooldownRule(t *testing.T) {
	rule := NewCooldownRule(100 * time.Millisecond)

	msg1 := timelineMsg("first", "user1", "misskey")
	allow, _ := rule.Allow(msg1)
	if !allow {
		t.Error("first message should be allowed")
	}

	// 同一用户冷却中
	msg2 := timelineMsg("second", "user1", "misskey")
	allow, _ = rule.Allow(msg2)
	if allow {
		t.Error("second message within cooldown should be blocked")
	}

	// 不同用户不受影响
	msg3 := timelineMsg("hello", "user2", "misskey")
	allow, _ = rule.Allow(msg3)
	if !allow {
		t.Error("different user should be allowed")
	}

	// 等待冷却过期
	time.Sleep(150 * time.Millisecond)
	msg4 := timelineMsg("after cooldown", "user1", "misskey")
	allow, _ = rule.Allow(msg4)
	if !allow {
		t.Error("message after cooldown should be allowed")
	}
}

func TestCooldownRule_Reset(t *testing.T) {
	rule := NewCooldownRule(1 * time.Hour)

	msg := timelineMsg("hello", "user1", "misskey")
	rule.Allow(msg)

	// 冷却中
	allow, _ := rule.Allow(msg)
	if allow {
		t.Error("should be on cooldown")
	}

	// Reset 后应该放行
	rule.Reset("user1")
	allow, _ = rule.Allow(msg)
	if !allow {
		t.Error("should be allowed after reset")
	}
}

// ============================================================================
// RuleEngine tests
// ============================================================================

func TestRuleEngine_AllPass(t *testing.T) {
	engine := NewRuleEngine(
		NewLengthRule(1, 1000),
		NewKeywordRule("golang"),
	)

	msg := timelineMsg("I love golang", "user1", "misskey")
	if !engine.Allow(msg) {
		t.Error("should allow when all rules pass")
	}
}

func TestRuleEngine_OneFails(t *testing.T) {
	engine := NewRuleEngine(
		NewLengthRule(1, 1000),
		NewKeywordRule("rust"), // 不匹配
	)

	msg := timelineMsg("I love golang", "user1", "misskey")
	if engine.Allow(msg) {
		t.Error("should deny when one rule fails")
	}
	if engine.LastReason() == "" {
		t.Error("LastReason should be set on denial")
	}
}

func TestRuleEngine_ShortCircuit(t *testing.T) {
	called := false
	engine := NewRuleEngine(
		NewLengthRule(1, 5), // 先失败
		RuleFunc(func(msg *core.Message) (bool, string) {
			called = true
			return true, ""
		}),
	)

	msg := timelineMsg("hello world this is long", "user1", "misskey")
	engine.Allow(msg)
	if called {
		t.Error("second rule should not be called when first fails")
	}
}

// ============================================================================
// RateLimiter tests
// ============================================================================

func TestTokenBucket(t *testing.T) {
	bucket := NewTokenBucket(3, 100*time.Millisecond)

	// 前 3 个令牌可用
	for i := 0; i < 3; i++ {
		if !bucket.TryTake() {
			t.Errorf("token %d should be available", i+1)
		}
	}

	// 第 4 个应失败
	if bucket.TryTake() {
		t.Error("4th token should not be available")
	}

	// 等待补充
	time.Sleep(150 * time.Millisecond)
	if !bucket.TryTake() {
		t.Error("token should be refilled after waiting")
	}
}

func TestSlidingWindow(t *testing.T) {
	window := NewSlidingWindow(3, 100*time.Millisecond)

	for i := 0; i < 3; i++ {
		if !window.Allow() {
			t.Errorf("call %d should be allowed", i+1)
		}
	}

	if window.Allow() {
		t.Error("4th call should be blocked")
	}

	// 等待窗口滑动
	time.Sleep(150 * time.Millisecond)
	if !window.Allow() {
		t.Error("call after window slide should be allowed")
	}
}

// ============================================================================
// LLM Judge tests
// ============================================================================

type mockLLMClient struct {
	response string
	err      error
	calls    int
	mu       sync.Mutex
}

func (m *mockLLMClient) Chat(_ context.Context, _, _ string) (string, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	return m.response, m.err
}

func TestParseJudgeResponse(t *testing.T) {
	tests := []struct {
		input  string
		engage bool
	}{
		{"YES I find this interesting", true},
		{"yes that's relevant", true},
		{"NO not interesting", false},
		{"no", false},
		{"garbage response", false},
		{"", false},
	}

	for _, tt := range tests {
		result := ParseJudgeResponse(tt.input)
		if result.Engage != tt.engage {
			t.Errorf("ParseJudgeResponse(%q).Engage = %v, want %v", tt.input, result.Engage, tt.engage)
		}
	}
}

func TestSimpleJudge_Yes(t *testing.T) {
	client := &mockLLMClient{response: "YES this is a topic I care about"}
	judge := NewSimpleJudge(client, DefaultPromptConfig())

	msg := timelineMsg("Let's discuss golang concurrency", "user1", "misskey")
	result, err := judge.Judge(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Engage {
		t.Error("should engage on YES response")
	}
}

func TestSimpleJudge_No(t *testing.T) {
	client := &mockLLMClient{response: "NO not relevant to me"}
	judge := NewSimpleJudge(client, DefaultPromptConfig())

	msg := timelineMsg("boring spam content", "user1", "misskey")
	result, err := judge.Judge(context.Background(), msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Engage {
		t.Error("should not engage on NO response")
	}
}

func TestSimpleJudge_Error(t *testing.T) {
	client := &mockLLMClient{err: errors.New("connection refused")}
	judge := NewSimpleJudge(client, DefaultPromptConfig())

	msg := timelineMsg("hello", "user1", "misskey")
	_, err := judge.Judge(context.Background(), msg)
	if err == nil {
		t.Error("should return error on LLM failure")
	}
}

// ============================================================================
// CompositePolicy tests
// ============================================================================

func TestCompositePolicy_ReadOnlyChannel(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey", "telegram"), // RSS 不在白名单
	)

	msg := timelineMsg("interesting post", "user1", "rss")
	decision := policy.Evaluate(context.Background(), msg)

	if decision.Engage {
		t.Error("should not engage on read-only channel (RSS)")
	}
	if decision.Tier != TierCapability {
		t.Errorf("tier = %v, want %v", decision.Tier, TierCapability)
	}
}

func TestCompositePolicy_RuleRejected(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("golang"),
		)),
	)

	msg := timelineMsg("boring content with no keywords", "user1", "misskey")
	decision := policy.Evaluate(context.Background(), msg)

	if decision.Engage {
		t.Error("should not engage when rules reject")
	}
	if decision.Tier != TierRule {
		t.Errorf("tier = %v, want %v", decision.Tier, TierRule)
	}
}

func TestCompositePolicy_AllPass(t *testing.T) {
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("golang"),
			NewLengthRule(1, 1000),
		)),
	)

	msg := timelineMsg("Let's discuss golang patterns", "user1", "misskey")
	decision := policy.Evaluate(context.Background(), msg)

	if !decision.Engage {
		t.Error("should engage when all tiers pass")
	}
	if decision.Tier != TierPass {
		t.Errorf("tier = %v, want %v", decision.Tier, TierPass)
	}
}

func TestCompositePolicy_LLMJudge(t *testing.T) {
	client := &mockLLMClient{response: "YES interesting"}
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("golang"),
		)),
		WithJudge(NewSimpleJudge(client, DefaultPromptConfig())),
	)

	msg := timelineMsg("golang is great", "user1", "misskey")
	decision := policy.Evaluate(context.Background(), msg)

	if !decision.Engage {
		t.Error("should engage when LLM says YES")
	}
	if decision.Tier != TierPass {
		t.Errorf("tier = %v, want %v", decision.Tier, TierPass)
	}
}

func TestCompositePolicy_LLMJudgeDecline(t *testing.T) {
	client := &mockLLMClient{response: "NO boring"}
	policy := NewCompositePolicy(
		NewSourceAllowlist("misskey"),
		WithRules(NewRuleEngine(
			NewKeywordRule("golang"),
		)),
		WithJudge(NewSimpleJudge(client, DefaultPromptConfig())),
	)

	msg := timelineMsg("golang discussion", "user1", "misskey")
	decision := policy.Evaluate(context.Background(), msg)

	if decision.Engage {
		t.Error("should not engage when LLM says NO")
	}
	if decision.Tier != TierLLM {
		t.Errorf("tier = %v, want %v", decision.Tier, TierLLM)
	}
}

func TestCompositePolicy_NilChecker(t *testing.T) {
	policy := NewCompositePolicy(nil)

	msg := timelineMsg("hello", "user1", "misskey")
	decision := policy.Evaluate(context.Background(), msg)

	if decision.Engage {
		t.Error("nil checker should default to DenyAll")
	}
}
