package memory

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// MockLLMProvider 用于测试的 LLM provider。
type MockLLMProvider struct {
	name     string
	response string
	err      error
	calls    int
}

func (m *MockLLMProvider) Name() string { return m.name }

func (m *MockLLMProvider) DoGenerate(_ context.Context, _ llm.GenerateParams) (*llm.GenerateResult, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return &llm.GenerateResult{
		Text: m.response,
	}, nil
}

func (m *MockLLMProvider) DoStream(_ context.Context, _ llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

func TestFormationPipeline_ShortContentSkip(t *testing.T) {
	mock := &MockLLMProvider{name: "mock"}
	f := NewFormationPipeline(FormationConfig{
		Provider:      mock,
		MinContentLen: 100, // 高阈值
	}, testTracerProvider(), testLogger())

	store := NewTieredStore(nil)
	scope := ChannelScope("test")

	result, err := f.ProcessTurn(context.Background(), store, scope, "hi", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Extracted != 0 {
		t.Errorf("expected 0 extracted for short content, got %d", result.Extracted)
	}
	if mock.calls != 0 {
		t.Errorf("expected 0 LLM calls for short content, got %d", mock.calls)
	}
}

func TestFormationPipeline_ExtractAndAdd(t *testing.T) {
	ctx := context.Background()
	mock := &MockLLMProvider{
		name:     "mock",
		response: `[{"content":"用户使用Go语言","category":"fact","importance":0.9},{"content":"用户偏好简洁回复","category":"preference","importance":0.7}]`,
	}
	f := NewFormationPipeline(FormationConfig{
		Provider:        mock,
		MinContentLen:   10,
		MaxFactsPerTurn: 5,
	}, testTracerProvider(), testLogger())

	store := NewTieredStore(nil)
	scope := ChannelScope("test")

	// 第一次调用: extract → no existing → ADD all
	result, err := f.ProcessTurn(ctx, store, scope,
		"我用Go语言写后端，帮我优化一下这段代码",
		"好的，我看了一下你的Go代码，以下是优化建议...")
	if err != nil {
		t.Fatal(err)
	}

	// 第一次只做 extract（LLM 返回 facts JSON）
	// 但 decide 阶段因为 existing 为空，不会调 LLM
	if mock.calls != 1 {
		t.Logf("LLM calls: %d (extract only, no existing for decide)", mock.calls)
	}

	if result.Extracted != 2 {
		t.Errorf("expected 2 extracted, got %d", result.Extracted)
	}
	if result.Added != 2 {
		t.Errorf("expected 2 added, got %d", result.Added)
	}

	// 验证 L1 中有 2 条
	l1, _ := store.GetAll(ctx, Tier1LongTerm, scope)
	if len(l1) != 2 {
		t.Errorf("expected 2 L1 entries, got %d", len(l1))
	}
}

func TestFormationPipeline_EmptyFacts(t *testing.T) {
	mock := &MockLLMProvider{
		name:     "mock",
		response: `[]`, // LLM 返回空数组
	}
	f := NewFormationPipeline(FormationConfig{
		Provider:      mock,
		MinContentLen: 10,
	}, testTracerProvider(), testLogger())

	store := NewTieredStore(nil)
	scope := ChannelScope("test")

	result, err := f.ProcessTurn(context.Background(), store, scope,
		"这是一段足够长的对话内容用于触发提取",
		"助手回复也是一段足够长的内容")
	if err != nil {
		t.Fatal(err)
	}
	if result.Extracted != 0 {
		t.Errorf("expected 0 extracted for empty facts, got %d", result.Extracted)
	}
}

func TestSemanticCompactor_TooFewEntries(t *testing.T) {
	mock := &MockLLMProvider{name: "mock"}
	c := NewSemanticCompactor(CompactionConfig{
		Provider:       mock,
		MinClusterSize: 2,
	}, testTracerProvider(), testLogger())

	store := NewTieredStore(nil)
	scope := ChannelScope("test")

	// 只有一条记忆，不触发压缩
	_ = store.Append(context.TODO(), TieredEntry{
		Entry: Entry{Scope: scope, Content: "单条记忆"},
		Tier:  Tier1LongTerm,
	})

	report, err := c.Compact(context.Background(), store, scope)
	if err != nil {
		t.Fatal(err)
	}
	if report.ClustersFound != 0 {
		t.Errorf("expected 0 clusters, got %d", report.ClustersFound)
	}
	if mock.calls != 0 {
		t.Errorf("expected 0 LLM calls, got %d", mock.calls)
	}
}

func TestSemanticCompactor_ClusterMerge(t *testing.T) {
	ctx := context.Background()
	mock := &MockLLMProvider{
		name:     "mock",
		response: `[{"merged_content":"用户使用Go语言和Gin框架做后端开发","category":"fact","importance":0.9,"source_ids":["mem-1","mem-2"]}]`,
	}
	c := NewSemanticCompactor(CompactionConfig{
		Provider:       mock,
		MinClusterSize: 2,
		MaxClusterSize: 10,
	}, testTracerProvider(), testLogger())

	store := NewTieredStore(nil)
	scope := ChannelScope("test")

	// 两条相似记忆
	_ = store.Append(context.TODO(), TieredEntry{
		Entry: Entry{ID: "mem-1", Scope: scope, Content: "用户使用 Go 语言", Category: "fact"},
		Tier:  Tier1LongTerm,
	})
	_ = store.Append(context.TODO(), TieredEntry{
		Entry: Entry{ID: "mem-2", Scope: scope, Content: "用户使用 Gin 框架", Category: "fact"},
		Tier:  Tier1LongTerm,
	})

	report, err := c.Compact(ctx, store, scope)
	if err != nil {
		t.Fatal(err)
	}

	if report.ClustersFound != 1 {
		t.Errorf("expected 1 cluster, got %d", report.ClustersFound)
	}
	if report.ArchivedCount != 2 {
		t.Errorf("expected 2 archived, got %d", report.ArchivedCount)
	}

	// 验证有 archived 标记的条目
	all, _ := store.GetAll(ctx, Tier1LongTerm, scope)
	archivedCount := 0
	mergedCount := 0
	for _, e := range all {
		if e.Metadata != nil {
			if archived, ok := e.Metadata["archived"].(bool); ok && archived {
				archivedCount++
			}
			if _, ok := e.Metadata["compacted_at"]; ok {
				mergedCount++
			}
		}
	}
	if archivedCount != 2 {
		t.Errorf("expected 2 archived entries, got %d", archivedCount)
	}
	if mergedCount != 1 {
		t.Errorf("expected 1 merged entry, got %d", mergedCount)
	}
}

func TestSemanticCompactor_PurgeArchived(t *testing.T) {
	ctx := context.Background()
	mock := &MockLLMProvider{
		name:     "mock",
		response: `[{"merged_content":"合并内容","category":"fact","importance":0.9,"source_ids":["a","b"]}]`,
	}
	c := NewSemanticCompactor(CompactionConfig{
		Provider:       mock,
		MinClusterSize: 2,
	}, testTracerProvider(), testLogger())

	store := NewTieredStore(nil)
	scope := ChannelScope("test")

	_ = store.Append(context.TODO(), TieredEntry{
		Entry: Entry{ID: "a", Scope: scope, Content: "记忆A"},
		Tier:  Tier1LongTerm,
	})
	_ = store.Append(context.TODO(), TieredEntry{
		Entry: Entry{ID: "b", Scope: scope, Content: "记忆B"},
		Tier:  Tier1LongTerm,
	})

	_, _ = c.Compact(ctx, store, scope)

	// 压缩前有 3 条（2 archived + 1 merged）
	all, _ := store.GetAll(ctx, Tier1LongTerm, scope)
	if len(all) != 3 {
		t.Fatalf("expected 3 entries before purge, got %d", len(all))
	}

	// 物理删除 archived
	removed, err := c.PurgeArchived(ctx, store, scope)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}

	// 压缩后只剩 1 条（合并后的）
	all, _ = store.GetAll(ctx, Tier1LongTerm, scope)
	if len(all) != 1 {
		t.Errorf("expected 1 entry after purge, got %d", len(all))
	}
}
