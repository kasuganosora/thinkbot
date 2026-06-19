package memory

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Tools — 工具定义验证
// ============================================================================

func TestTools_ReturnsFiveTools(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	if len(defs) != 5 {
		t.Fatalf("expected 5 tool defs, got %d", len(defs))
	}

	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, expected := range []string{"memory_search", "memory_write", "memory_delete", "memory_recent", "memory_count"} {
		if !names[expected] {
			t.Errorf("missing tool: %s", expected)
		}
	}
}

func TestTools_NilRepoReturnsNil(t *testing.T) {
	defs := ToolConfig{}
	result := Tools(defs)
	if result != nil {
		t.Errorf("expected nil for nil repo")
	}
}

// ============================================================================
// memory_search
// ============================================================================

func TestMemoryTool_Search(t *testing.T) {
	repo := NewMemoryRepository()
	repo.Append(context.Background(), Entry{
		Scope:   ChannelScope("ch1"),
		Content: "用户喜欢 Go 语言",
	})
	repo.Append(context.Background(), Entry{
		Scope:   ChannelScope("ch1"),
		Content: "用户喜欢 Python",
	})
	repo.Append(context.Background(), Entry{
		Scope:   ChannelScope("ch2"),
		Content: "另一个频道的记忆",
	})

	defs := Tools(ToolConfig{Repo: repo})
	searchDef := findMemoryTool(defs, "memory_search")
	if searchDef == nil {
		t.Fatal("memory_search not found")
	}

	result, err := searchDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"query":      "Go",
			"scope_kind": "channel",
			"scope_id":   "ch1",
		},
	)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	m := result.(map[string]any)
	count := m["count"].(int)
	if count != 1 {
		t.Fatalf("expected count=1, got %d", count)
	}
	entries := m["entries"].([]EntryResult)
	if entries[0].Content != "用户喜欢 Go 语言" {
		t.Errorf("unexpected content: %q", entries[0].Content)
	}
}

func TestMemoryTool_Search_NoResults(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	searchDef := findMemoryTool(defs, "memory_search")

	result, err := searchDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"query": "不存在的内容"},
	)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	m := result.(map[string]any)
	if m["count"].(int) != 0 {
		t.Errorf("expected count=0, got %v", m["count"])
	}
}

// ============================================================================
// memory_write
// ============================================================================

func TestMemoryTool_Write(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	writeDef := findMemoryTool(defs, "memory_write")

	result, err := writeDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"content":     "用户是一名后端工程师，擅长 Go 和 Rust",
			"category":    "fact",
			"importance":  0.8,
			"scope_kind":  "user",
			"scope_id":    "user123",
		},
	)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	m := result.(map[string]any)
	if !m["success"].(bool) {
		t.Error("expected success=true")
	}

	// 验证写入的内容可以通过搜索找到
	searchDef := findMemoryTool(defs, "memory_search")
	searchResult, _ := searchDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"query":      "后端工程师",
			"scope_kind": "user",
			"scope_id":   "user123",
		},
	)
	sm := searchResult.(map[string]any)
	if sm["count"].(int) != 1 {
		t.Errorf("expected to find 1 entry after write, got %d", sm["count"])
	}
}

func TestMemoryTool_Write_StripsThinkingAndToolOutput(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	writeDef := findMemoryTool(defs, "memory_write")

	writeDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"content": `<think>分析一下</think><tool_call>{"name":"x"}</tool_call>用户喜欢咖啡`,
		},
	)

	// 搜索验证内容被清理
	searchDef := findMemoryTool(defs, "memory_search")
	result, _ := searchDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"query": "咖啡"},
	)
	m := result.(map[string]any)
	entries := m["entries"].([]EntryResult)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Content != "用户喜欢咖啡" {
		t.Errorf("content not stripped: got %q", entries[0].Content)
	}
}

func TestMemoryTool_Write_EmptyContentError(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	writeDef := findMemoryTool(defs, "memory_write")

	_, err := writeDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"content": ""},
	)
	if err == nil {
		t.Error("expected error for empty content")
	}
}

// ============================================================================
// memory_delete
// ============================================================================

func TestMemoryTool_Delete(t *testing.T) {
	repo := NewMemoryRepository()
	repo.Append(context.Background(), Entry{
		Scope:   ChannelScope("ch1"),
		Content: "要删除的记忆",
	})

	// 获取 ID
	entries, _ := repo.Recent(context.Background(), ChannelScope("ch1"), 1)
	if len(entries) != 1 {
		t.Fatal("expected 1 entry")
	}
	entryID := entries[0].ID

	defs := Tools(ToolConfig{Repo: repo})
	deleteDef := findMemoryTool(defs, "memory_delete")

	result, err := deleteDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"memory_id":  entryID,
			"scope_kind": "channel",
			"scope_id":   "ch1",
		},
	)
	if err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	m := result.(map[string]any)
	if !m["success"].(bool) {
		t.Error("expected success=true")
	}

	// 验证已删除
	count, _ := repo.Count(context.Background(), ChannelScope("ch1"))
	if count != 0 {
		t.Errorf("expected count=0 after delete, got %d", count)
	}
}

// ============================================================================
// memory_recent
// ============================================================================

func TestMemoryTool_Recent(t *testing.T) {
	repo := NewMemoryRepository()
	for i := 0; i < 5; i++ {
		repo.Append(context.Background(), Entry{
			Scope:   ChannelScope("ch1"),
			Content: "记忆-" + string(rune('A'+i)),
		})
	}

	defs := Tools(ToolConfig{Repo: repo})
	recentDef := findMemoryTool(defs, "memory_recent")

	result, err := recentDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"scope_kind": "channel",
			"scope_id":   "ch1",
			"limit":      3,
		},
	)
	if err != nil {
		t.Fatalf("recent failed: %v", err)
	}

	m := result.(map[string]any)
	if m["count"].(int) != 3 {
		t.Errorf("expected count=3, got %d", m["count"])
	}
	entries := m["entries"].([]EntryResult)
	// 最新的在前面（倒序）
	if entries[0].Content != "记忆-E" {
		t.Errorf("expected first entry to be '记忆-E', got %q", entries[0].Content)
	}
}

func TestMemoryTool_Recent_EmptyScope(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	recentDef := findMemoryTool(defs, "memory_recent")

	result, err := recentDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{},
	)
	if err != nil {
		t.Fatalf("recent failed: %v", err)
	}

	m := result.(map[string]any)
	if m["count"].(int) != 0 {
		t.Errorf("expected count=0 for empty scope, got %d", m["count"])
	}
}

// ============================================================================
// memory_count
// ============================================================================

func TestMemoryTool_Count(t *testing.T) {
	repo := NewMemoryRepository()
	repo.Append(context.Background(), Entry{Scope: UserScope("u1"), Content: "a"})
	repo.Append(context.Background(), Entry{Scope: UserScope("u1"), Content: "b"})
	repo.Append(context.Background(), Entry{Scope: UserScope("u1"), Content: "c"})

	defs := Tools(ToolConfig{Repo: repo})
	countDef := findMemoryTool(defs, "memory_count")

	result, err := countDef.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"scope_kind": "user",
			"scope_id":   "u1",
		},
	)
	if err != nil {
		t.Fatalf("count failed: %v", err)
	}

	m := result.(map[string]any)
	if m["count"].(int) != 3 {
		t.Errorf("expected count=3, got %d", m["count"])
	}
}

// ============================================================================
// RegisterTools
// ============================================================================

func TestRegisterTools(t *testing.T) {
	mgr := newTestToolManager(t)
	repo := NewMemoryRepository()

	if err := RegisterTools(mgr, ToolConfig{Repo: repo}); err != nil {
		t.Fatalf("RegisterTools failed: %v", err)
	}

	if mgr.StaticCount() != 5 {
		t.Errorf("expected 5 tools registered, got %d", mgr.StaticCount())
	}
}

func TestRegisterTools_NilRepo(t *testing.T) {
	mgr := newTestToolManager(t)
	err := RegisterTools(mgr, ToolConfig{})
	if err == nil {
		t.Error("expected error for nil repo")
	}
}

// ============================================================================
// 端到端流程：写入 → 搜索 → 删除
// ============================================================================

func TestMemoryTool_EndToEnd(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	ctx := &llm.ToolExecContext{Context: context.Background()}

	writeDef := findMemoryTool(defs, "memory_write")
	searchDef := findMemoryTool(defs, "memory_search")
	deleteDef := findMemoryTool(defs, "memory_delete")

	// Step 1: 写入
	writeDef.Execute(ctx, map[string]any{
		"content":    "用户偏好深色主题",
		"category":   "preference",
		"scope_kind": "user",
		"scope_id":   "u1",
	})
	writeDef.Execute(ctx, map[string]any{
		"content":    "用户在用 MacBook Pro",
		"category":   "fact",
		"scope_kind": "user",
		"scope_id":   "u1",
	})

	// Step 2: 搜索
	result, _ := searchDef.Execute(ctx, map[string]any{
		"query":      "深色",
		"scope_kind": "user",
		"scope_id":   "u1",
	})
	m := result.(map[string]any)
	if m["count"].(int) != 1 {
		t.Fatalf("search: expected 1 result, got %d", m["count"])
	}
	entryID := m["entries"].([]EntryResult)[0].ID

	// Step 3: 删除
	deleteDef.Execute(ctx, map[string]any{
		"memory_id":  entryID,
		"scope_kind": "user",
		"scope_id":   "u1",
	})

	// Step 4: 再次搜索确认删除
	result, _ = searchDef.Execute(ctx, map[string]any{
		"query":      "深色",
		"scope_kind": "user",
		"scope_id":   "u1",
	})
	m = result.(map[string]any)
	if m["count"].(int) != 0 {
		t.Errorf("after delete: expected 0 results, got %d", m["count"])
	}

	// Step 5: 另一条记忆仍在
	result, _ = searchDef.Execute(ctx, map[string]any{
		"query":      "MacBook",
		"scope_kind": "user",
		"scope_id":   "u1",
	})
	m = result.(map[string]any)
	if m["count"].(int) != 1 {
		t.Errorf("other entry should still exist, got %d results", m["count"])
	}
}

func newTestToolManager(t *testing.T) *tools.ToolManager {
	t.Helper()
	return tools.NewToolManager(prompt.NewRegistry(), nil)
}

// ============================================================================
// Helpers
// ============================================================================

func findMemoryTool(defs []tools.ToolDef, name string) *tools.ToolDef {
	for i := range defs {
		if defs[i].Name == name {
			return &defs[i]
		}
	}
	return nil
}
