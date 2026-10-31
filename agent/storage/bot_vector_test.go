package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/db"
	gormlogger "gorm.io/gorm/logger"
)

func TestProductionMergedContextBotVectorHydration(t *testing.T) {
	gdb, err := db.OpenSQLiteWithLogger(filepath.Join(t.TempDir(), "vec.db"), gormlogger.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := dao.Migrate(gdb); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	repo := NewSQLiteRepository(gdb, SQLiteRepositoryConfig{BotID: "a"})
	if !repo.Vec().Enabled() {
		t.Skip("sqlite-vec unavailable")
	}
	// Exact production source composition and priority.
	l3 := NewTieredProfileRetriever(gdb, "a")
	l1 := NewTieredL1Retriever(gdb, "a")
	merged := NewMergedRetriever(l3, l1, repo)
	rows := []dao.TieredMemoryModel{
		{ID: "l3-owned", Tier: 3, ScopeKind: "user", ScopeID: "u", Content: "fictional amber orchard profile", MetadataJSON: `{"bot_id":"a"}`, CreatedAt: now.Add(-time.Hour)},
		{ID: "l1-owned", Tier: 1, ScopeKind: "channel", ScopeID: "c", Content: "fictional amber orchard project", MetadataJSON: `{"bot_id":"a"}`, CreatedAt: now.Add(-time.Hour)},
		{ID: "l3-recent", Tier: 3, ScopeKind: "user", ScopeID: "u", Content: "unrelated newest profile", MetadataJSON: `{"bot_id":"a"}`, CreatedAt: now},
		{ID: "l1-recent", Tier: 1, ScopeKind: "channel", ScopeID: "c", Content: "unrelated newest event", MetadataJSON: `{"bot_id":"a"}`, CreatedAt: now},
		{ID: "l0-excluded", Tier: 0, ScopeKind: "user", ScopeID: "u", Content: "fictional amber orchard L0", MetadataJSON: `{"bot_id":"a"}`, CreatedAt: now},
		{ID: "foreign", Tier: 3, ScopeKind: "user", ScopeID: "u", Content: "fictional amber orchard foreign", MetadataJSON: `{"bot_id":"b"}`, CreatedAt: now},
		{ID: "unknown", Tier: 1, ScopeKind: "channel", ScopeID: "c", Content: "fictional amber orchard unknown", CreatedAt: now},
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(ctx, memory.Entry{ID: "raw-owned", Scope: memory.ChannelScope("c"), Content: "fictional amber orchard note"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runAppVecBackfill(ctx, gdb, repo.Vec(), nil); err != nil {
		t.Fatal(err)
	}
	// Even incorrect index ownership cannot import foreign/unattributed hits.
	for _, row := range rows {
		repo.Vec().Upsert("bot:a", row.ID, row.Content)
	}
	ids := []string{"l3-owned", "l1-owned", "raw-owned", "l0-excluded", "foreign", "unknown"}
	got, err := merged.FetchEntries(ctx, ids)
	if err != nil || len(got) != 3 {
		t.Fatalf("merged hydration: %+v %v", got, err)
	}
	if got, err := l3.FetchEntries(ctx, ids); err != nil || len(got) != 1 || got[0].ID != "l3-owned" {
		t.Fatalf("tier hydration: %+v %v", got, err)
	}
	for _, tc := range []struct {
		scope memory.Scope
		want  []string
	}{
		{memory.BotScope("a"), []string{"l3-owned", "l1-owned", "raw-owned"}},
		{memory.UserScope("u"), []string{"l3-owned"}},
	} {
		mgr := memory.NewContextManager(merged, memory.NewContextBuilder(), nil, nil, memory.ContextManagerConfig{RecentLimit: 1, RelevantLimit: 1})
		mgr.UseVec(repo.Vec())
		// Reversed words defeat tiered SQL LIKE; old tiered rows are outside
		// Recent(1), so they can only arrive through merged vector hydration.
		result, err := mgr.AssembleContextScopes(ctx, []memory.Scope{tc.scope}, "orchard amber")
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, e := range result.RecentEntries {
			seen[e.ID] = true
		}
		for _, id := range tc.want {
			if !seen[id] {
				t.Fatalf("scope=%s missing %s, got %+v text=%s", tc.scope.Key(), id, result.RecentEntries, result.ContextText)
			}
		}
		for _, id := range []string{"l0-excluded", "foreign", "unknown"} {
			if seen[id] || strings.Contains(result.ContextText, id) {
				t.Fatalf("excluded hit injected: %s", id)
			}
		}
		if tc.scope.Kind == memory.ScopeUser && (seen["l1-owned"] || seen["raw-owned"]) {
			t.Fatal("exact user classification widened")
		}
	}
}

func TestSQLiteRepositoryBotVectorsPreserveSharingAndFilters(t *testing.T) {
	gdb, err := db.OpenSQLiteWithLogger(filepath.Join(t.TempDir(), "vec.db"), gormlogger.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := dao.Migrate(gdb); err != nil {
		t.Fatal(err)
	}
	a := NewSQLiteRepository(gdb, SQLiteRepositoryConfig{BotID: "a"})
	b := NewSQLiteRepository(gdb, SQLiteRepositoryConfig{BotID: "b"})
	if !a.Vec().Enabled() {
		t.Skip("sqlite-vec not available")
	}
	ctx := context.Background()
	now := time.Now()
	for _, item := range []struct {
		repo  *SQLiteRepository
		entry memory.Entry
	}{
		{a, memory.Entry{ID: "a-user", Scope: memory.UserScope("u"), Content: "fictional amber orchard", Category: "fact", Importance: 0.9, CreatedAt: now}},
		{a, memory.Entry{ID: "a-channel", Scope: memory.ChannelScope("c"), Content: "fictional amber orchard", Category: "event", Importance: 0.5, CreatedAt: now.Add(-time.Hour)}},
		{b, memory.Entry{ID: "b-user", Scope: memory.UserScope("u"), Content: "fictional amber orchard", Category: "fact", Importance: 0.9, CreatedAt: now}},
	} {
		if err := item.repo.Append(ctx, item.entry); err != nil {
			t.Fatal(err)
		}
	}
	// The reversed query has no substring match, so this exercises real vectors.
	check := func(q memory.Query, want ...string) {
		t.Helper()
		q.Text = "orchard amber"
		q.Limit = 20
		got, err := a.Retrieve(ctx, q)
		if err != nil || len(got) != len(want) {
			t.Fatalf("got %+v, err %v, want %v", got, err, want)
		}
		ids := map[string]bool{}
		for _, e := range got {
			ids[e.ID] = true
			if e.Scope.Kind == memory.ScopeBot {
				t.Fatalf("original category overwritten: %+v", e)
			}
		}
		for _, id := range want {
			if !ids[id] {
				t.Fatalf("missing %s in %v", id, ids)
			}
		}
	}
	check(memory.Query{Scopes: []memory.Scope{memory.BotScope("a")}}, "a-user", "a-channel")
	check(memory.Query{}, "a-user", "a-channel")
	check(memory.Query{Scopes: []memory.Scope{memory.UserScope("u")}}, "a-user")
	check(memory.Query{Category: "fact", MinImportance: 0.8, Since: now.Add(-time.Minute)}, "a-user")
	check(memory.Query{Until: now.Add(-time.Minute)}, "a-channel")
	check(memory.Query{Scopes: []memory.Scope{memory.BotScope("b")}})
	// A corrupted/stale index entry cannot bypass trusted SQL ownership.
	a.Vec().Upsert(memory.BotScope("a").Key(), "b-user", "fictional amber orchard")
	check(memory.Query{}, "a-user", "a-channel")
	updated := memory.Entry{ID: "a-user", Content: "fictional amber orchard changed", Category: "fact", CreatedAt: now}
	if err := a.Replace(ctx, memory.UserScope("u"), "a-user", updated); err != nil {
		t.Fatal(err)
	}
	check(memory.Query{Scopes: []memory.Scope{memory.UserScope("u")}}, "a-user")
	// Recreate an old raw-scope vector and let the normal app startup migrate it.
	a.Vec().Upsert(memory.ChannelScope("c").Key(), "a-channel", "fictional amber orchard")
	if _, err := runAppVecBackfill(ctx, gdb, a.Vec(), nil); err != nil {
		t.Fatal(err)
	}
	check(memory.Query{}, "a-user", "a-channel")
	if hits := a.Vec().Search(memory.ChannelScope("c").Key(), "orchard amber", 10); len(hits) != 0 {
		t.Fatalf("old bucket remains: %+v", hits)
	}
	if hits := b.Vec().Search(memory.BotScope("b").Key(), "orchard amber", 10); len(hits) != 1 || hits[0].EntryID != "b-user" {
		t.Fatalf("other bot vector not preserved: %+v", hits)
	}
}

func TestSQLiteRepositoryBotVectorsCompactorAndReindex(t *testing.T) {
	gdb, err := db.OpenSQLiteWithLogger(filepath.Join(t.TempDir(), "vec.db"), gormlogger.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := dao.Migrate(gdb); err != nil {
		t.Fatal(err)
	}
	a := NewSQLiteRepository(gdb, SQLiteRepositoryConfig{BotID: "a"})
	b := NewSQLiteRepository(gdb, SQLiteRepositoryConfig{BotID: "b"})
	if !a.Vec().Enabled() {
		t.Skip("sqlite-vec unavailable")
	}
	ctx := context.Background()
	scope := memory.ChannelScope("shared")
	for _, item := range []struct {
		repo  *SQLiteRepository
		id    string
		scope memory.Scope
	}{
		{a, "seed", scope}, {a, "neighbor", scope}, {a, "another-scope", memory.UserScope("u")}, {b, "other-bot", scope},
	} {
		if err := item.repo.Append(ctx, memory.Entry{ID: item.id, Scope: item.scope, Content: "fictional orchard amber"}); err != nil {
			t.Fatal(err)
		}
	}
	seed, err := a.FetchEntries(ctx, []string{"seed"})
	if err != nil || len(seed) != 1 {
		t.Fatalf("seed=%+v err=%v", seed, err)
	}
	c := &SQLiteCompactor{repo: a, sel: dedupSelectConfig{topK: 10, minSim: 0.1, maxMergedChars: 1000}}
	check := func() {
		t.Helper()
		got := c.vecNeighbours(ctx, scope, a.Vec(), seed[0])
		if len(got) != 1 || got[0].ID != "neighbor" {
			t.Fatalf("neighbors=%+v", got)
		}
	}
	check()
	maintenance := NewSQLiteRepository(gdb)
	if _, err := maintenance.DryRunDedupGroups(ctx, DedupDryRunOptions{Reindex: true}); err != nil {
		t.Fatal(err)
	}
	check()
}
