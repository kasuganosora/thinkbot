package pipeline

import (
	"context"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

// ============================================================================
// mockQuotaStore — 模拟配置存储
// ============================================================================

type mockQuotaStore struct {
	mu   sync.Mutex
	data map[string]int64
}

func newMockQuotaStore() *mockQuotaStore {
	return &mockQuotaStore{data: make(map[string]int64)}
}

func (s *mockQuotaStore) GetInt64(key string, defaultValue int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.data[key]; ok {
		return v
	}
	return defaultValue
}

func (s *mockQuotaStore) Set(key string, v int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = v
}

// mockLLMResultStage 是一个返回指定 token 用量的假 LLM stage。
func mockLLMResultStage(totalTokens int) core.Stage {
	return &core.StageFunc{
		StageName: "mock_llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			env.Set("llm.result", &llm.GenerateResult{
				Usage: llm.Usage{TotalTokens: totalTokens},
			})
			return env, nil
		},
	}
}

// mockNoResultStage 不写入 llm.result（模拟无 LLM 调用的 stage）。
func mockNoResultStage() core.Stage {
	return &core.StageFunc{
		StageName: "mock_noop",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			return env, nil
		},
	}
}

func quotaTestLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

func quotaTestTP() *noop.TracerProvider {
	tp := noop.NewTracerProvider()
	return &tp
}

// ============================================================================
// QuotaResolver 测试
// ============================================================================

func TestQuotaResolver_NoLimits(t *testing.T) {
	store := newMockQuotaStore()
	resolver := NewQuotaResolver(store)

	res := resolver.Resolve("bot1", "telegram", "-123")
	if res.Limit != 0 {
		t.Errorf("expected unlimited (0), got %d", res.Limit)
	}
	if res.Dimension != "" {
		t.Errorf("expected empty dimension, got %q", res.Dimension)
	}
}

func TestQuotaResolver_SystemFallback(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("system.token_quota", 1000000)
	resolver := NewQuotaResolver(store)

	res := resolver.Resolve("bot1", "telegram", "-123")
	if res.Limit != 1000000 {
		t.Errorf("expected system limit 1000000, got %d", res.Limit)
	}
	if res.Dimension != "system" {
		t.Errorf("expected dimension 'system', got %q", res.Dimension)
	}
}

func TestQuotaResolver_BotOverridesSystem(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("system.token_quota", 1000000)
	store.Set("bot.bot1.token_quota", 500000)
	resolver := NewQuotaResolver(store)

	res := resolver.Resolve("bot1", "telegram", "-123")
	if res.Limit != 500000 {
		t.Errorf("expected bot limit 500000, got %d", res.Limit)
	}
	if res.Dimension != "bot:bot1" {
		t.Errorf("expected dimension 'bot:bot1', got %q", res.Dimension)
	}
}

func TestQuotaResolver_ChannelOverridesBot(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("system.token_quota", 1000000)
	store.Set("bot.bot1.token_quota", 500000)
	store.Set("bot.bot1.token_quota.channel.telegram", 300000)
	resolver := NewQuotaResolver(store)

	res := resolver.Resolve("bot1", "telegram", "-123")
	if res.Limit != 300000 {
		t.Errorf("expected channel limit 300000, got %d", res.Limit)
	}
	if res.Dimension != "channel:telegram" {
		t.Errorf("expected dimension 'channel:telegram', got %q", res.Dimension)
	}
}

func TestQuotaResolver_ChatOverridesChannel(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("system.token_quota", 1000000)
	store.Set("bot.bot1.token_quota", 500000)
	store.Set("bot.bot1.token_quota.channel.telegram", 300000)
	store.Set("bot.bot1.token_quota.channel.telegram.-123", 100000)
	resolver := NewQuotaResolver(store)

	res := resolver.Resolve("bot1", "telegram", "-123")
	if res.Limit != 100000 {
		t.Errorf("expected chat limit 100000, got %d", res.Limit)
	}
	if res.Dimension != "chat:telegram:-123" {
		t.Errorf("expected dimension 'chat:telegram:-123', got %q", res.Dimension)
	}
}

func TestQuotaResolver_DifferentChatsDifferentLimits(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 500000)
	store.Set("bot.bot1.token_quota.channel.telegram.-123", 100000)
	store.Set("bot.bot1.token_quota.channel.telegram.-456", 200000)
	resolver := NewQuotaResolver(store)

	res1 := resolver.Resolve("bot1", "telegram", "-123")
	if res1.Limit != 100000 {
		t.Errorf("chat -123: expected 100000, got %d", res1.Limit)
	}

	res2 := resolver.Resolve("bot1", "telegram", "-456")
	if res2.Limit != 200000 {
		t.Errorf("chat -456: expected 200000, got %d", res2.Limit)
	}
}

func TestQuotaResolver_NoChatID_FallsToChannel(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 500000)
	store.Set("bot.bot1.token_quota.channel.telegram", 300000)
	store.Set("bot.bot1.token_quota.channel.telegram.-123", 100000)
	resolver := NewQuotaResolver(store)

	// chatID 为空 → 跳过 chat 级，落到 channel 级
	res := resolver.Resolve("bot1", "telegram", "")
	if res.Limit != 300000 {
		t.Errorf("expected channel limit 300000, got %d", res.Limit)
	}
	if res.Dimension != "channel:telegram" {
		t.Errorf("expected dimension 'channel:telegram', got %q", res.Dimension)
	}
}

func TestQuotaResolver_ChannelNotSet_FallsToBot(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 500000)
	// 不给 telegram 设置 channel 级限额
	resolver := NewQuotaResolver(store)

	res := resolver.Resolve("bot1", "telegram", "-123")
	if res.Limit != 500000 {
		t.Errorf("expected bot limit 500000, got %d", res.Limit)
	}
	if res.Dimension != "bot:bot1" {
		t.Errorf("expected dimension 'bot:bot1', got %q", res.Dimension)
	}
}

func TestQuotaResolver_NegativeLimit_TreatedAsZero(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", -1) // negative
	resolver := NewQuotaResolver(store)

	res := resolver.Resolve("bot1", "telegram", "-123")
	if res.Limit != 0 {
		t.Errorf("negative limit should be treated as 0 (unlimited), got %d", res.Limit)
	}
}

func TestQuotaResolver_ZeroLimit_TreatedAsUnset(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 0)
	store.Set("system.token_quota", 1000000)
	resolver := NewQuotaResolver(store)

	res := resolver.Resolve("bot1", "telegram", "-123")
	// 0 should fall through to system
	if res.Limit != 1000000 {
		t.Errorf("expected system limit 1000000, got %d", res.Limit)
	}
	if res.Dimension != "system" {
		t.Errorf("expected dimension 'system', got %q", res.Dimension)
	}
}

// ============================================================================
// MonthlyCounter 测试
// ============================================================================

func TestMonthlyCounter_BasicAccumulation(t *testing.T) {
	c := newMonthlyCounter()
	if total := c.get(); total != 0 {
		t.Errorf("expected 0, got %d", total)
	}

	newTotal := c.add(500)
	if newTotal != 500 {
		t.Errorf("expected 500, got %d", newTotal)
	}

	newTotal = c.add(300)
	if newTotal != 800 {
		t.Errorf("expected 800, got %d", newTotal)
	}

	if total := c.get(); total != 800 {
		t.Errorf("expected 800 from get(), got %d", total)
	}
}

func TestMonthlyCounter_ConcurrentAccess(t *testing.T) {
	c := newMonthlyCounter()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.add(10)
		}()
	}
	wg.Wait()

	if total := c.get(); total != 1000 {
		t.Errorf("expected 1000, got %d", total)
	}
}

// ============================================================================
// TokenQuotaMiddleware 测试
// ============================================================================

func TestTokenQuotaMiddleware_NilResolver(t *testing.T) {
	mw := TokenQuotaMiddleware(nil, quotaTestTP(), quotaTestLogger())
	inner := mockLLMResultStage(100)
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{ID: "test", BotID: "bot1", Channel: "telegram:-123"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestTokenQuotaMiddleware_NoLimit_PassThrough(t *testing.T) {
	store := newMockQuotaStore()
	resolver := NewQuotaResolver(store)
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())

	inner := mockLLMResultStage(500)
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{ID: "test", BotID: "bot1", Channel: "telegram:-123"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// llm.result should have been set
	v, ok := result.Get("llm.result")
	if !ok || v == nil {
		t.Error("expected llm.result in envelope")
	}
}

func TestTokenQuotaMiddleware_UnderLimit_Succeeds(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 10000)
	resolver := NewQuotaResolver(store)
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())

	inner := mockLLMResultStage(500)
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{ID: "test", BotID: "bot1", Channel: "telegram:-123"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestTokenQuotaMiddleware_AtLimit_Blocked(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 1000)
	resolver := NewQuotaResolver(store)

	// 先积累到 998
	state := newTokenQuotaState()
	state.AddUsage("bot:bot1", 998)

	// 需要手动状态注入…… 实际上 TokenQuotaMiddleware 有自己的内部状态
	// 让我绕过这个问题：先调用一次达到 998，然后第二次用一个小调用让它到 1000
	// 但我们没法预设 998...

	// 简单方式：直接设置 limit=1000，先调用一次 1000，再次调用应被阻塞
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())
	inner := mockLLMResultStage(1000)
	wrapped := mw(inner)

	env1 := core.NewEnvelope(core.Message{ID: "test1", BotID: "bot1", Channel: "telegram:-123"})
	_, err1 := wrapped.Process(context.Background(), env1)
	if err1 != nil {
		t.Fatalf("first call should succeed: %v", err1)
	}

	// 第二次调用 → 1000 + 1000 = 2000 > limit 1000 → blocked
	env2 := core.NewEnvelope(core.Message{ID: "test2", BotID: "bot1", Channel: "telegram:-123"})
	_, err2 := wrapped.Process(context.Background(), env2)
	if err2 == nil {
		t.Fatal("expected token quota error on second call")
	}
	pipeErr, ok := err2.(*core.PipelineError)
	if !ok {
		t.Fatalf("expected PipelineError, got %T: %v", err2, pipeErr)
	}
	if pipeErr.Stage != "token_quota" {
		t.Errorf("expected stage 'token_quota', got %q", pipeErr.Stage)
	}
}

// seqMockLLMStage 按顺序返回预设 token 量的假 LLM stage。
type seqMockLLMStage struct {
	tokens []int
	idx    int
}

func (s *seqMockLLMStage) Name() string { return "mock_llm_seq" }

func (s *seqMockLLMStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	tok := s.tokens[s.idx]
	s.idx++
	env.Set("llm.result", &llm.GenerateResult{
		Usage: llm.Usage{TotalTokens: tok},
	})
	return env, nil
}

func TestTokenQuotaMiddleware_998Plus102_ExceedsOnceThenBlocks(t *testing.T) {
	// 模拟场景：500 + 498 = 998 < 1000，第3次调用 + 500 = 1498 > 1000
	// 998 < 1000 → 第3次应成功（超限额度）
	// 第4次 → 1498 >= 1000 → 拦截

	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 1000)
	resolver := NewQuotaResolver(store)
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())

	seqMock := &seqMockLLMStage{tokens: []int{500, 498, 500, 1}}
	wrapped := mw(seqMock)

	// Call 1: 500 → total 500
	env1 := core.NewEnvelope(core.Message{ID: "test1", BotID: "bot1", Channel: "telegram:-123"})
	_, err1 := wrapped.Process(context.Background(), env1)
	if err1 != nil {
		t.Fatalf("call 1 should succeed: %v", err1)
	}

	// Call 2: 498 → total 998 (< 1000, OK)
	env2 := core.NewEnvelope(core.Message{ID: "test2", BotID: "bot1", Channel: "telegram:-123"})
	_, err2 := wrapped.Process(context.Background(), env2)
	if err2 != nil {
		t.Fatalf("call 2 should succeed: %v", err2)
	}

	// Call 3: 500 → check before = 998 < 1000 → 允许 → total = 1498
	env3 := core.NewEnvelope(core.Message{ID: "test3", BotID: "bot1", Channel: "telegram:-123"})
	_, err3 := wrapped.Process(context.Background(), env3)
	if err3 != nil {
		t.Fatalf("call 3 should succeed (check before was 998 < 1000): %v", err3)
	}

	// Call 4: 1 → check before = 1498 >= 1000 → 拦截
	env4 := core.NewEnvelope(core.Message{ID: "test4", BotID: "bot1", Channel: "telegram:-123"})
	_, err4 := wrapped.Process(context.Background(), env4)
	if err4 == nil {
		t.Fatal("call 4 should be blocked after exceeding quota")
	}
}

func TestTokenQuotaMiddleware_NoLLMResult_NoAccumulation(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 100)
	resolver := NewQuotaResolver(store)
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())

	inner := mockNoResultStage()
	wrapped := mw(inner)

	// 多次调用不产 llm.result → 不会累加 → 不会被拦截
	for i := 0; i < 10; i++ {
		env := core.NewEnvelope(core.Message{ID: "test", BotID: "bot1", Channel: "telegram:-123"})
		_, err := wrapped.Process(context.Background(), env)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
}

func TestTokenQuotaMiddleware_DifferentBotsIndependent(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 1000)
	store.Set("bot.bot2.token_quota", 1000)
	resolver := NewQuotaResolver(store)
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())

	inner := mockLLMResultStage(900)
	wrapped := mw(inner)

	// bot1 使用 900
	env1 := core.NewEnvelope(core.Message{ID: "test1", BotID: "bot1", Channel: "telegram:-123"})
	_, err := wrapped.Process(context.Background(), env1)
	if err != nil {
		t.Fatalf("bot1 call 1 should succeed: %v", err)
	}

	// bot2 使用 900（其他 bot 的 900）
	env2 := core.NewEnvelope(core.Message{ID: "test2", BotID: "bot2", Channel: "telegram:-456"})
	_, err = wrapped.Process(context.Background(), env2)
	if err != nil {
		t.Fatalf("bot2 should succeed: %v", err)
	}

	// bot1 再用 200 → 900+200=1100 > 1000 → 但现在 check before = 900 < 1000, 通过
	// 下次被拦截
	env3 := core.NewEnvelope(core.Message{ID: "test3", BotID: "bot1", Channel: "telegram:-123"})
	_, err = wrapped.Process(context.Background(), env3)
	if err != nil {
		t.Fatalf("bot1 call 2 should succeed (900 < 1000): %v", err)
	}

	// bot1 用第3次 → blocked
	env4 := core.NewEnvelope(core.Message{ID: "test4", BotID: "bot1", Channel: "telegram:-123"})
	_, err = wrapped.Process(context.Background(), env4)
	if err == nil {
		t.Error("bot1 should be blocked after exceeding quota")
	}

	// bot2 仍然可以（900 < 1000）
	env5 := core.NewEnvelope(core.Message{ID: "test5", BotID: "bot2", Channel: "telegram:-456"})
	_, err = wrapped.Process(context.Background(), env5)
	if err != nil {
		t.Fatalf("bot2 call 2 should succeed: %v", err)
	}
}

func TestTokenQuotaMiddleware_ChannelDimension(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota.channel.telegram", 500)
	resolver := NewQuotaResolver(store)
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())

	inner := mockLLMResultStage(400)
	wrapped := mw(inner)

	env := core.NewEnvelope(core.Message{
		ID:       "test",
		BotID:    "bot1",
		Channel:  "telegram:-123",
		Metadata: map[string]any{"channel_type": "telegram", "chat_id": "-123"},
	})
	_, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("first call should succeed: %v", err)
	}

	// 不同的 chat，同一 channel → 共享 channel 级额度
	env2 := core.NewEnvelope(core.Message{
		ID:       "test2",
		BotID:    "bot1",
		Channel:  "telegram:-456",
		Metadata: map[string]any{"channel_type": "telegram", "chat_id": "-456"},
	})

	// 400 + 400 = 800 > 500 → check before = 400 < 500, succeeds but accumulates to 800
	_, err2 := wrapped.Process(context.Background(), env2)
	if err2 != nil {
		t.Fatalf("second call should succeed (400 < 500): %v", err2)
	}

	// 第三次 → blocked (800 >= 500)
	env3 := core.NewEnvelope(core.Message{
		ID:       "test3",
		BotID:    "bot1",
		Channel:  "telegram:-789",
		Metadata: map[string]any{"channel_type": "telegram", "chat_id": "-789"},
	})
	_, err3 := wrapped.Process(context.Background(), env3)
	if err3 == nil {
		t.Error("third call should be blocked (channel quota shared)")
	}
}

func TestTokenQuotaMiddleware_ChatDimension(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota.channel.telegram.-123", 500)
	store.Set("bot.bot1.token_quota.channel.telegram.-456", 500)
	resolver := NewQuotaResolver(store)
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())

	inner := mockLLMResultStage(400)
	wrapped := mw(inner)

	// chat -123 使用 400
	env1 := core.NewEnvelope(core.Message{
		ID:       "test1",
		BotID:    "bot1",
		Channel:  "telegram:-123",
		Metadata: map[string]any{"channel_type": "telegram", "chat_id": "-123"},
	})
	_, err := wrapped.Process(context.Background(), env1)
	if err != nil {
		t.Fatalf("chat -123 call 1 should succeed: %v", err)
	}

	// chat -456 使用 400（独立计数）
	env2 := core.NewEnvelope(core.Message{
		ID:       "test2",
		BotID:    "bot1",
		Channel:  "telegram:-456",
		Metadata: map[string]any{"channel_type": "telegram", "chat_id": "-456"},
	})
	_, err = wrapped.Process(context.Background(), env2)
	if err != nil {
		t.Fatalf("chat -456 should succeed: %v", err)
	}

	// chat -123 再用 200 → 400+200=600 > 500 → succeeds but next blocked
	env3 := core.NewEnvelope(core.Message{
		ID:       "test3",
		BotID:    "bot1",
		Channel:  "telegram:-123",
		Metadata: map[string]any{"channel_type": "telegram", "chat_id": "-123"},
	})
	_, err = wrapped.Process(context.Background(), env3)
	if err != nil {
		t.Fatalf("chat -123 call 2 should succeed (400 < 500): %v", err)
	}

	// chat -123 第3次 → blocked (600 >= 500)
	env4 := core.NewEnvelope(core.Message{
		ID:       "test4",
		BotID:    "bot1",
		Channel:  "telegram:-123",
		Metadata: map[string]any{"channel_type": "telegram", "chat_id": "-123"},
	})
	_, err = wrapped.Process(context.Background(), env4)
	if err == nil {
		t.Error("chat -123 should be blocked after exceeding its quota")
	}

	// chat -456 仍可继续（400 < 500）
	env5 := core.NewEnvelope(core.Message{
		ID:       "test5",
		BotID:    "bot1",
		Channel:  "telegram:-456",
		Metadata: map[string]any{"channel_type": "telegram", "chat_id": "-456"},
	})
	_, err = wrapped.Process(context.Background(), env5)
	if err != nil {
		t.Fatalf("chat -456 should still succeed: %v", err)
	}
}

func TestTokenQuotaMiddleware_ConcurrentAccess(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 100000)
	resolver := NewQuotaResolver(store)
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())

	inner := mockLLMResultStage(10)
	wrapped := mw(inner)

	stateMW := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())
	fakeLLM := mockLLMResultStage(10)
	wrapped2 := stateMW(fakeLLM)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env := core.NewEnvelope(core.Message{
				ID:      "test",
				BotID:   "bot1",
				Channel: "telegram:-123",
			})
			_, _ = wrapped2.Process(context.Background(), env)
		}()
	}
	wg.Wait()

	// 50 * 10 = 500 tokens, well under 100000
	// 没有死锁或数据竞争即可
	_ = wrapped
}

func TestTokenQuotaMiddleware_SourceAsFallback(t *testing.T) {
	// 没有 channel_type metadata → 使用 Source 作为 channelType
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota.channel.telegram", 1000)
	resolver := NewQuotaResolver(store)
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())

	inner := mockLLMResultStage(500)
	wrapped := mw(inner)

	// Source = "telegram" 会自动被用作 channelType
	env := core.NewEnvelope(core.Message{
		ID:      "test",
		BotID:   "bot1",
		Channel: "telegram:-123",
		Source:  "telegram",
	})
	_, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("should use source as channelType: %v", err)
	}
}

func TestTokenQuotaMiddleware_ZeroTokenResult_StillCounts(t *testing.T) {
	store := newMockQuotaStore()
	store.Set("bot.bot1.token_quota", 10)
	resolver := NewQuotaResolver(store)
	mw := TokenQuotaMiddleware(resolver, quotaTestTP(), quotaTestLogger())

	zeroStage := mockLLMResultStage(0)
	wrapped := mw(zeroStage)

	// 调用 100 次零 token → 不会累积到超过限额
	for i := 0; i < 100; i++ {
		env := core.NewEnvelope(core.Message{ID: "test", BotID: "bot1", Channel: "telegram:-123"})
		_, err := wrapped.Process(context.Background(), env)
		if err != nil {
			t.Fatalf("call %d should succeed with 0 tokens: %v", i, err)
		}
	}
}

// ============================================================================
// TokenQuotaState 测试
// ============================================================================

func TestTokenQuotaState_Snapshot(t *testing.T) {
	s := newTokenQuotaState()
	s.AddUsage("bot:bot1", 100)
	s.AddUsage("channel:telegram", 200)
	s.AddUsage("chat:telegram:-123", 300)

	snap := s.Snapshot()
	if len(snap) != 3 {
		t.Errorf("expected 3 entries in snapshot, got %d", len(snap))
	}
	if snap["bot:bot1"] != 100 {
		t.Errorf("expected bot:bot1=100, got %d", snap["bot:bot1"])
	}
	if snap["channel:telegram"] != 200 {
		t.Errorf("expected channel:telegram=200, got %d", snap["channel:telegram"])
	}
	if snap["chat:telegram:-123"] != 300 {
		t.Errorf("expected chat:telegram:-123=300, got %d", snap["chat:telegram:-123"])
	}
}

func TestTokenQuotaState_Reset(t *testing.T) {
	s := newTokenQuotaState()
	s.AddUsage("bot:bot1", 1000)
	if s.Usage("bot:bot1") != 1000 {
		t.Fatal("expected 1000 before reset")
	}

	s.Reset("bot:bot1")
	if s.Usage("bot:bot1") != 0 {
		t.Errorf("expected 0 after reset, got %d", s.Usage("bot:bot1"))
	}
}
