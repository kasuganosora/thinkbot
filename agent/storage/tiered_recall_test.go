package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
)

func TestTieredRetrievers_BotOwnershipAndOriginalScopes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for _, tier := range []int{0, 1, 3} {
		rows := []dao.TieredMemoryModel{
			{ScopeKind: "user", ScopeID: "shared-user", MetadataJSON: `{"bot_id":"a"}`},
			{ScopeKind: "channel", ScopeID: "shared-channel", MetadataJSON: `{"bot_id":"a"}`},
			{ScopeKind: "bot", ScopeID: "a"}, // explicitly owned legacy row
			{ScopeKind: "user", ScopeID: "shared-user", MetadataJSON: `{"bot_id":"b"}`},
			{ScopeKind: "channel", ScopeID: "shared-channel", MetadataJSON: `{"bot_id":"b"}`},
			{ScopeKind: "user", ScopeID: "shared-user"}, // unattributed legacy
			{ScopeKind: "bot", ScopeID: "b", MetadataJSON: `{"bot_id":"a"}`},
			{ScopeKind: "bot", ScopeID: "a", MetadataJSON: `{"bot_id":"b"}`},
			{ScopeKind: "user", ScopeID: "shared-user", MetadataJSON: `{"nested":{"bot_id":"a"}}`},
		}
		for i := range rows {
			rows[i].ID = fmt.Sprintf("tier-%d-row-%d", tier, i)
			rows[i].Content = rows[i].ID
			rows[i].Tier = tier
			rows[i].CreatedAt = time.Now().Add(time.Duration(i) * time.Second)
		}
		if err := db.Create(&rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []*TieredL1Retriever{NewTieredL0Retriever(db, "a"), NewTieredL1Retriever(db, "a"), NewTieredProfileRetriever(db, "a")} {
		t.Run(fmt.Sprintf("tier-%d", r.tier), func(t *testing.T) {
			if r.BoundBotID() != "a" {
				t.Fatal("missing trusted owner")
			}
			for _, query := range []memory.Query{{}, {Scopes: []memory.Scope{memory.BotScope("a")}}, {Scopes: []memory.Scope{{Kind: memory.ScopeAll}}}} {
				got, err := r.Retrieve(ctx, query)
				if err != nil || len(got) != 3 {
					t.Fatalf("bot-wide retrieve: got %+v, err %v", got, err)
				}
				for _, e := range got {
					if e.ID != fmt.Sprintf("tier-%d-row-0", r.tier) && e.ID != fmt.Sprintf("tier-%d-row-1", r.tier) && e.ID != fmt.Sprintf("tier-%d-row-2", r.tier) {
						t.Fatalf("foreign, unattributed or wrong-tier entry: %+v", e)
					}
				}
			}
			got, err := r.Recent(ctx, memory.BotScope("a"), 1)
			if err != nil || len(got) != 1 || got[0].ID != fmt.Sprintf("tier-%d-row-2", r.tier) {
				t.Fatalf("ownership must precede limit: %+v, %v", got, err)
			}
			for _, scope := range []memory.Scope{memory.UserScope("shared-user"), memory.ChannelScope("shared-channel")} {
				got, err := r.Recent(ctx, scope, 100)
				if err != nil || len(got) != 1 || got[0].Scope != scope {
					t.Fatalf("original profile category must remain exact: %+v, %v", got, err)
				}
				if count, err := r.Count(ctx, scope); err != nil || count != 1 {
					t.Fatalf("scoped count = %d, %v", count, err)
				}
			}
			if count, err := r.Count(ctx, memory.BotScope("a")); err != nil || count != 3 {
				t.Fatalf("bot count = %d, %v", count, err)
			}
			got, err = r.Retrieve(ctx, memory.Query{Scopes: []memory.Scope{memory.BotScope("b")}})
			if err != nil || len(got) != 0 {
				t.Fatalf("foreign bot scope: %+v, %v", got, err)
			}
		})
	}
}

func TestMergedRetrieverRequiresConsistentTrustedOwner(t *testing.T) {
	db := testDB(t)
	a := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "a"})
	for _, sources := range [][]memory.Retriever{
		nil,
		{a, NewTieredL1Retriever(db)},
		{a, NewTieredL1Retriever(db, "b")},
		{a, &fakeRetriever{}},
	} {
		merged := NewMergedRetriever(sources...)
		if merged.BoundBotID() != "" {
			t.Fatal("mixed/unbound sources granted a bot identity")
		}
		if got, err := merged.FetchEntries(context.Background(), []string{"id"}); err == nil || len(got) != 0 {
			t.Fatalf("unbound hydration: %+v %v", got, err)
		}
	}
	if got := NewMergedRetriever(NewTieredProfileRetriever(db, "a"), NewTieredL1Retriever(db, "a"), a).BoundBotID(); got != "a" {
		t.Fatalf("consistent binding = %q", got)
	}
}

// fakeRetriever 是 memory.Retriever 的内存桩，按构造时给定的条目响应，
// 用于无 DB 验证 MergedRetriever 的合并 / 优先级 / 去重逻辑。
type fakeRetriever struct {
	byScope map[memory.Scope][]memory.Entry
}

func (f *fakeRetriever) Recent(_ context.Context, scope memory.Scope, _ int) ([]memory.Entry, error) {
	return f.byScope[scope], nil
}
func (f *fakeRetriever) Retrieve(_ context.Context, _ memory.Query) ([]memory.Entry, error) {
	var all []memory.Entry
	for _, es := range f.byScope {
		all = append(all, es...)
	}
	return all, nil
}
func (f *fakeRetriever) Count(_ context.Context, scope memory.Scope) (int, error) {
	return len(f.byScope[scope]), nil
}

func TestMergedRetriever_L1PriorityAndDedup(t *testing.T) {
	scope := memory.BotScope("bot-x")

	// L1 源：蒸馏知识（应与原始笔记去重后保留，且排在前面）
	l1 := &fakeRetriever{byScope: map[memory.Scope][]memory.Entry{
		scope: {
			{ID: "l1-1", Scope: scope, Content: "blogtalk 是科技/小说博主"},
			{ID: "l1-2", Scope: scope, Content: "thinkbot 用 Gin+fx+GORM 架构"},
		},
	}}
	// 原始笔记源：含一条与 L1 重复的内容（应被去重）和独有内容
	raw := &fakeRetriever{byScope: map[memory.Scope][]memory.Entry{
		scope: {
			{ID: "raw-1", Scope: scope, Content: "blogtalk 是科技/小说博主"}, // 与 l1-1 重复
			{ID: "raw-2", Scope: scope, Content: "今天天气不错"},
		},
	}}

	// L1 源排在前面 → 去重时优先保留 L1 条目
	merged := NewMergedRetriever(l1, raw)

	got, err := merged.Recent(context.Background(), scope, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expect 3 unique entries after dedup, got %d: %+v", len(got), got)
	}
	// 第一个必须是 L1 蒸馏条目（优先级），而非被重复的原始笔记
	if got[0].ID != "l1-1" {
		t.Fatalf("L1 entry should be prioritized first, got %s", got[0].ID)
	}
	// 重复内容只出现一次，且保留的是 L1 版本
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.ID] = true
	}
	if !ids["l1-1"] || ids["raw-1"] {
		t.Fatalf("dedup failed: l1-1 should survive, raw-1 (duplicate) should be dropped: %+v", got)
	}
}

func TestMergedRetriever_SingleSourceFailureNonFatal(t *testing.T) {
	scope := memory.BotScope("bot-y")
	good := &fakeRetriever{byScope: map[memory.Scope][]memory.Entry{
		scope: {{ID: "g1", Scope: scope, Content: "ok"}},
	}}
	bad := &fakeRetriever{byScope: map[memory.Scope][]memory.Entry{}} // 空源，模拟无数据

	merged := NewMergedRetriever(bad, good)
	got, err := merged.Recent(context.Background(), scope, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "g1" {
		t.Fatalf("healthy source should still return entries, got %+v", got)
	}
}

// TestTieredL1Retriever_Integration 用真实（临时文件）SQLite 验证：
// TieredL1Retriever 能从 tiered_memories(tier=1) 捞出 L1，且 MergedRetriever
// 把 L1 与 memory_entries 合并、L1 优先、内容去重。
func TestTieredL1Retriever_Integration(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	scope := memory.BotScope("bot-int")

	// 写入一条 L1 蒸馏记忆到 tiered_memories
	l1Content := "blogtalk 是科技/小说博主，开发了甄仁岛灵魂插件 MCP"
	if err := db.Create(&dao.TieredMemoryModel{
		ID:        "t1",
		Tier:      1,
		ScopeKind: string(scope.Kind),
		ScopeID:   scope.ID,
		Content:   l1Content,
		Category:  "fact",
		Source:    "dreaming",
		CreatedAt: time.Now(),
	}).Error; err != nil {
		t.Fatalf("insert tiered L1: %v", err)
	}

	// 写入一条原始笔记到 memory_entries（含与 L1 重复的内容 + 独有内容）
	repo := NewSQLiteRepository(db)
	if err := repo.Append(ctx, memory.Entry{
		Scope:    scope,
		Content:  l1Content, // 故意与 L1 重复，验证去重
		Category: "fact",
		Source:   "note",
	}); err != nil {
		t.Fatalf("append memory_entries: %v", err)
	}
	if err := repo.Append(ctx, memory.Entry{
		Scope:    scope,
		Content:  "今天在调 recall stage 的复合召回",
		Category: "event",
		Source:   "note",
	}); err != nil {
		t.Fatalf("append memory_entries: %v", err)
	}

	// 1) TieredL1Retriever 单独应能捞出 L1
	l1 := NewTieredL1Retriever(db)
	l1Entries, err := l1.Recent(ctx, scope, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(l1Entries) != 1 || l1Entries[0].Content != l1Content {
		t.Fatalf("TieredL1Retriever should return exactly the L1 row, got %+v", l1Entries)
	}

	// 2) MergedRetriever 合并：L1 优先 + 去重（重复内容只留 L1 版）
	merged := NewMergedRetriever(l1, repo)
	got, err := merged.Recent(ctx, scope, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expect 2 unique entries (L1 + 1 distinct raw), got %d: %+v", len(got), got)
	}
	if got[0].Content != l1Content {
		t.Fatalf("L1 must be first after merge, got %q", got[0].Content)
	}
	// 确认重复的 raw 版被丢弃（只保留 L1 版）
	for _, e := range got {
		if e.Content == l1Content && e.Source != "dreaming" {
			t.Fatalf("duplicate raw entry should be deduped in favor of L1, got %+v", e)
		}
	}
}

func TestTieredProfileRetriever_ReadsL3NotL1(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	scope := memory.UserScope("u-l3")
	now := time.Now()

	if err := db.Create(&dao.TieredMemoryModel{
		ID:        "l1-row",
		Tier:      1,
		ScopeKind: string(scope.Kind),
		ScopeID:   scope.ID,
		Content:   "L1 fact",
		Category:  "fact",
		Source:    "dreaming",
		CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("insert L1: %v", err)
	}
	if err := db.Create(&dao.TieredMemoryModel{
		ID:        "l3-row",
		Tier:      3,
		ScopeKind: string(scope.Kind),
		ScopeID:   scope.ID,
		Content:   "用户偏理性",
		Category:  "trait",
		Source:    "profiler",
		CreatedAt: now,
	}).Error; err != nil {
		t.Fatalf("insert L3: %v", err)
	}

	l1 := NewTieredL1Retriever(db)
	l3 := NewTieredProfileRetriever(db)

	l1Got, err := l1.Recent(ctx, scope, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(l1Got) != 1 || l1Got[0].Content != "L1 fact" {
		t.Fatalf("L1 retriever should see only L1, got %+v", l1Got)
	}

	l3Got, err := l3.Recent(ctx, scope, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(l3Got) != 1 || l3Got[0].Content != "用户偏理性" {
		t.Fatalf("L3 retriever should see only L3, got %+v", l3Got)
	}

	retrieved, err := l3.Retrieve(ctx, memory.Query{Scopes: []memory.Scope{scope}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(retrieved) != 1 || retrieved[0].ID != "l3-row" {
		t.Fatalf("L3 Retrieve should use tier=3, got %+v", retrieved)
	}

	n, err := l3.Count(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("L3 Count want 1, got %d", n)
	}

	merged := NewMergedRetriever(l3, l1)
	all, err := merged.Recent(ctx, scope, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Content != "用户偏理性" {
		t.Fatalf("merged should put L3 first, got %+v", all)
	}
}

// TestMergedRetriever_DedupesChannelMirror 镜像副本必须被去重。
//
// 线上同一条事实会在候选池里出现两次：channel scope 的原文，和 bot scope 里
// 正文为 "[<channel-id>] <原文>" 的镜像（xch: 前缀）。逐字精确去重认不出它们
// 是同一件事，于是召回预算被副本吃掉一半（36 条候选里就有一批是这种副本）。
func TestMergedRetriever_DedupesChannelMirror(t *testing.T) {
	ctx := context.Background()
	bot := memory.BotScope("bot-x")
	ch := memory.ChannelScope("wecom:dm:T1")

	botSrc := &fakeRetriever{byScope: map[memory.Scope][]memory.Entry{
		bot: {
			{ID: "xch:mem-1", Scope: bot, Content: "[wecom:dm:T1] appuser 测试库连接 host=192.0.2.10"},
		},
	}}
	chSrc := &fakeRetriever{byScope: map[memory.Scope][]memory.Entry{
		ch: {
			{ID: "mem-1", Scope: ch, Content: "appuser 测试库连接 host=192.0.2.10"},
		},
	}}

	merged := NewMergedRetriever(botSrc, chSrc)
	got, err := merged.Recent(ctx, bot, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("mirror copy should be deduped, got %d: %+v", len(got), got)
	}

	all, err := merged.Retrieve(ctx, memory.Query{Scopes: []memory.Scope{bot}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("Retrieve should dedupe the mirror copy too, got %d: %+v", len(all), all)
	}
}
