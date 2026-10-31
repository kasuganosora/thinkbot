package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Tools — 工具定义验证（统一 memory 工具）
// ============================================================================

func TestTools_Definition(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	if len(defs) != 1 {
		t.Fatalf("expected 1 tool def, got %d", len(defs))
	}
	if defs[0].Name != "memory" {
		t.Errorf("expected tool name 'memory', got '%s'", defs[0].Name)
	}
}

// ============================================================================
// search
// ============================================================================

func TestMemoryTool_Search(t *testing.T) {
	repo := NewMemoryRepository()
	_ = repo.Append(context.Background(), Entry{
		Scope:   ChannelScope("ch1"),
		Content: "用户喜欢 Go 语言",
	})
	_ = repo.Append(context.Background(), Entry{
		Scope:   ChannelScope("ch1"),
		Content: "用户喜欢 Python",
	})
	_ = repo.Append(context.Background(), Entry{
		Scope:   ChannelScope("ch2"),
		Content: "另一个频道的记忆",
	})

	defs := Tools(ToolConfig{Repo: repo})
	tool := defs[0].Tool

	result, err := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"action":     "search",
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
	tool := defs[0].Tool

	result, err := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"action": "search",
			"query":  "不存在的内容",
		},
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
// add
// ============================================================================

func TestMemoryTool_Add(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	tool := defs[0].Tool

	result, err := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"action":     "add",
			"content":    "用户是一名后端工程师，擅长 Go 和 Rust",
			"category":   "fact",
			"scope_kind": "user",
			"scope_id":   "user123",
		},
	)
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}

	m := result.(map[string]any)
	if !m["success"].(bool) {
		t.Error("expected success=true")
	}

	// 验证写入的内容可以通过搜索找到
	searchResult, _ := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"action":     "search",
			"query":      "后端工程师",
			"scope_kind": "user",
			"scope_id":   "user123",
		},
	)
	sm := searchResult.(map[string]any)
	if sm["count"].(int) != 1 {
		t.Errorf("expected to find 1 entry after add, got %d", sm["count"])
	}
}

func TestMemoryTool_Add_StripsThinkingAndToolOutput(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	tool := defs[0].Tool

	_, _ = tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"action":  "add",
			"content": `<think>分析一下</think><tool_call>{"name":"x"}</tool_call>用户喜欢咖啡`,
		},
	)

	// 搜索验证内容被清理
	result, _ := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"action": "search",
			"query":  "咖啡",
		},
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

func TestMemoryTool_Add_EmptyContentError(t *testing.T) {
	repo := NewMemoryRepository()
	defs := Tools(ToolConfig{Repo: repo})
	tool := defs[0].Tool

	_, err := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"action":  "add",
			"content": "",
		},
	)
	if err == nil {
		t.Error("expected error for empty content")
	}
}

// ============================================================================
// remove (by ID)
// ============================================================================

func TestMemoryTool_Remove_ByID(t *testing.T) {
	repo := NewMemoryRepository()
	_ = repo.Append(context.Background(), Entry{
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
	tool := defs[0].Tool

	result, err := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"action":     "remove",
			"memory_id":  entryID,
			"scope_kind": "channel",
			"scope_id":   "ch1",
		},
	)
	if err != nil {
		t.Fatalf("remove failed: %v", err)
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

// TestMemoryTool_Remove_CrossScopeByID 验证：从 channel 会话按 ID 删除一条
// 落在 bot 全局作用域的记忆时，工具能跨 scope 解析并真正删掉（修复假记忆无法自清理）。
func TestMemoryTool_Remove_CrossScopeByID(t *testing.T) {
	repo := NewMemoryRepository()
	botScope := BotScope("botX")
	_ = repo.Append(context.Background(), Entry{
		Scope:   botScope,
		Content: "bot 作用域下的过期心跳记忆",
	})
	entries, _ := repo.Recent(context.Background(), botScope, 1)
	if len(entries) != 1 {
		t.Fatal("expected 1 bot-scope entry")
	}
	entryID := entries[0].ID

	// 从 channel 会话删除该 bot-scope 记忆（按 ID 跨 scope）
	defs := Tools(ToolConfig{Repo: repo, BotID: "botX"})
	tool := defs[0].Tool
	result, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"action":     "remove",
		"id":         entryID,
		"scope_kind": "channel",
		"scope_id":   "ch1",
	})
	if err != nil {
		t.Fatalf("remove failed: %v", err)
	}
	m := result.(map[string]any)
	if !m["success"].(bool) {
		t.Fatalf("expected success=true, got %v", m)
	}
	// 确认已从 bot scope 删除
	left, _ := repo.Count(context.Background(), botScope)
	if left != 0 {
		t.Errorf("expected bot-scope count=0 after cross-scope delete, got %d", left)
	}
}

// Every conversation of a bot can remove its shared memory by substring.
func TestMemoryTool_Remove_SubstringUsesSharedBotMemory(t *testing.T) {
	repo := NewMemoryRepository()
	botScope := BotScope("botX")
	_ = repo.Append(context.Background(), Entry{
		Scope:   botScope,
		Content: "cfblog a11y 任务仍未完成收尾",
	})
	defs := Tools(ToolConfig{Repo: repo, BotID: "botX"})
	tool := defs[0].Tool
	result, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"action":     "remove",
		"old_text":   "cfblog a11y 任务仍未完成收尾",
		"scope_kind": "channel",
		"scope_id":   "ch1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := result.(map[string]any)
	if m["success"] != true {
		t.Fatalf("same-bot memory was not shared: %#v", m)
	}
	if n, _ := repo.Count(context.Background(), botScope); n != 0 {
		t.Fatal("shared memory was not removed")
	}
}

// TestMemoryTool_BatchRemove_CrossScopeByID 验证：batch 的 remove 支持 id 跨 scope 删除。
func TestMemoryTool_BatchRemove_CrossScopeByID(t *testing.T) {
	repo := NewMemoryRepository()
	botScope := BotScope("botX")
	_ = repo.Append(context.Background(), Entry{
		Scope:   botScope,
		Content: "要批量删除的 bot 记忆",
	})
	entries, _ := repo.Recent(context.Background(), botScope, 1)
	if len(entries) != 1 {
		t.Fatal("expected 1 bot-scope entry")
	}
	entryID := entries[0].ID

	defs := Tools(ToolConfig{Repo: repo, BotID: "botX"})
	tool := defs[0].Tool
	result, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"action":     "batch",
		"scope_kind": "channel",
		"scope_id":   "ch1",
		"operations": []any{
			map[string]any{"action": "remove", "id": entryID},
		},
	})
	if err != nil {
		t.Fatalf("batch remove failed: %v", err)
	}
	m := result.(map[string]any)
	if !m["success"].(bool) {
		t.Fatalf("expected success=true, got %v", m)
	}
	left, _ := repo.Count(context.Background(), botScope)
	if left != 0 {
		t.Errorf("expected bot-scope count=0 after batch cross-scope delete, got %d", left)
	}
}

// ============================================================================
// recent
// ============================================================================

func TestMemoryTool_Recent(t *testing.T) {
	repo := NewMemoryRepository()
	for i := 0; i < 5; i++ {
		_ = repo.Append(context.Background(), Entry{
			Scope:   ChannelScope("ch1"),
			Content: "记忆-" + string(rune('A'+i)),
		})
	}

	defs := Tools(ToolConfig{Repo: repo})
	tool := defs[0].Tool

	result, err := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"action":     "recent",
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
	tool := defs[0].Tool

	result, err := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"action": "recent"},
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
// count
// ============================================================================

func TestMemoryTool_Count(t *testing.T) {
	repo := NewMemoryRepository()
	_ = repo.Append(context.Background(), Entry{Scope: UserScope("u1"), Content: "a"})
	_ = repo.Append(context.Background(), Entry{Scope: UserScope("u1"), Content: "b"})
	_ = repo.Append(context.Background(), Entry{Scope: UserScope("u1"), Content: "c"})

	defs := Tools(ToolConfig{Repo: repo})
	tool := defs[0].Tool

	result, err := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{
			"action":     "count",
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

	if mgr.StaticCount() != 1 {
		t.Errorf("expected 1 tool registered, got %d", mgr.StaticCount())
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
	tool := defs[0].Tool

	// Step 1: 写入
	_, _ = tool.Execute(ctx, map[string]any{
		"action":     "add",
		"content":    "用户偏好深色主题",
		"category":   "preference",
		"scope_kind": "user",
		"scope_id":   "u1",
	})
	_, _ = tool.Execute(ctx, map[string]any{
		"action":     "add",
		"content":    "用户在用 MacBook Pro",
		"category":   "fact",
		"scope_kind": "user",
		"scope_id":   "u1",
	})

	// Step 2: 搜索
	result, _ := tool.Execute(ctx, map[string]any{
		"action":     "search",
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
	_, _ = tool.Execute(ctx, map[string]any{
		"action":     "remove",
		"memory_id":  entryID,
		"scope_kind": "user",
		"scope_id":   "u1",
	})

	// Step 4: 再次搜索确认删除
	result, _ = tool.Execute(ctx, map[string]any{
		"action":     "search",
		"query":      "深色",
		"scope_kind": "user",
		"scope_id":   "u1",
	})
	m = result.(map[string]any)
	if m["count"].(int) != 0 {
		t.Errorf("after delete: expected 0 results, got %d", m["count"])
	}

	// Step 5: 另一条记忆仍在
	result, _ = tool.Execute(ctx, map[string]any{
		"action":     "search",
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
	return tools.NewToolManager(prompt.NewRegistry(), nil, nil)
}

// ============================================================================
// 时间轴问答（回归防线）
//
// 事故：用户问「你最早的记忆是什么时候」，bot 答「9 月」，真实最早是 8 月。
// 根因不是模型笨，而是工具**没有取最早的能力**：search/recent 一律按时间倒序
// 取前 N 条，模型只能在「最近这批」里找最早的一条。下面锁住四条能力：
// order=oldest、since/until、count 的时间跨度、scope_kind=all。
// ============================================================================

// seedTimeSpanRepo 写入跨三个月、跨两个 scope 的时间轴样本。
func seedTimeSpanRepo(t *testing.T, repo Repository) (jul, aug, sep time.Time) {
	t.Helper()
	ctx := context.Background()
	jul = time.Date(2026, 7, 1, 10, 0, 0, 0, time.Local)
	aug = time.Date(2026, 8, 12, 19, 39, 0, 0, time.Local)
	sep = time.Date(2026, 9, 19, 10, 49, 0, 0, time.Local)

	entries := []Entry{
		{Scope: BotScope("bot-1"), Content: "最早的记忆：第一次被点名", CreatedAt: jul},
		{Scope: ChannelScope("ch1"), Content: "八月的记忆", CreatedAt: aug},
		{Scope: ChannelScope("ch1"), Content: "九月的记忆", CreatedAt: sep},
	}
	for _, e := range entries {
		if err := repo.Append(ctx, e); err != nil {
			t.Fatalf("append %q: %v", e.Content, err)
		}
	}
	return jul, aug, sep
}

func TestMemoryTool_SearchOrderOldest(t *testing.T) {
	repo := NewMemoryRepository()
	_, aug, sep := seedTimeSpanRepo(t, repo)
	tool := Tools(ToolConfig{Repo: repo})[0].Tool
	ctx := &llm.ToolExecContext{Context: context.Background()}

	// 显式 order=newest：最前面是九月（省略 query 且不给 order 会走深度召回，
	// 不再把「最近 N 条」当成默认）。
	got, err := tool.Execute(ctx, map[string]any{
		"action": "search", "scope_kind": "channel", "scope_id": "ch1", "limit": 10, "order": "newest",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	first := got.(map[string]any)["entries"].([]EntryResult)[0]
	if first.Content != "九月的记忆" {
		t.Fatalf("order=newest should be newest-first, got %q", first.Content)
	}

	// order=oldest：走到时间轴另一端，拿到八月。
	got, err = tool.Execute(ctx, map[string]any{
		"action": "search", "scope_kind": "channel", "scope_id": "ch1",
		"limit": 10, "order": "oldest",
	})
	if err != nil {
		t.Fatalf("search oldest: %v", err)
	}
	first = got.(map[string]any)["entries"].([]EntryResult)[0]
	if first.Content != "八月的记忆" {
		t.Fatalf("order=oldest should return the August entry first, got %q", first.Content)
	}
	if first.CreatedAt != aug.Format("2006-01-02 15:04") {
		t.Errorf("created_at: got %q, want %q", first.CreatedAt, aug.Format("2006-01-02 15:04"))
	}
	_ = sep
}

func TestMemoryTool_SearchTimeRange(t *testing.T) {
	repo := NewMemoryRepository()
	seedTimeSpanRepo(t, repo)
	tool := Tools(ToolConfig{Repo: repo})[0].Tool
	ctx := &llm.ToolExecContext{Context: context.Background()}

	got, err := tool.Execute(ctx, map[string]any{
		"action": "search", "scope_kind": "channel", "scope_id": "ch1",
		"limit": 10, "order": "oldest",
		"since": "2026-08-01", "until": "2026-08-31",
	})
	if err != nil {
		t.Fatalf("search range: %v", err)
	}
	m := got.(map[string]any)
	if m["count"].(int) != 1 {
		t.Fatalf("expected only the August entry, got %d", m["count"].(int))
	}
	if m["entries"].([]EntryResult)[0].Content != "八月的记忆" {
		t.Errorf("unexpected entry: %+v", m["entries"])
	}

	// 非法时间要显式报错，而不是被静默忽略（否则模型以为过滤生效了）。
	if _, err := tool.Execute(ctx, map[string]any{
		"action": "search", "scope_kind": "channel", "scope_id": "ch1", "since": "上个月",
	}); err == nil {
		t.Error("unparseable time should return an error")
	}
}

func TestMemoryTool_ScopeAllCoversOtherScopes(t *testing.T) {
	repo := NewMemoryRepository()
	jul, _, _ := seedTimeSpanRepo(t, repo)
	tool := Tools(ToolConfig{Repo: repo})[0].Tool
	ctx := &llm.ToolExecContext{Context: context.Background()}

	// 当前 channel 里最早的是八月；跨 scope 才能看到 bot scope 里那条七月。
	got, err := tool.Execute(ctx, map[string]any{
		"action": "search", "scope_kind": "all", "limit": 10, "order": "oldest",
	})
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	first := got.(map[string]any)["entries"].([]EntryResult)[0]
	if first.Content != "最早的记忆：第一次被点名" {
		t.Fatalf("scope_kind=all should surface the bot-scope July entry, got %q", first.Content)
	}
	if first.CreatedAt != jul.Format("2006-01-02 15:04") {
		t.Errorf("created_at: got %q, want %q", first.CreatedAt, jul.Format("2006-01-02 15:04"))
	}
}

func TestMemoryTool_CountReportsTimeSpan(t *testing.T) {
	repo := NewMemoryRepository()
	jul, _, sep := seedTimeSpanRepo(t, repo)
	tool := Tools(ToolConfig{Repo: repo})[0].Tool
	ctx := &llm.ToolExecContext{Context: context.Background()}

	// 单 scope
	got, err := tool.Execute(ctx, map[string]any{
		"action": "count", "scope_kind": "channel", "scope_id": "ch1",
	})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	m := got.(map[string]any)
	if m["count"].(int) != 2 {
		t.Fatalf("channel count: got %d, want 2", m["count"].(int))
	}
	if m["oldest_at"] == "" || m["newest_at"] == "" {
		t.Fatalf("count must report oldest_at/newest_at, got %+v", m)
	}

	// 全 scope：这是「我一共有多少记忆 / 最早是哪天」的权威答案
	got, err = tool.Execute(ctx, map[string]any{"action": "count", "scope_kind": "all"})
	if err != nil {
		t.Fatalf("count all: %v", err)
	}
	m = got.(map[string]any)
	if m["count"].(int) != 3 {
		t.Errorf("all-scope count: got %d, want 3", m["count"].(int))
	}
	if m["oldest_at"] != jul.Format("2006-01-02 15:04") {
		t.Errorf("all-scope oldest_at: got %v, want %v", m["oldest_at"], jul.Format("2006-01-02 15:04"))
	}
	if m["newest_at"] != sep.Format("2006-01-02 15:04") {
		t.Errorf("all-scope newest_at: got %v, want %v", m["newest_at"], sep.Format("2006-01-02 15:04"))
	}
}

func TestMemoryTool_RecentHonorsOrder(t *testing.T) {
	repo := NewMemoryRepository()
	_, aug, _ := seedTimeSpanRepo(t, repo)
	tool := Tools(ToolConfig{Repo: repo})[0].Tool
	ctx := &llm.ToolExecContext{Context: context.Background()}

	got, err := tool.Execute(ctx, map[string]any{
		"action": "recent", "scope_kind": "channel", "scope_id": "ch1",
		"limit": 5, "order": "oldest",
	})
	if err != nil {
		t.Fatalf("recent oldest: %v", err)
	}
	first := got.(map[string]any)["entries"].([]EntryResult)[0]
	if first.Content != "八月的记忆" {
		t.Fatalf("recent with order=oldest should return the August entry, got %q", first.Content)
	}
	_ = aug
}

// TestMemoryTool_ScopeAllIsReadOnly scope_kind="all" 只服务读取：
// 若允许写入，会凭空造出 kind="all" 的孤儿 scope，里面的记忆以后谁也搜不到。
func TestMemoryTool_ScopeAllIsReadOnly(t *testing.T) {
	repo := NewMemoryRepository()
	seedTimeSpanRepo(t, repo)
	tool := Tools(ToolConfig{Repo: repo})[0].Tool
	ctx := &llm.ToolExecContext{Context: context.Background()}

	for _, action := range []string{"add", "remove", "replace"} {
		args := map[string]any{"action": action, "scope_kind": "all", "content": "x", "old_text": "y"}
		if _, err := tool.Execute(ctx, args); err == nil {
			t.Errorf("action %q with scope_kind=all must be rejected", action)
		}
	}
	// 读取类操作不受影响
	if _, err := tool.Execute(ctx, map[string]any{"action": "count", "scope_kind": "all"}); err != nil {
		t.Errorf("count with scope_kind=all should work: %v", err)
	}
}

// TestMemoryTool_Search_MultiKeywordSpansWhitespace 多词 query 要跨空格命中。
// 整串 LIKE 在「演示环境 日志主题」对「演示环境日志主题为 demo_events」上是 0 条。
func TestMemoryTool_Search_MultiKeywordSpansWhitespace(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	target := "演示环境日志主题为 demo_events，可用日志通道查演示环境问题"
	for _, c := range []string{
		target,
		"测试数据库MySQL：192.0.2.10:3306，账号 appuser",
		"用户偏好深色主题",
	} {
		if err := repo.Append(ctx, Entry{Scope: ChannelScope("ch1"), Content: c}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	tool := Tools(ToolConfig{Repo: repo})[0].Tool
	got, err := tool.Execute(&llm.ToolExecContext{Context: ctx}, map[string]any{
		"action": "search", "query": "演示环境 日志主题", "scope_kind": "channel", "scope_id": "ch1",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	m := got.(map[string]any)
	if m["count"].(int) == 0 {
		t.Fatalf("multi-keyword query must match across whitespace; got 0 (scanned %v)", m)
	}
	entries := m["entries"].([]EntryResult)
	if entries[0].Content != target {
		t.Errorf("top hit should be the log-topic fact, got %q", entries[0].Content)
	}
	if entries[0].Relevance <= 0 {
		t.Errorf("search results must include the raw relevance score, got %v", entries[0].Relevance)
	}
	// 「测试数据库MySQL…」与 query 零交集，不该出现；
	// 「用户偏好深色主题」共享「主题」bigram，属合理弱相关，允许跟在后面。
	for _, e := range entries {
		if strings.Contains(e.Content, "MySQL") {
			t.Errorf("zero-overlap distractor leaked into results: %q", e.Content)
		}
	}
}

// TestMemoryTool_Search_NoQueryKeepsTimeOrder 显式 order、无 query 时不得引入相关性排序。
// 这条路径是「按时间翻记忆」。省略 order 的空 query 是深度召回，不在这里。
func TestMemoryTool_Search_NoQueryKeepsTimeOrder(t *testing.T) {
	repo := NewMemoryRepository()
	_, aug, sep := seedTimeSpanRepo(t, repo)
	tool := Tools(ToolConfig{Repo: repo})[0].Tool
	ctx := &llm.ToolExecContext{Context: context.Background()}

	got, err := tool.Execute(ctx, map[string]any{
		"action": "search", "scope_kind": "channel", "scope_id": "ch1", "limit": 2, "order": "newest",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	entries := got.(map[string]any)["entries"].([]EntryResult)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Content != "九月的记忆" || entries[1].Content != "八月的记忆" {
		t.Errorf("newest-first order broken: %+v", entries)
	}
	if entries[0].Relevance != 0 || entries[1].Relevance != 0 {
		t.Errorf("time-ordered search must not attach relevance scores, got %+v", entries)
	}
	if entries[0].CreatedAt != sep.Format("2006-01-02 15:04") {
		t.Errorf("created_at: got %q want %q", entries[0].CreatedAt, sep.Format("2006-01-02 15:04"))
	}
	_ = aug
}

// TestMemoryTool_Search_LongEntryIsTruncated 长条目要封顶并标注总长。
// 短查询可能命中一篇很长的笔记，真正要找的句子被埋在后面，模型读不完就当没有。
func TestMemoryTool_Search_LongEntryIsTruncated(t *testing.T) {
	repo := NewMemoryRepository()
	long := strings.Repeat("巡检细节占位 ", 300) + "关键连接串 host=192.0.2.10 port=3306"
	if err := repo.Append(context.Background(), Entry{Scope: ChannelScope("ch1"), Content: long}); err != nil {
		t.Fatalf("append: %v", err)
	}
	tool := Tools(ToolConfig{Repo: repo})[0].Tool

	got, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"action": "search", "query": "连接串", "scope_kind": "channel", "scope_id": "ch1",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	entries := got.(map[string]any)["entries"].([]EntryResult)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !strings.Contains(entries[0].Content, "truncated") {
		t.Errorf("long entry should be flagged as truncated, got %d chars", len([]rune(entries[0].Content)))
	}
	if !strings.Contains(entries[0].Content, "chars total") {
		t.Errorf("truncation notice should report the total length: %q", entries[0].Content[len(entries[0].Content)-80:])
	}
}

// TestMemoryTool_Search_MissSuggestsRetry 未命中时要给可行动的下一步，
// 否则模型只会把同一个词再搜一遍。
func TestMemoryTool_Search_MissSuggestsRetry(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.Append(context.Background(), Entry{Scope: ChannelScope("ch1"), Content: "用户偏好深色主题"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	tool := Tools(ToolConfig{Repo: repo})[0].Tool

	got, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"action": "search", "query": "数据库 mysql 连接", "scope_kind": "channel", "scope_id": "ch1",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	m := got.(map[string]any)
	if m["count"].(int) != 0 {
		t.Fatalf("expected 0 hits, got %v", m["count"])
	}
	msg, _ := m["message"].(string)
	if msg == "No matching memory found." {
		t.Error("bare miss message gives the model nothing to do next")
	}
	if !strings.Contains(msg, "recent") {
		t.Errorf("miss message should point at action \"recent\", got %q", msg)
	}
}

// 镜像必须收敛到至多一个前缀：模型会把已经带标签的正文再存一次。

func TestMirrorContent_IsIdempotent(t *testing.T) {
	const ch = "wecom:dm:demo-user"
	plain := "appuser 测试库连接：host=192.0.2.10 port=3306"
	once := MirrorContent(ch, plain)
	if once != "["+ch+"] "+plain {
		t.Fatalf("first mirror = %q", once)
	}
	// 二次镜像（源条目正文已带标签）不得再叠一层
	if twice := MirrorContent(ch, once); twice != once {
		t.Errorf("mirror is not idempotent: %q", twice)
	}
	// 已有的多叠前缀也要收敛（存量脏数据路径）
	if got := MirrorContent(ch, "["+ch+"] ["+ch+"] "+plain); got != once {
		t.Errorf("stacked prefixes not collapsed: %q", got)
	}
}

func TestMirrorContent_KeepsForeignChannelTag(t *testing.T) {
	// 别的频道的标签属于正文的一部分（说明这条记忆是从那边抄来的），不能剥
	got := MirrorContent("wecom:dm:A", "[wecom:dm:B] 来自 B 频道的事实")
	if got != "[wecom:dm:A] [wecom:dm:B] 来自 B 频道的事实" {
		t.Errorf("foreign tag must be preserved, got %q", got)
	}
}

func TestMirrorDedupeKey_StripsSourceTag(t *testing.T) {
	cases := []struct{ in, want string }{
		{"[wecom:dm:T1] appuser 测试库连接", "appuser 测试库连接"},
		{"appuser 测试库连接", "appuser 测试库连接"},
		{"[重要] 不是来源标签，不该剥", "[重要] 不是来源标签，不该剥"},
	}
	for _, c := range cases {
		if got := MirrorDedupeKey(c.in); got != c.want {
			t.Errorf("MirrorDedupeKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMirrorChannelMemoryToBot_DoesNotStack 走真实调用路径：同一条记忆镜像两次，
// bot scope 里仍只有一条、且只有一个前缀。
func TestMirrorChannelMemoryToBot_DoesNotStack(t *testing.T) {
	repo := NewMemoryRepository()
	cfg := ToolConfig{Repo: repo, BotID: "bot-1"}
	ctx := &llm.ToolExecContext{Context: context.Background()}
	ch := ChannelScope("wecom:dm:T1")
	entry := Entry{ID: "mem-1", Scope: ch, Content: "appuser 测试库连接：host=192.0.2.10"}

	mirrorChannelMemoryToBot(ctx, repo, cfg, entry.ID, entry)
	// 模拟「LLM 把带标签的镜像原文存回 channel scope」后再镜像一次
	reSaved := entry
	reSaved.Content = MirrorContent(ch.ID, entry.Content)
	mirrorChannelMemoryToBot(ctx, repo, cfg, reSaved.ID, reSaved)

	got, err := repo.Retrieve(ctx, Query{Scopes: []Scope{BotScope("bot-1")}, Limit: 100})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("mirror must reuse one row, got %d: %+v", len(got), got)
	}
	if got[0].Content != "[wecom:dm:T1] appuser 测试库连接：host=192.0.2.10" {
		t.Errorf("mirror content = %q", got[0].Content)
	}
}
