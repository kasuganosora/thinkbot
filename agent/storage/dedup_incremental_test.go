package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

func ent(id, content string) memory.Entry { return memory.Entry{ID: id, Content: content} }

func TestBuildDedupGroups(t *testing.T) {
	seeds := []memory.Entry{ent("a", "x"), ent("b", "x"), ent("c", "x"), ent("d", "x")}
	nb := map[string][]memory.Entry{
		"a": {ent("b", ""), ent("n1", ""), ent("xch:m", "")}, // b is also a seed, mirror skipped
		"b": {ent("a", "")},
		"c": {ent("n1", "")}, // n1 already used by a's group
		"d": {},
	}
	groups, lonely := buildDedupGroups(seeds, func(e memory.Entry) []memory.Entry { return nb[e.ID] }, 10)
	if len(groups) != 1 || len(groups[0]) != 3 || groups[0][0].ID != "a" || groups[0][1].ID != "b" || groups[0][2].ID != "n1" {
		t.Fatalf("groups = %+v", groups)
	}
	if fmt.Sprint(lonely) != "[c d]" {
		t.Fatalf("lonely = %v", lonely)
	}
	// group size cap
	many := []memory.Entry{}
	for i := 0; i < 20; i++ {
		many = append(many, ent(fmt.Sprintf("n%d", i), ""))
	}
	groups, _ = buildDedupGroups([]memory.Entry{ent("s", "")}, func(memory.Entry) []memory.Entry { return many }, 10)
	if len(groups[0]) != 10 {
		t.Fatalf("group must be capped at 10, got %d", len(groups[0]))
	}
}

func TestFilterNeighbours(t *testing.T) {
	seed := ent("s", "用户使用Go语言开发后端服务")
	cands := []memory.Entry{
		ent("s", seed.Content),
		ent("near", "用户使用Go语言开发后端"),
		ent("far", "今天天气晴朗适合散步"),
		ent("xch:near", "用户使用Go语言开发后端"),
	}
	got := filterNeighbours(seed, cands, DefaultDedupMinSimilarity, 5)
	if len(got) != 1 || got[0].ID != "near" {
		t.Fatalf("neighbours = %+v", got)
	}
}

func TestBuildFallbackGroups(t *testing.T) {
	var seeds []memory.Entry
	for i := 0; i < 15; i++ {
		seeds = append(seeds, ent(fmt.Sprintf("s%d", i), ""))
	}
	recent := []memory.Entry{ent("r1", ""), ent("r2", ""), ent("r3", "")}
	groups, lonely := buildFallbackGroups(seeds, recent, 10)
	if len(groups) != 3 || len(lonely) != 0 {
		t.Fatalf("groups=%d lonely=%v", len(groups), lonely)
	}
	for _, g := range groups {
		if len(g) > 10 || len(g) < 2 {
			t.Fatalf("bad group size %d", len(g))
		}
	}
	// single seed, no recent → lonely, no LLM call
	groups, lonely = buildFallbackGroups([]memory.Entry{ent("only", "")}, nil, 10)
	if len(groups) != 0 || len(lonely) != 1 {
		t.Fatalf("groups=%v lonely=%v", groups, lonely)
	}
}

// Legacy memory_entries without dedup_checked_at must be upgraded in place.
func TestMigrate_AddsDedupColumnAndState(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "legacy.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE memory_entries (id TEXT PRIMARY KEY, scope_kind TEXT NOT NULL, scope_id TEXT,
		content TEXT NOT NULL, category TEXT, source TEXT, importance REAL DEFAULT 0, metadata_json TEXT,
		created_at DATETIME NOT NULL, last_accessed_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO memory_entries (id, scope_kind, scope_id, content, created_at) VALUES ('old','channel','c','legacy',?)`, time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if err := dao.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := dao.Migrate(db); err != nil { // idempotent
		t.Fatalf("second migrate: %v", err)
	}
	var cnt int64
	db.Raw("SELECT COUNT(*) FROM pragma_table_info('memory_entries') WHERE name = 'dedup_checked_at'").Scan(&cnt)
	if cnt != 1 {
		t.Fatal("dedup_checked_at column missing")
	}
	db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_memory_entries_dedup'").Scan(&cnt)
	if cnt != 1 {
		t.Fatal("idx_memory_entries_dedup missing")
	}
	repo := NewSQLiteRepository(db)
	// Backfill: existing entries are marked checked (no first-run storm)…
	if n, _ := repo.CountUncheckedDedup(context.Background(), memory.ChannelScope("c")); n != 0 {
		t.Fatalf("legacy entry must be backfilled as checked, got %d unchecked", n)
	}
	var content string
	db.Raw("SELECT content FROM memory_entries WHERE id='old'").Scan(&content)
	if content != "legacy" {
		t.Fatalf("backfill must not rewrite content: %q", content)
	}
	// …and the backfill runs once: entries written later stay unchecked.
	db.Exec(`INSERT INTO memory_entries (id, scope_kind, scope_id, content, created_at) VALUES ('new','channel','c','fresh',?)`, time.Now())
	if err := dao.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if n, _ := repo.CountUncheckedDedup(context.Background(), memory.ChannelScope("c")); n != 1 {
		t.Fatalf("entries written after the backfill must stay unchecked, got %d", n)
	}
	repo.storeDedupRun(context.Background(), "channel:c", time.Now())
}

func TestDedupCooldownPersisted(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	r1 := NewSQLiteRepository(db)
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	r1.storeDedupRun(ctx, "channel:x", at)
	r2 := NewSQLiteRepository(db) // "restart"
	got, ok := r2.lastDedupRun(ctx, "channel:x")
	if !ok || !got.Equal(at) {
		t.Fatalf("cooldown not persisted: %v %v", got, ok)
	}
}

func TestDedupTriggerOnNewEntriesAndCooldown(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	scope := memory.ChannelScope("trig")
	spy := &spyCompactor{}
	repo := NewSQLiteRepository(db, SQLiteRepositoryConfig{Compactor: spy, DedupTriggerNew: 3})
	for i := 0; i < 2; i++ {
		_ = repo.db.Create(&dao.EntryModel{ID: fmt.Sprintf("t%d", i), ScopeKind: "channel", ScopeID: "trig", Content: "x", CreatedAt: time.Now()}).Error
	}
	repo.maybeCompact(ctx, scope)
	if spy.calls != 0 {
		t.Fatal("below trigger (no window) must not compact")
	}
	_ = repo.db.Create(&dao.EntryModel{ID: "t2", ScopeKind: "channel", ScopeID: "trig", Content: "x", CreatedAt: time.Now()}).Error
	repo.maybeCompact(ctx, scope)
	if spy.calls != 1 {
		t.Fatalf("trigger on 3 unchecked: calls=%d", spy.calls)
	}
	// persisted cooldown blocks a fresh repository too
	repo2 := NewSQLiteRepository(db, SQLiteRepositoryConfig{Compactor: spy, DedupTriggerNew: 3})
	repo2.maybeCompact(ctx, scope)
	if spy.calls != 1 {
		t.Fatalf("cooldown must survive restart: calls=%d", spy.calls)
	}
}

func TestPrecheckedSourcesSkipDedup(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := NewSQLiteRepository(db)
	scope := memory.BotScope("b1")
	_ = repo.Append(ctx, memory.Entry{Scope: scope, Content: "merged", Source: "compactor"})
	_ = repo.Append(ctx, memory.Entry{ID: "xch:mem-1", Scope: scope, Content: "[c] mirror", Source: "note"})
	_ = repo.Append(ctx, memory.Entry{Scope: scope, Content: "fresh", Source: "note"})
	if n, _ := repo.CountUncheckedDedup(ctx, scope); n != 1 {
		t.Fatalf("only the fresh entry must be unchecked, got %d", n)
	}
}

// TestDedupTokenMeasurement compares, on a synthetic scope, the LLM calls and
// prompt size of the old algorithm (all active entries, batches of 50, every
// pass) with the incremental one. Mock LLM: answers [] (nothing to merge).
// Run with -v to see the numbers.
func TestDedupTokenMeasurement(t *testing.T) {
	const existing, fresh, passes = 45, 5, 3
	topics := []string{"用户使用Go语言开发后端，项目仓库 thinkbot", "用户喜欢猫，家里养了一只橘猫", "用户每天早上八点跑步五公里",
		"用户在上海工作，时区 UTC+8", "用户偏好简洁的中文回复", "用户的服务器部署在 Docker 上", "用户关注 GLM 模型的 token 成本",
		"用户周末会去爬山", "用户正在学习日语 N2", "用户使用 Misskey 和 Telegram"}
	content := func(i int) string {
		return fmt.Sprintf("%s（第 %d 条记录，补充细节：编号 R-%04d，备注说明这是一条较长的长期记忆，用于模拟真实条目的长度与内容密度）", topics[i%len(topics)], i, i)
	}
	// ---- old: every pass sends every active entry (batches of 50) ----
	var oldCalls, oldChars, oldTok int
	for p := 0; p < passes; p++ {
		var inputs []memory.ClusterInput
		for i := 0; i < existing+fresh; i++ {
			inputs = append(inputs, memory.ClusterInput{ID: fmt.Sprintf("mem-%d", i), Category: "fact", Content: content(i)})
		}
		for i := 0; i < len(inputs); i += 50 {
			end := i + 50
			if end > len(inputs) {
				end = len(inputs)
			}
			prov := &sizeRecordingProvider{}
			_, _ = memory.ClusterMerge(context.Background(), prov, &llm.Model{ID: "mock"}, memory.DefaultCompactionConfig().SystemPrompt, inputs[i:end], 0, nil)
			oldCalls += len(prov.sizes)
			for k, c := range prov.chars {
				oldChars += c
				oldTok += prov.tokens[k]
			}
		}
	}
	// ---- new: existing entries already checked, 5 fresh unchecked ----
	db := testDB(t)
	ctx := context.Background()
	scope := memory.ChannelScope("measure")
	repo := NewSQLiteRepository(db)
	now := time.Now()
	for i := 0; i < existing+fresh; i++ {
		m := dao.EntryModel{ID: fmt.Sprintf("mem-%d", i), ScopeKind: "channel", ScopeID: "measure", Content: content(i),
			Category: "fact", Source: "note", CreatedAt: now.Add(time.Duration(i) * time.Second)}
		if i < existing {
			m.DedupCheckedAt = &now
		}
		if err := db.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
		if repo.Vec().Enabled() {
			repo.Vec().Upsert(scope.Key(), m.ID, m.Content)
		}
	}
	prov := &sizeRecordingProvider{}
	c := NewSQLiteCompactor(SQLiteCompactorConfig{Provider: prov, Model: &llm.Model{ID: "mock"}}, zap.NewNop().Sugar())
	c.SetRepository(repo)
	for p := 0; p < passes; p++ {
		_ = c.CompactScope(ctx, scope)
	}
	newChars, newTok := 0, 0
	for k, x := range prov.chars {
		newChars += x
		newTok += prov.tokens[k]
	}
	t.Logf("dedup measurement (%d active, %d new, %d passes, vec=%v): old calls=%d prompt_chars=%d est_tokens=%d | new calls=%d prompt_chars=%d est_tokens=%d group_sizes=%v",
		existing+fresh, fresh, passes, repo.Vec().Enabled(), oldCalls, oldChars, oldTok, len(prov.sizes), newChars, newTok, prov.sizes)
	if newChars >= oldChars {
		t.Fatalf("incremental dedup must send less: old=%d new=%d", oldChars, newChars)
	}
}

// scriptedClusterProvider returns a fixed answer.
type scriptedClusterProvider struct {
	answer string
	calls  int
	prompt string
}

func (p *scriptedClusterProvider) Name() string { return "mock" }
func (p *scriptedClusterProvider) DoGenerate(_ context.Context, gp llm.GenerateParams) (*llm.GenerateResult, error) {
	p.calls++
	p.prompt = extractPromptText(gp.Messages[0])
	return &llm.GenerateResult{Text: p.answer, FinishReason: llm.FinishReasonStop}, nil
}
func (p *scriptedClusterProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

func TestDedupOversizedAndRejectedMerges(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	scope := memory.ChannelScope("caps")
	repo := NewSQLiteRepository(db)
	long := ""
	for i := 0; i < 120; i++ {
		long += "很长的记忆内容"
	}
	mk := func(id, content, source string) {
		if err := db.Create(&dao.EntryModel{ID: id, ScopeKind: "channel", ScopeID: "caps", Content: content, Source: source, CreatedAt: time.Now()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk("big", long, "note")
	mk("a", "用户使用Go语言开发后端服务", "note")
	mk("b", "用户使用Go语言开发后端", "note")
	mk("cmp", "旧的合并结果", "compactor") // never a seed
	if n, _ := repo.CountUncheckedDedup(ctx, scope); n != 3 {
		t.Fatalf("compactor output must not count as unchecked seed: %d", n)
	}
	// merge result ≈ source a → no-op
	prov := &scriptedClusterProvider{answer: `[{"merged_content":"用户使用Go语言开发后端服务","category":"fact","source_ids":["a","b"]}]`}
	c := NewSQLiteCompactor(SQLiteCompactorConfig{Provider: prov, Model: &llm.Model{ID: "mock"}}, zap.NewNop().Sugar())
	c.SetRepository(repo)
	if err := c.CompactScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if prov.calls != 1 || containsID(prov.prompt, "big") {
		t.Fatalf("oversized entry must not be sent: calls=%d prompt=%s", prov.calls, prov.prompt)
	}
	active, _ := repo.GetAllActive(ctx, scope)
	if len(active) != 4 {
		t.Fatalf("no-op merge must keep all sources, active=%d", len(active))
	}
	var bigContent string
	db.Raw("SELECT content FROM memory_entries WHERE id='big'").Scan(&bigContent)
	if bigContent != long {
		t.Fatal("oversized entry must stay untouched")
	}
	if n, _ := repo.CountUncheckedDedup(ctx, scope); n != 0 {
		t.Fatalf("everything must be marked checked, %d left", n)
	}
	// too long merge result → rejected
	r := c.mergeRejectReason(memory.ClusterResult{MergedContent: long, SourceIDs: []string{"a"}}, map[string]memory.Entry{})
	if r != "too_long" {
		t.Fatalf("reason=%q", r)
	}
}

func containsID(prompt, id string) bool {
	return len(prompt) > 0 && (stringsContains(prompt, "["+id+"]"))
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
