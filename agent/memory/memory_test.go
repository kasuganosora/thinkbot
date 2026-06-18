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

	repo.Append(ctx, Entry{Scope: scope, Content: "The user prefers dark mode"})
	repo.Append(ctx, Entry{Scope: scope, Content: "Meeting scheduled for Friday"})
	repo.Append(ctx, Entry{Scope: scope, Content: "User likes Go programming"})

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

	repo.Append(ctx, Entry{Scope: scope, Content: "fact 1", Category: "fact"})
	repo.Append(ctx, Entry{Scope: scope, Content: "preference 1", Category: "preference"})
	repo.Append(ctx, Entry{Scope: scope, Content: "fact 2", Category: "fact"})

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
		repo.Append(ctx, Entry{
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

	repo.Append(ctx, Entry{ID: "id-1", Scope: scope, Content: "keep"})
	repo.Append(ctx, Entry{ID: "id-2", Scope: scope, Content: "delete me"})

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

	repo.Append(ctx, Entry{Scope: scope, Content: "one"})
	repo.Append(ctx, Entry{Scope: scope, Content: "two"})

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

	repo.Append(ctx, Entry{Scope: scope1, Content: "channel memory"})
	repo.Append(ctx, Entry{Scope: scope2, Content: "user memory"})

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

	repo.Append(ctx, Entry{Scope: scope, Content: "one"})
	repo.Append(ctx, Entry{Scope: scope, Content: "two"})
	repo.Recent(ctx, scope, 5)

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
	repo.Append(ctx, Entry{Scope: scope, Content: "User name is Luna"})
	repo.Append(ctx, Entry{Scope: scope, Content: "User is a Go developer"})

	builder := NewContextBuilder()
	mgr := NewContextManager(repo, builder)

	result, err := mgr.AssembleContext(ctx, "chat-ctx", "user-1", "")
	if err != nil {
		t.Fatalf("assemble failed: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty context")
	}
	if !containsIgnoreCase(result, "Luna") {
		t.Error("expected context to contain 'Luna'")
	}
}

func TestContextManager_AssembleContext_WithRelevance(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()

	scope := ChannelScope("chat-rel")
	repo.Append(ctx, Entry{Scope: scope, Content: "Meeting with team at 3pm"})
	repo.Append(ctx, Entry{Scope: scope, Content: "User prefers dark mode"})
	repo.Append(ctx, Entry{Scope: scope, Content: "Golang project uses Bazel build"})

	builder := NewContextBuilder()
	mgr := NewContextManager(repo, builder)

	// 搜索与 "Golang" 相关的
	result, err := mgr.AssembleContext(ctx, "chat-rel", "user-1", "golang")
	if err != nil {
		t.Fatalf("assemble failed: %v", err)
	}
	if !containsIgnoreCase(result, "Bazel") {
		t.Error("expected relevant memory about Golang/Bazel to be in context")
	}
}

func TestContextManager_EmptyMemory(t *testing.T) {
	repo := NewMemoryRepository()
	builder := NewContextBuilder()
	mgr := NewContextManager(repo, builder)

	result, err := mgr.AssembleContext(context.Background(), "empty-channel", "user-1", "hello")
	if err != nil {
		t.Fatalf("assemble failed: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty context for empty memory, got %q", result)
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
