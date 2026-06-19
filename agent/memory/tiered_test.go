package memory

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// ============================================================================
// TieredStore 基础操作
// ============================================================================

func TestTieredStore_AppendAndRetrieve(t *testing.T) {
	store := NewTieredStore(nil)
	ctx := context.Background()

	store.Append(ctx, TieredEntry{
		Entry: Entry{Scope: ChannelScope("ch1"), Content: "hello"},
		Tier:  Tier0Working,
	})
	store.Append(ctx, TieredEntry{
		Entry: Entry{Scope: ChannelScope("ch1"), Content: "world"},
		Tier:  Tier0Working,
	})

	results, err := store.Retrieve(ctx, Tier0Working, []Scope{ChannelScope("ch1")}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(results))
	}
}

func TestTieredStore_TierIsolation(t *testing.T) {
	store := NewTieredStore(nil)
	ctx := context.Background()
	scope := ChannelScope("ch1")

	store.Append(ctx, TieredEntry{Entry: Entry{Scope: scope, Content: "L0"}, Tier: Tier0Working})
	store.Append(ctx, TieredEntry{Entry: Entry{Scope: scope, Content: "L1"}, Tier: Tier1LongTerm})

	l0, _ := store.Retrieve(ctx, Tier0Working, []Scope{scope}, 10)
	l1, _ := store.Retrieve(ctx, Tier1LongTerm, []Scope{scope}, 10)

	if len(l0) != 1 {
		t.Errorf("L0 should have 1 entry, got %d", len(l0))
	}
	if len(l1) != 1 {
		t.Errorf("L1 should have 1 entry, got %d", len(l1))
	}
	if l0[0].Content != "L0" {
		t.Errorf("L0 content mismatch: %q", l0[0].Content)
	}
	if l1[0].Content != "L1" {
		t.Errorf("L1 content mismatch: %q", l1[0].Content)
	}
}

func TestTieredStore_TTLExpiry(t *testing.T) {
	store := NewTieredStore(nil)
	ctx := context.Background()

	// 手动设置已过期的条目
	store.Append(ctx, TieredEntry{
		Entry: Entry{Scope: GlobalScope(), Content: "expired"},
		Tier:  Tier0Working,
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	})

	results, _ := store.Retrieve(ctx, Tier0Working, []Scope{GlobalScope()}, 10)
	if len(results) != 0 {
		t.Errorf("expired entry should not be retrieved, got %d", len(results))
	}

	// GC 应清理过期条目
	removed := store.GC(ctx)
	if removed != 1 {
		t.Errorf("GC should remove 1 entry, got %d", removed)
	}
}

func TestTieredStore_MaxEntriesEviction(t *testing.T) {
	configs := map[MemoryTier]TierConfig{
		Tier0Working: {MaxEntries: 3},
	}
	store := NewTieredStore(configs)
	ctx := context.Background()
	scope := ChannelScope("evict")

	for i := 0; i < 5; i++ {
		store.Append(ctx, TieredEntry{
			Entry: Entry{Scope: scope, Content: "item"},
			Tier:  Tier0Working,
		})
	}

	count, _ := store.Count(ctx, Tier0Working, scope)
	if count != 3 {
		t.Errorf("expected 3 entries after eviction, got %d", count)
	}
}

func TestTieredStore_Count(t *testing.T) {
	store := NewTieredStore(nil)
	ctx := context.Background()
	scope := ChannelScope("count-test")

	for i := 0; i < 5; i++ {
		store.Append(ctx, TieredEntry{
			Entry: Entry{Scope: scope, Content: "x"},
			Tier:  Tier1LongTerm,
		})
	}

	count, _ := store.Count(ctx, Tier1LongTerm, scope)
	if count != 5 {
		t.Errorf("expected 5, got %d", count)
	}
}

func TestTieredStore_Delete(t *testing.T) {
	store := NewTieredStore(nil)
	ctx := context.Background()
	scope := ChannelScope("del-test")

	store.Append(ctx, TieredEntry{
		Entry: Entry{ID: "test-1", Scope: scope, Content: "x"},
		Tier:  Tier1LongTerm,
	})

	store.Delete(ctx, Tier1LongTerm, scope, "test-1")

	count, _ := store.Count(ctx, Tier1LongTerm, scope)
	if count != 0 {
		t.Errorf("expected 0 after delete, got %d", count)
	}
}

func TestTieredStore_ClearTier(t *testing.T) {
	store := NewTieredStore(nil)
	ctx := context.Background()
	scope := ChannelScope("clear-test")

	store.Append(ctx, TieredEntry{Entry: Entry{Scope: scope, Content: "a"}, Tier: Tier0Working})
	store.Append(ctx, TieredEntry{Entry: Entry{Scope: scope, Content: "b"}, Tier: Tier1LongTerm})

	store.ClearTier(ctx, Tier0Working)

	l0Count, _ := store.Count(ctx, Tier0Working, scope)
	l1Count, _ := store.Count(ctx, Tier1LongTerm, scope)

	if l0Count != 0 {
		t.Errorf("L0 should be cleared, got %d", l0Count)
	}
	if l1Count != 1 {
		t.Errorf("L1 should be untouched, got %d", l1Count)
	}
}

func TestTieredStore_GetUnprocessed(t *testing.T) {
	store := NewTieredStore(nil)
	ctx := context.Background()
	scope := ChannelScope("unproc-test")

	store.Append(ctx, TieredEntry{Entry: Entry{ID: "a", Scope: scope, Content: "x"}, Tier: Tier0Working})
	store.Append(ctx, TieredEntry{Entry: Entry{ID: "b", Scope: scope, Content: "y"}, Tier: Tier0Working})

	// Mark "a" as processed
	store.MarkProcessed(ctx, scope, []string{"a"})

	unprocessed, _ := store.GetUnprocessed(ctx, scope, 10)
	if len(unprocessed) != 1 {
		t.Fatalf("expected 1 unprocessed, got %d", len(unprocessed))
	}
	if unprocessed[0].ID != "b" {
		t.Errorf("expected 'b', got %q", unprocessed[0].ID)
	}
}

func TestTieredStore_RetrieveTimeDescOrder(t *testing.T) {
	store := NewTieredStore(nil)
	ctx := context.Background()
	scope := ChannelScope("order-test")

	entries := []string{"first", "second", "third"}
	for _, content := range entries {
		store.Append(ctx, TieredEntry{
			Entry: Entry{Scope: scope, Content: content},
			Tier:  Tier0Working,
		})
		time.Sleep(1 * time.Millisecond)
	}

	results, _ := store.Retrieve(ctx, Tier0Working, []Scope{scope}, 10)
	if len(results) != 3 {
		t.Fatalf("expected 3, got %d", len(results))
	}
	// 最新的在前
	if results[0].Content != "third" {
		t.Errorf("expected 'third' first, got %q", results[0].Content)
	}
	if results[2].Content != "first" {
		t.Errorf("expected 'first' last, got %q", results[2].Content)
	}
}

// ============================================================================
// RuleConsolidator
// ============================================================================

func TestRuleConsolidator_ShortContent(t *testing.T) {
	c := NewRuleConsolidator()
	l0 := []TieredEntry{
		{Entry: Entry{ID: "1", Content: "hi"}, Tier: Tier0Working},
	}
	results, err := c.Consolidate(context.Background(), l0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Decision != DecisionSkip {
		t.Errorf("expected SKIP for short content, got %s", results[0].Decision)
	}
}

func TestRuleConsolidator_SkipKeyword(t *testing.T) {
	c := NewRuleConsolidator()
	l0 := []TieredEntry{
		{Entry: Entry{ID: "1", Content: "你好你好你好你好你好"}, Tier: Tier0Working},
	}
	results, _ := c.Consolidate(context.Background(), l0, nil)
	if len(results) != 1 || results[0].Decision != DecisionSkip {
		t.Errorf("expected SKIP for greeting, got %+v", results)
	}
}

func TestRuleConsolidator_AddValuable(t *testing.T) {
	c := NewRuleConsolidator()
	l0 := []TieredEntry{
		{Entry: Entry{ID: "1", Content: "用户使用 Go 语言进行后端开发，偏好 Gin 框架"}, Tier: Tier0Working},
	}
	results, _ := c.Consolidate(context.Background(), l0, nil)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Decision != DecisionAdd {
		t.Errorf("expected ADD, got %s", results[0].Decision)
	}
	if results[0].Category != "observation" {
		t.Errorf("expected category 'observation', got %q", results[0].Category)
	}
	if results[0].Importance != 0.5 {
		t.Errorf("expected importance 0.5, got %f", results[0].Importance)
	}
}

func TestRuleConsolidator_StripsThinkTags(t *testing.T) {
	c := NewRuleConsolidator()
	l0 := []TieredEntry{
		{Entry: Entry{ID: "1", Content: "<think>reasoning</think>用户喜欢用 Python 做数据分析"}, Tier: Tier0Working},
	}
	results, _ := c.Consolidate(context.Background(), l0, nil)
	if results[0].Content != "用户喜欢用 Python 做数据分析" {
		t.Errorf("think tags not stripped: %q", results[0].Content)
	}
}

// ============================================================================
// TieredManager
// ============================================================================

func TestTieredManager_WriteAndConsolidate(t *testing.T) {
	store := NewTieredStore(nil)
	mgr := NewTieredManager(TieredManagerConfig{
		Store:         store,
		Consolidator:  NewRuleConsolidator(),
	}, testTracerProvider(), testLogger())

	ctx := context.Background()
	scope := ChannelScope("mgr-test")

	// 写入工作记忆
	mgr.WriteWorking(ctx, scope, "用户使用 Go 语言进行后端开发", "test")
	mgr.WriteWorking(ctx, scope, "你好", "test") // 应被 SKIP

	// 巩固
	promoted, err := mgr.Consolidate(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if promoted != 1 {
		t.Errorf("expected 1 promoted, got %d", promoted)
	}

	// L1 应有 1 条
	l1, _ := store.Retrieve(ctx, Tier1LongTerm, []Scope{scope}, 10)
	if len(l1) != 1 {
		t.Errorf("expected 1 L1 entry, got %d", len(l1))
	}

	// 未处理的 L0 应为 0
	unprocessed, _ := store.GetUnprocessed(ctx, scope, 10)
	if len(unprocessed) != 0 {
		t.Errorf("expected 0 unprocessed after consolidation, got %d", len(unprocessed))
	}
}

func TestTieredManager_RetrieveMerged(t *testing.T) {
	store := NewTieredStore(nil)
	mgr := NewTieredManager(TieredManagerConfig{
		Store: store,
	}, testTracerProvider(), testLogger())

	ctx := context.Background()
	scope := ChannelScope("merge-test")

	// 写入各层级
	mgr.WriteWorking(ctx, scope, "L0 content", "test")
	mgr.WriteLongTerm(ctx, Entry{Scope: scope, Content: "L1 content", Category: "fact"}, Tier0Working)
	mgr.WriteEpisodic(ctx, Entry{Scope: scope, Content: "L2 scene"}, []string{"mem-1"})
	mgr.WriteProfile(ctx, Entry{Scope: scope, Content: "L3 trait"})

	results, err := mgr.RetrieveMerged(ctx, []Scope{scope}, 20)
	if err != nil {
		t.Fatal(err)
	}

	// 应包含所有 4 层
	if len(results) != 4 {
		t.Fatalf("expected 4 merged entries, got %d", len(results))
	}

	// 验证层级顺序: L3 → L2 → L1 → L0
	expectedTiers := []MemoryTier{Tier3Profile, Tier2Episodic, Tier1LongTerm, Tier0Working}
	for i, expected := range expectedTiers {
		if results[i].Tier != expected {
			t.Errorf("position %d: expected tier %s, got %s", i, expected, results[i].Tier)
		}
	}
}

func TestTieredManager_RunGC(t *testing.T) {
	store := NewTieredStore(nil)
	mgr := NewTieredManager(TieredManagerConfig{
		Store: store,
	}, testTracerProvider(), testLogger())

	ctx := context.Background()
	scope := ChannelScope("gc-test")

	// 写入 L0 + 手动过期
	mgr.WriteWorking(ctx, scope, "valid", "test")
	store.Append(ctx, TieredEntry{
		Entry: Entry{Scope: scope, Content: "expired"},
		Tier:  Tier0Working,
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	})

	removed := mgr.RunGC(ctx)
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}

	// 验证有效条目还在
	l0, _ := store.Retrieve(ctx, Tier0Working, []Scope{scope}, 10)
	if len(l0) != 1 {
		t.Errorf("expected 1 remaining, got %d", len(l0))
	}
	if l0[0].Content != "valid" {
		t.Errorf("wrong entry survived GC: %q", l0[0].Content)
	}
}

func TestTieredManager_Snapshot(t *testing.T) {
	store := NewTieredStore(nil)
	ctx := context.Background()
	scope := ChannelScope("snap-test")

	store.Append(ctx, TieredEntry{Entry: Entry{Scope: scope, Content: "a"}, Tier: Tier0Working})
	store.Append(ctx, TieredEntry{Entry: Entry{Scope: scope, Content: "b"}, Tier: Tier1LongTerm})

	snap := store.Snapshot()

	l0Count := len(snap[Tier0Working])
	l1Count := len(snap[Tier1LongTerm])

	if l0Count != 1 {
		t.Errorf("snapshot L0 scopes: expected 1, got %d", l0Count)
	}
	if l1Count != 1 {
		t.Errorf("snapshot L1 scopes: expected 1, got %d", l1Count)
	}
}

// ============================================================================
// LLMConsolidator JSON Parsing (critical regression tests)
// ============================================================================

func TestLLMConsolidator_parseResult_MultiLineJSON(t *testing.T) {
	// Simulate LLM output with multi-line JSON objects (the common case)
	// This was completely broken with the old line-based parser.
	fence := "```"
	llmOutput := "Here are the decisions:\n\n" + fence + "json\n" +
		`[
  {
    "source_id": "mem-aaa",
    "decision": "ADD",
    "category": "fact",
    "content": "用户使用 Go 语言进行后端开发",
    "importance": 0.8,
    "reason": "技术栈信息"
  },
  {
    "source_id": "mem-bbb",
    "decision": "SKIP",
    "reason": "闲聊内容"
  }
]
` + fence

	c := NewLLMConsolidator(DefaultLLMConsolidatorConfig(), otel.GetTracerProvider(), zap.NewNop().Sugar())
	results := c.parseResult(llmOutput)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Decision != DecisionAdd {
		t.Errorf("result[0]: expected ADD, got %s", results[0].Decision)
	}
	if results[0].Content != "用户使用 Go 语言进行后端开发" {
		t.Errorf("result[0]: content mismatch: %q", results[0].Content)
	}
	if results[0].Importance != 0.8 {
		t.Errorf("result[0]: expected importance 0.8, got %f", results[0].Importance)
	}
	if results[1].Decision != DecisionSkip {
		t.Errorf("result[1]: expected SKIP, got %s", results[1].Decision)
	}
}

func TestLLMConsolidator_parseResult_CommaInContent(t *testing.T) {
	// Content with commas was truncated by the old extractJSONString parser
	llmOutput := `[{"source_id":"mem-1","decision":"ADD","category":"fact","content":"hello, world, foo","importance":0.5}]`

	c := NewLLMConsolidator(DefaultLLMConsolidatorConfig(), otel.GetTracerProvider(), zap.NewNop().Sugar())
	results := c.parseResult(llmOutput)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Content != "hello, world, foo" {
		t.Errorf("content with commas was truncated: got %q", results[0].Content)
	}
}

func TestLLMConsolidator_parseResult_EmptyInput(t *testing.T) {
	c := NewLLMConsolidator(DefaultLLMConsolidatorConfig(), otel.GetTracerProvider(), zap.NewNop().Sugar())
	results := c.parseResult("no json here")
	if results != nil {
		t.Errorf("expected nil for non-JSON input, got %v", results)
	}
}

func TestLLMConsolidator_parseResult_UpdateWithTargetID(t *testing.T) {
	llmOutput := `[{"source_id":"mem-1","decision":"UPDATE","target_id":"mem-old","content":"updated content","importance":0.9}]`
	c := NewLLMConsolidator(DefaultLLMConsolidatorConfig(), otel.GetTracerProvider(), zap.NewNop().Sugar())
	results := c.parseResult(llmOutput)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Decision != DecisionUpdate {
		t.Errorf("expected UPDATE, got %s", results[0].Decision)
	}
	if results[0].TargetID != "mem-old" {
		t.Errorf("expected target_id 'mem-old', got %q", results[0].TargetID)
	}
}

// ============================================================================
// TieredManager Aggregate / ExtractProfile error handling
// ============================================================================

func TestTieredManager_Aggregate_NoAggregator(t *testing.T) {
	store := NewTieredStore(nil)
	mgr := NewTieredManager(TieredManagerConfig{Store: store}, testTracerProvider(), testLogger())

	_, err := mgr.Aggregate(context.Background(), ChannelScope("test"))
	if err == nil {
		t.Error("expected error when no Aggregator configured")
	}
}

func TestTieredManager_ExtractProfile_NoProfiler(t *testing.T) {
	store := NewTieredStore(nil)
	mgr := NewTieredManager(TieredManagerConfig{Store: store}, testTracerProvider(), testLogger())

	_, err := mgr.ExtractProfile(context.Background(), ChannelScope("test"))
	if err == nil {
		t.Error("expected error when no Profiler configured")
	}
}

// ============================================================================
// Helpers
// ============================================================================

func testTracerProvider() trace.TracerProvider {
	return otel.GetTracerProvider()
}

func testLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}
