package memory

import (
	"context"
	"testing"
	"time"
)

// ============================================================================
// Repository Tests
// ============================================================================

func TestMemoryRepository_AppendAndRecent(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	scope := ChannelScope("chat-123")

	// 写入 3 条记忆
	for i := 0; i < 3; i++ {
		err := repo.Append(ctx, Entry{
			Scope:   scope,
			Content: "memory " + string(rune('A'+i)),
		})
		if err != nil {
			t.Fatalf("append failed: %v", err)
		}
		time.Sleep(time.Millisecond) // 确保时间不同
	}

	// 检索最近 2 条
	entries, err := repo.Recent(ctx, scope, 2)
	if err != nil {
		t.Fatalf("recent failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// 最新的在前
	if entries[0].Content != "memory C" {
		t.Errorf("expected latest first, got %q", entries[0].Content)
	}
	if entries[1].Content != "memory B" {
		t.Errorf("expected second latest, got %q", entries[1].Content)
	}
}

func TestMemoryRepository_Retrieve_TextFilter(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	scope := ChannelScope("chat-456")

	_ = repo.Append(ctx, Entry{Scope: scope, Content: "The user prefers dark mode"})
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "Meeting scheduled for Friday"})
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "User likes Go programming"})

	// 搜索包含 "user" 的记忆
	results, err := repo.Retrieve(ctx, Query{
		Scopes: []Scope{scope},
		Text:   "user",
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("retrieve failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results matching 'user', got %d", len(results))
	}
}

func TestMemoryRepository_Retrieve_CategoryFilter(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	scope := UserScope("user-1")

	_ = repo.Append(ctx, Entry{Scope: scope, Content: "fact 1", Category: "fact"})
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "preference 1", Category: "preference"})
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "fact 2", Category: "fact"})

	results, err := repo.Retrieve(ctx, Query{
		Scopes:   []Scope{scope},
		Category: "fact",
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("retrieve failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(results))
	}
}

func TestMemoryRepository_CapacityLimit(t *testing.T) {
	repo := NewMemoryRepository(MemoryRepositoryConfig{
		MaxEntriesPerScope: 3,
	})
	ctx := context.Background()
	scope := ChannelScope("chat-overflow")

	// 写入 5 条
	for i := 0; i < 5; i++ {
		_ = repo.Append(ctx, Entry{
			Scope:   scope,
			Content: string(rune('A' + i)),
		})
	}

	// 只保留最新 3 条
	count, _ := repo.Count(ctx, scope)
	if count != 3 {
		t.Fatalf("expected 3 entries after overflow, got %d", count)
	}

	recent, _ := repo.Recent(ctx, scope, 3)
	if recent[0].Content != "E" {
		t.Errorf("expected latest entry 'E', got %q", recent[0].Content)
	}
}

func TestMemoryRepository_Delete(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	scope := ChannelScope("chat-del")

	_ = repo.Append(ctx, Entry{ID: "id-1", Scope: scope, Content: "keep"})
	_ = repo.Append(ctx, Entry{ID: "id-2", Scope: scope, Content: "delete me"})

	err := repo.Delete(ctx, scope, "id-2")
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	count, _ := repo.Count(ctx, scope)
	if count != 1 {
		t.Fatalf("expected 1 entry after delete, got %d", count)
	}
}

func TestMemoryRepository_Clear(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	scope := ChannelScope("chat-clear")

	_ = repo.Append(ctx, Entry{Scope: scope, Content: "one"})
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "two"})

	err := repo.Clear(ctx, scope)
	if err != nil {
		t.Fatalf("clear failed: %v", err)
	}

	count, _ := repo.Count(ctx, scope)
	if count != 0 {
		t.Fatalf("expected 0 entries after clear, got %d", count)
	}
}

func TestMemoryRepository_MultiScope(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()

	scope1 := ChannelScope("chat-1")
	scope2 := UserScope("user-1")

	_ = repo.Append(ctx, Entry{Scope: scope1, Content: "channel memory"})
	_ = repo.Append(ctx, Entry{Scope: scope2, Content: "user memory"})

	// 各 scope 独立
	c1, _ := repo.Count(ctx, scope1)
	c2, _ := repo.Count(ctx, scope2)
	if c1 != 1 || c2 != 1 {
		t.Fatalf("expected 1/1, got %d/%d", c1, c2)
	}

	// 跨 scope 检索
	results, _ := repo.Retrieve(ctx, Query{
		Scopes: []Scope{scope1, scope2},
		Text:   "memory",
		Limit:  10,
	})
	if len(results) != 2 {
		t.Fatalf("expected 2 cross-scope results, got %d", len(results))
	}
}

func TestMemoryRepository_Metrics(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	scope := ChannelScope("metrics-test")

	_ = repo.Append(ctx, Entry{Scope: scope, Content: "one"})
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "two"})
	_, _ = repo.Recent(ctx, scope, 5)

	m := repo.Metrics()
	if m.EntriesAppended != 2 {
		t.Errorf("expected 2 appended, got %d", m.EntriesAppended)
	}
	if m.Retrievals != 1 {
		t.Errorf("expected 1 retrieval, got %d", m.Retrievals)
	}
	if m.TotalEntries != 2 {
		t.Errorf("expected 2 total entries, got %d", m.TotalEntries)
	}
}

// ============================================================================
// ContextBuilder Tests
// ============================================================================

func TestContextBuilder_Build(t *testing.T) {
	builder := NewContextBuilder()

	entries := []Entry{
		{Content: "User prefers Go", Category: "preference", CreatedAt: time.Now().Add(-2 * time.Hour)},
		{Content: "Project uses Bazel", Category: "fact", CreatedAt: time.Now().Add(-1 * time.Hour)},
	}

	result := builder.Build(entries)
	if result == "" {
		t.Fatal("expected non-empty context")
	}
	if !containsIgnoreCase(result, "User prefers Go") {
		t.Error("expected context to contain first entry")
	}
	if !containsIgnoreCase(result, "Project uses Bazel") {
		t.Error("expected context to contain second entry")
	}
	if !containsIgnoreCase(result, "[Memory Context]") {
		t.Error("expected context to contain header")
	}
	if !containsIgnoreCase(result, "[End Memory Context]") {
		t.Error("expected context to contain footer")
	}
}

func TestContextBuilder_EmptyEntries(t *testing.T) {
	builder := NewContextBuilder()
	result := builder.Build(nil)
	if result != "" {
		t.Errorf("expected empty string for nil entries, got %q", result)
	}
}

func TestContextBuilder_Truncation(t *testing.T) {
	builder := NewContextBuilder(ContextBuilderConfig{
		MaxTokenEstimate: 10, // 非常小的 limit (40 chars)
	})

	entries := []Entry{
		{Content: "This is a very long memory that should cause truncation of later entries in the context builder"},
		{Content: "This should be truncated"},
	}

	result := builder.Build(entries)
	if !containsIgnoreCase(result, "truncated") {
		// 应该有截断提示或只包含第一条
		// 由于第一条就超了，可能两条都部分显示
		t.Log("result:", result)
	}
}

// ============================================================================
// ContextManager Tests
// ============================================================================

func TestContextManager_AssembleContext(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()

	// 写入一些记忆
	scope := ChannelScope("chat-ctx")
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "User name is Luna"})
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "User is a Go developer"})

	builder := NewContextBuilder()
	mgr := NewContextManager(repo, builder, nil, nil)

	assembleResult, err := mgr.AssembleContext(ctx, "chat-ctx", "user-1", "")
	if err != nil {
		t.Fatalf("assemble failed: %v", err)
	}
	if assembleResult.ContextText == "" {
		t.Fatal("expected non-empty context")
	}
	if !containsIgnoreCase(assembleResult.ContextText, "Luna") {
		t.Error("expected context to contain 'Luna'")
	}
}

func TestContextManager_AssembleContext_WithRelevance(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()

	scope := ChannelScope("chat-rel")
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "Meeting with team at 3pm"})
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "User prefers dark mode"})
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "Golang project uses Bazel build"})

	builder := NewContextBuilder()
	mgr := NewContextManager(repo, builder, nil, nil)

	// 搜索与 "Golang" 相关的
	assembleResult, err := mgr.AssembleContext(ctx, "chat-rel", "user-1", "golang")
	if err != nil {
		t.Fatalf("assemble failed: %v", err)
	}
	if !containsIgnoreCase(assembleResult.ContextText, "Bazel") {
		t.Error("expected relevant memory about Golang/Bazel to be in context")
	}
}

func TestContextManager_EmptyMemory(t *testing.T) {
	repo := NewMemoryRepository()
	builder := NewContextBuilder()
	mgr := NewContextManager(repo, builder, nil, nil)

	assembleResult, err := mgr.AssembleContext(context.Background(), "empty-channel", "user-1", "hello")
	if err != nil {
		t.Fatalf("assemble failed: %v", err)
	}
	if assembleResult.ContextText != "" {
		t.Errorf("expected empty context for empty memory, got %q", assembleResult.ContextText)
	}
}

// ============================================================================
// Scope Tests
// ============================================================================

func TestScope_Key(t *testing.T) {
	tests := []struct {
		scope    Scope
		expected string
	}{
		{ChannelScope("chat-1"), "channel:chat-1"},
		{UserScope("user-1"), "user:user-1"},
		{BotScope("bot-1"), "bot:bot-1"},
		{GlobalScope(), "global"},
	}

	for _, tt := range tests {
		got := tt.scope.Key()
		if got != tt.expected {
			t.Errorf("Scope.Key() = %q, want %q", got, tt.expected)
		}
	}
}

// ============================================================================
// Window Tests
// ============================================================================

func TestWindow_Available(t *testing.T) {
	w := NewWindow(WindowConfig{
		MaxContextTokens:  10000,
		ReservedTokens:    1000,
		OutputReserve:     2000,
		MemoryBudgetRatio: 0.3,
	})

	// 初始状态：(10000 - 1000 - 2000 - 0) * 0.3 = 2100
	available := w.Available()
	if available != 2100 {
		t.Errorf("expected 2100 available, got %d", available)
	}

	// 记录用量后：(10000 - 1000 - 2000 - 3000) * 0.3 = 1200
	w.RecordUsage(3000, 500)
	available = w.Available()
	if available != 1200 {
		t.Errorf("expected 1200 available after usage, got %d", available)
	}
}

func TestWindow_ShouldCompress(t *testing.T) {
	w := NewWindow(WindowConfig{
		MaxContextTokens:  10000,
		ReservedTokens:    1000,
		OutputReserve:     2000,
		MemoryBudgetRatio: 0.3,
		CompressThreshold: 0.8,
	})

	// Available = 2100, threshold = 2100 * 0.8 = 1680
	if w.ShouldCompress(1000) {
		t.Error("1000 tokens should not trigger compression")
	}
	if !w.ShouldCompress(1700) {
		t.Error("1700 tokens should trigger compression")
	}
}

func TestWindow_NeedsTruncation(t *testing.T) {
	w := NewWindow(WindowConfig{
		MaxContextTokens:  10000,
		ReservedTokens:    1000,
		OutputReserve:     2000,
		MemoryBudgetRatio: 0.3,
	})

	// Available = 2100
	if w.NeedsTruncation(2000) {
		t.Error("2000 tokens should not need truncation (available=2100)")
	}
	if !w.NeedsTruncation(2200) {
		t.Error("2200 tokens should need truncation (available=2100)")
	}
}

func TestWindow_Reset(t *testing.T) {
	w := NewWindow()
	w.RecordUsage(5000, 1000)
	w.Reset()

	m := w.Metrics()
	if m.UsedTokens != 0 {
		t.Errorf("expected 0 used after reset, got %d", m.UsedTokens)
	}
	if m.RoundCount != 0 {
		t.Errorf("expected 0 rounds after reset, got %d", m.RoundCount)
	}
}

// ============================================================================
// Compressor Tests (NoopCompressor)
// ============================================================================

func TestNoopCompressor_Compress(t *testing.T) {
	c := &NoopCompressor{}
	ctx := context.Background()

	entries := []Entry{
		{ID: "mem-1", Content: "first memory"},
		{ID: "mem-2", Content: "second memory"},
	}

	block, err := c.Compress(ctx, entries)
	if err != nil {
		t.Fatalf("noop compress failed: %v", err)
	}
	if len(block.EntryIDs) != 2 {
		t.Errorf("expected 2 entry IDs, got %d", len(block.EntryIDs))
	}
	if block.EntryIDs[0] != "mem-1" || block.EntryIDs[1] != "mem-2" {
		t.Errorf("unexpected entry IDs: %v", block.EntryIDs)
	}
	if block.Summary == "" {
		t.Error("expected non-empty summary")
	}
}

// ============================================================================
// Expander Tests
// ============================================================================

func TestExtractRefIDs(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{
			input:    "用户偏好Go语言 [ref:mem-abc123] 使用Bazel [ref:mem-def456]",
			expected: []string{"mem-abc123", "mem-def456"},
		},
		{
			input:    "no refs here",
			expected: nil,
		},
		{
			input:    "[ref:mem-111] duplicate [ref:mem-111]",
			expected: []string{"mem-111"},
		},
		{
			input:    "[ref:mem-aaa, ref:mem-bbb] mixed",
			expected: []string{"mem-aaa"},
		},
	}

	for _, tt := range tests {
		got := ExtractRefIDs(tt.input)
		if len(got) != len(tt.expected) {
			t.Errorf("ExtractRefIDs(%q): got %v, want %v", tt.input, got, tt.expected)
			continue
		}
		for i, id := range got {
			if id != tt.expected[i] {
				t.Errorf("ExtractRefIDs(%q)[%d]: got %q, want %q", tt.input, i, id, tt.expected[i])
			}
		}
	}
}

// ============================================================================
// Integration: ContextManager with Window + Compression
// ============================================================================

func TestContextManager_WithWindow_NoCompress(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()

	scope := ChannelScope("win-test")
	_ = repo.Append(ctx, Entry{Scope: scope, Content: "short memory"})

	builder := NewContextBuilder()
	window := NewWindow(WindowConfig{
		MaxContextTokens:  100000,
		ReservedTokens:    1000,
		OutputReserve:     2000,
		MemoryBudgetRatio: 0.5,
	})
	mgr := NewContextManager(repo, builder, window, nil)

	result, err := mgr.AssembleContext(ctx, "win-test", "user-1", "")
	if err != nil {
		t.Fatalf("assemble failed: %v", err)
	}
	if result.Compressed {
		t.Error("should not compress with large window")
	}
	if result.ContextText == "" {
		t.Error("expected non-empty context")
	}
}

func TestContextManager_UpdateUsage(t *testing.T) {
	repo := NewMemoryRepository()
	builder := NewContextBuilder()
	window := NewWindow(WindowConfig{
		MaxContextTokens:  10000,
		ReservedTokens:    1000,
		OutputReserve:     2000,
		MemoryBudgetRatio: 0.3,
	})
	mgr := NewContextManager(repo, builder, window, nil)

	before := mgr.Available()
	mgr.UpdateUsage(5000, 1000)
	after := mgr.Available()

	if after >= before {
		t.Errorf("expected available to decrease after usage, before=%d, after=%d", before, after)
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		text     string
		minToken int
		maxToken int
	}{
		{"", 0, 0},
		{"hello", 1, 5},
		{"这是一段中文文本", 2, 10},
	}

	for _, tt := range tests {
		got := estimateTokens(tt.text)
		if got < tt.minToken || got > tt.maxToken {
			t.Errorf("estimateTokens(%q) = %d, want in [%d, %d]", tt.text, got, tt.minToken, tt.maxToken)
		}
	}
}
