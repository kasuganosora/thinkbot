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

	// 极小 window：MemoryBudget=1 token → 3 字符预算，超过即触发。
	//
	// 构造时必须把三个扣减项都压到极小，否则预算为 0、压缩永不触发
	// （本测试曾因此空转 3 秒后失败）：
	//   - OutputReserve 默认 128000（GLM 1M 模型口径），远大于这里给的
	//     MaxContextTokens=10000 → totalAvailable 为负 → MemoryBudget() 直接返回 0。
	//   - ReservedTokens 默认 2000，同样会把小额 MaxContextTokens 吃穿。
	//   - MemoryBudgetRatio 必须显式给值：MemoryBudget() 先算 totalAvailable*ratio，
	//     再用 MaxMemoryTokens 做「上限」截断，ratio 为零时压根到不了上限分支。
	win := memory.NewWindow(memory.WindowConfig{
		MaxContextTokens:  10000,
		ReservedTokens:    1,
		OutputReserve:     1,
		MemoryBudgetRatio: 0.2,
		MaxMemoryTokens:   1, // 上限 1 token → 3 字符
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

// TestSQLiteCompactor_CompactScopeBatchesWhenExceedsMaxInput 是「记忆压缩积压」bug 的回归测试。
// 根因：活跃条目超过 MaxInputEntries 时，旧代码 entries[:MaxInputEntries] 截断丢弃超额部分，
// 而 GetAllActive 按 created_at ASC 排序，导致较新记忆永远轮不到压缩、活跃数只增不减
// （生产日志见 "sqlite_compactor exceeds MaxInputEntries"，大 scope 从 343 涨到 367 且持续）。
// 修复：按 MaxInputEntries 切块循环处理全部活跃条目。本测试验证这一点。
func TestSQLiteCompactor_CompactScopeBatchesWhenExceedsMaxInput(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	scope := memory.ChannelScope("batch-compact")

	// spy 防 Append 的 maybeCompact 干扰，且 nil-safe
	spy := &spyCompactor{}
	repo := NewSQLiteRepository(db, SQLiteRepositoryConfig{Compactor: spy})

	const n = 120
	for i := 0; i < n; i++ {
		if err := repo.Append(ctx, memory.Entry{
			Scope:    scope,
			Content:  fmt.Sprintf("记忆条目编号 %d 用于验证分批压缩", i),
			Category: "fact",
			Source:   "conversation",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	before, err := repo.GetAllActive(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != n {
		t.Fatalf("expected %d active before compaction, got %d", n, len(before))
	}

	provider := &mockClusterProvider{}
	const maxInput = 50
	compactor := NewSQLiteCompactor(SQLiteCompactorConfig{
		Provider:        provider,
		Model:           &llm.Model{ID: "mock"},
		MaxInputEntries: maxInput,
		MinClusterSize:  2,
	}, zap.NewNop().Sugar())
	compactor.SetRepository(repo)

	if err := compactor.CompactScope(ctx, scope); err != nil {
		t.Fatalf("CompactScope: %v", err)
	}

	// 回归核心：超过 MaxInputEntries(50) 应分批处理全部 120 条，
	// 分批次数 = ceil(120/50) = 3。旧逻辑只调 1 次 LLM（处理前 50 条）。
	if provider.calls != 3 {
		t.Errorf("expected 3 batch LLM calls (ceil(120/50)), got %d", provider.calls)
	}

	active, err := repo.GetAllActive(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	// 旧逻辑(entries[:50])：只处理前 50 -> 1 条 merged + 后 70 活跃残留 ≈71。
	// 新逻辑：120 条源全归档 + 3 条 merged，活跃应≈3。
	if len(active) >= 50 {
		t.Errorf("regression: %d active remain (expected ~3); old truncation bug would leave ~71", len(active))
	}
	var merged int
	for _, e := range active {
		if e.Source == "compactor" {
			merged++
		}
	}
	if merged != 3 {
		t.Errorf("expected 3 merged entries (one per batch), got %d", merged)
	}
}

// TestSQLiteRepository_ZeroBudgetWarns 覆盖「预算算出 0」的告警路径。
// 该状态危害在于完全静默：maybeCompact 每次提前返回，记忆只增不减，
// 故障表现为存储缓慢膨胀而非报错，极难定位。
// 构造方式与 TestSQLiteRepository_AppendTriggersCompaction 相反——
// 这里刻意不覆盖 OutputReserve 默认值，使可用额度为负、预算归零。
func TestSQLiteRepository_ZeroBudgetWarns(t *testing.T) {
	db := testDB(t)
	scope := memory.ChannelScope("zero-budget")

	win := memory.NewWindow(memory.WindowConfig{
		MaxContextTokens: 10000, // OutputReserve 保持默认 128000 → 可用额度为负
	})
	if win.MemoryBudget() != 0 {
		t.Fatalf("构造前提不成立：MemoryBudget() = %d, want 0", win.MemoryBudget())
	}

	spy := &spyCompactor{}
	repo := NewSQLiteRepository(db, SQLiteRepositoryConfig{Window: win, Compactor: spy})

	repo.maybeCompact(context.Background(), scope)

	if !repo.budgetWarned.Load() {
		t.Error("预算为 0 时应置起告警标志")
	}
	spy.mu.Lock()
	calls := spy.calls
	spy.mu.Unlock()
	if calls != 0 {
		t.Errorf("预算为 0 时不应触发压缩，实际调用 %d 次", calls)
	}
}

// TestSQLiteRepository_NilWindowNoWarn window 未注入是设计上的「不限制」，
// 不是配置错误，不应污染日志。
func TestSQLiteRepository_NilWindowNoWarn(t *testing.T) {
	db := testDB(t)
	repo := NewSQLiteRepository(db, SQLiteRepositoryConfig{}) // 不注入 window

	repo.maybeCompact(context.Background(), memory.ChannelScope("no-window"))

	if repo.budgetWarned.Load() {
		t.Error("window 未注入属设计上的不限制，不应置起告警标志")
	}
}
