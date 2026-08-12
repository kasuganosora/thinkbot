package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// SQLiteCompactor 测试
// ============================================================================

// mockClusterProvider 返回「将所有来源条目合并为一条」的聚类结果，
// 来源 ID 直接从 prompt 中的 [mem-xxx] 解析，确保与真实写入的条目对应。
type mockClusterProvider struct {
	calls int
}

func (m *mockClusterProvider) Name() string { return "mock" }

func (m *mockClusterProvider) DoGenerate(_ context.Context, p llm.GenerateParams) (*llm.GenerateResult, error) {
	m.calls++
	text := extractPromptText(p.Messages[0])
	re := regexp.MustCompile(`\[([a-z0-9-]+)\]`)
	matches := re.FindAllStringSubmatch(text, -1)
	var ids []string
	for _, mm := range matches {
		ids = append(ids, mm[1])
	}
	if len(ids) < 2 {
		// 不足两条：返回空数组（无可合并项）
		return &llm.GenerateResult{Text: "[]"}, nil
	}
	b, _ := json.Marshal(ids)
	merged := fmt.Sprintf(`[{"merged_content":"MERGED-%d","category":"fact","importance":0.8,"source_ids":%s}]`, len(ids), string(b))
	return &llm.GenerateResult{Text: merged}, nil
}

func (m *mockClusterProvider) DoStream(_ context.Context, _ llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

func extractPromptText(m llm.Message) string {
	var sb strings.Builder
	for _, p := range m.Content {
		if tp, ok := p.(llm.TextPart); ok {
			sb.WriteString(tp.Text)
		}
	}
	return sb.String()
}

func TestSQLiteCompactor_CompactScope(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	scope := memory.ChannelScope("compact-test")

	inputs := []string{"用户使用Go语言开发后端", "用户使用Gin框架", "用户喜欢猫"}
	for _, c := range inputs {
		if err := repo.Append(ctx, memory.Entry{
			Scope:    scope,
			Content:  c,
			Category: "fact",
			Source:   "conversation",
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	before, err := repo.totalChars(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if before <= 0 {
		t.Fatalf("expected positive totalChars before, got %d", before)
	}

	provider := &mockClusterProvider{}
	compactor := NewSQLiteCompactor(SQLiteCompactorConfig{
		Provider: provider,
		Model:    &llm.Model{ID: "mock"},
	}, zap.NewNop().Sugar())
	compactor.SetRepository(repo)

	if err := compactor.CompactScope(ctx, scope); err != nil {
		t.Fatalf("CompactScope: %v", err)
	}
	if provider.calls != 1 {
		t.Errorf("expected 1 LLM call, got %d", provider.calls)
	}

	active, err := repo.GetAllActive(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}

	// 应只剩 1 条合并后的 compactor 条目，来源已被归档
	var merged []memory.Entry
	for _, e := range active {
		if e.Source == "compactor" {
			merged = append(merged, e)
		}
	}
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged entry, got %d", len(merged))
	}
	if merged[0].Content != "MERGED-3" {
		t.Errorf("unexpected merged content: %q", merged[0].Content)
	}
	if len(active) != 1 {
		t.Errorf("expected only 1 active entry after archival, got %d", len(active))
	}

	after, err := repo.totalChars(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if after >= before {
		t.Errorf("expected totalChars to decrease after compaction: before=%d after=%d", before, after)
	}
}

func TestSQLiteRepository_ArchiveByID(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db)
	ctx := context.Background()
	scope := memory.ChannelScope("archive-test")

	if err := repo.Append(ctx, memory.Entry{
		Scope:   scope,
		Content: "待归档的记忆",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	entries, err := repo.GetAllActive(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 active entry, got %d", len(entries))
	}
	id := entries[0].ID

	// 首次归档
	if !repo.ArchiveByID(ctx, scope, id) {
		t.Fatal("expected ArchiveByID to succeed")
	}
	// 幂等：再次归档仍成功
	if !repo.ArchiveByID(ctx, scope, id) {
		t.Fatal("expected ArchiveByID idempotent success")
	}

	active, err := repo.GetAllActive(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Errorf("expected 0 active entries after archival, got %d", len(active))
	}

	// 不存在的 ID 应返回 false
	if repo.ArchiveByID(ctx, scope, "nope") {
		t.Error("expected ArchiveByID to fail for unknown id")
	}
}

// spyCompactor 记录被触发的次数，用于验证 Append 自动触发压缩。
type spyCompactor struct {
	mu    sync.Mutex
	calls int
}

func (s *spyCompactor) CompactScope(_ context.Context, _ memory.Scope) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return nil
}

func TestSQLiteRepository_AppendTriggersCompaction(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	scope := memory.ChannelScope("auto-compact")

	// 极小 window：MemoryBudget=1 token → 3 字符预算，超过即触发
	win := memory.NewWindow(memory.WindowConfig{
		MaxContextTokens: 10000,
		MaxMemoryTokens:  1,
	})
	spy := &spyCompactor{}
	repo := NewSQLiteRepository(db, SQLiteRepositoryConfig{
		Window:    win,
		Compactor: spy,
	})

	// 写入一条明显超过 3 字符预算的记忆
	if err := repo.Append(ctx, memory.Entry{
		Scope:   scope,
		Content: "这是一条明显超过预算的记忆内容",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// 等待异步 maybeCompact 触发
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		spy.mu.Lock()
		c := spy.calls
		spy.mu.Unlock()
		if c >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	spy.mu.Lock()
	c := spy.calls
	spy.mu.Unlock()
	if c < 1 {
		t.Errorf("expected Append to trigger compaction, got %d calls", c)
	}
}
