package storage

import (
	"context"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
)

func TestSQLiteRepositoryBotIsolationAcrossSharedDatabase(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	a := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "a"})
	b := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "b"})
	raw := memory.ChannelScope("shared-channel")
	for _, pair := range []struct {
		repo *SQLiteRepository
		id   string
	}{{a, "a-new"}, {b, "b-new"}} {
		if err := pair.repo.Append(ctx, memory.Entry{ID: pair.id, Scope: raw, Content: pair.id}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	for _, row := range []dao.EntryModel{
		{ID: "a-legacy", ScopeKind: "user", ScopeID: "same-user", Content: "owned legacy", MetadataJSON: `{"bot_id": "a"}`, CreatedAt: now},
		{ID: "a-bot", ScopeKind: "bot", ScopeID: "a", Content: "explicit owner", CreatedAt: now},
		{ID: "b-bot", ScopeKind: "bot", ScopeID: "b", Content: "other explicit owner", CreatedAt: now},
		{ID: "unknown", ScopeKind: "channel", ScopeID: "shared-channel", Content: "unattributed", CreatedAt: now},
		{ID: "conflicting", ScopeKind: "bot", ScopeID: "b", Content: "conflict", MetadataJSON: `{"bot_id":"a"}`, CreatedAt: now},
		{ID: "nested", ScopeKind: "channel", ScopeID: "shared-channel", Content: "not owner", MetadataJSON: `{"nested":{"bot_id":"a"}}`, CreatedAt: now},
		{ID: "invalid-json", ScopeKind: "channel", ScopeID: "shared-channel", Content: "broken", MetadataJSON: `not-json`, CreatedAt: now},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	assertIDs := func(got []memory.Entry, err error, want ...string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, e := range got {
			ids[e.ID] = true
		}
		if len(ids) != len(want) {
			t.Fatalf("got ids %v, want %v", ids, want)
		}
		for _, id := range want {
			if !ids[id] {
				t.Fatalf("missing %s in %v", id, ids)
			}
		}
	}
	got, err := a.Retrieve(ctx, memory.Query{Limit: 100})
	assertIDs(got, err, "a-new", "a-legacy", "a-bot")
	got, err = a.Recent(ctx, memory.BotScope("a"), 100)
	assertIDs(got, err, "a-new", "a-legacy", "a-bot")
	got, err = a.Retrieve(ctx, memory.Query{Scopes: []memory.Scope{memory.BotScope("b")}, Limit: 100})
	assertIDs(got, err)
	got, err = a.Recent(ctx, raw, 100)
	assertIDs(got, err, "a-new")
	got, err = a.FetchEntries(ctx, []string{"a-new", "a-legacy", "b-new", "b-bot", "unknown", "conflicting"})
	assertIDs(got, err, "a-new", "a-legacy")
	count, err := a.Count(ctx, memory.BotScope("a"))
	if err != nil || count != 3 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	stats, err := a.MemoryStats(ctx, nil)
	if err != nil || stats.Total != 3 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	if a.Metrics().TotalEntries != 3 {
		t.Fatalf("metrics include other bots: %+v", a.Metrics())
	}
	if err := a.Delete(ctx, memory.BotScope("a"), "b-new"); err != nil {
		t.Fatal(err)
	}
	if err := a.Replace(ctx, memory.BotScope("a"), "b-new", memory.Entry{ID: "injected", Content: "bad"}); err == nil {
		t.Fatal("cross-bot replacement accepted")
	}
	got, err = b.FetchEntries(ctx, []string{"b-new"})
	assertIDs(got, err, "b-new")
	got, err = a.FetchEntries(ctx, []string{"injected"})
	assertIDs(got, err)
	if err := a.Replace(ctx, memory.BotScope("a"), "a-legacy", memory.Entry{ID: "a-legacy", Content: "wrong scope"}); err == nil {
		t.Fatal("shared read alias must not reclassify a user memory")
	}
	if err := a.Replace(ctx, memory.UserScope("same-user"), "a-legacy", memory.Entry{ID: "a-legacy", Content: "updated legacy"}); err != nil {
		t.Fatal(err)
	}
	got, err = a.FetchEntries(ctx, []string{"a-legacy"})
	assertIDs(got, err, "a-legacy")
	if got[0].Scope != memory.UserScope("same-user") || got[0].Metadata["bot_id"] != "a" {
		t.Fatal("replacement did not preserve scope and trusted owner")
	}
	if err := a.Delete(ctx, memory.BotScope("a"), "a-new"); err != nil {
		t.Fatal(err)
	}
	got, err = a.FetchEntries(ctx, []string{"a-new"})
	assertIDs(got, err, "a-new")
	if err := a.Delete(ctx, raw, "a-new"); err != nil {
		t.Fatal(err)
	}
	got, err = a.FetchEntries(ctx, []string{"a-new"})
	assertIDs(got, err)
	if err := a.Clear(ctx, memory.BotScope("a")); err != nil {
		t.Fatal(err)
	}
	got, err = a.Retrieve(ctx, memory.Query{Limit: 100})
	assertIDs(got, err, "a-legacy")
	if err := a.Clear(ctx, memory.UserScope("same-user")); err != nil {
		t.Fatal(err)
	}
	got, err = a.Retrieve(ctx, memory.Query{Limit: 100})
	assertIDs(got, err)
	got, err = b.Retrieve(ctx, memory.Query{Limit: 100})
	assertIDs(got, err, "b-new", "b-bot")
	var unknown int64
	db.Model(&dao.EntryModel{}).Where("id = ?", "unknown").Count(&unknown)
	if unknown != 1 {
		t.Fatal("clear removed unowned legacy entry")
	}
}

func TestSQLiteRepositoryRejectsForeignOwnersAndIsolatesMaintenance(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	a := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "a"})
	b := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "b"})
	raw := memory.ChannelScope("shared-channel")
	for _, entry := range []memory.Entry{
		{Scope: memory.BotScope("b"), Content: "foreign scope"},
		{Scope: raw, Content: "foreign metadata", Metadata: map[string]any{"bot_id": "b"}},
	} {
		if err := a.Append(ctx, entry); err == nil {
			t.Fatal("foreign write accepted")
		}
	}
	for _, pair := range []struct {
		repo *SQLiteRepository
		id   string
	}{{a, "a-note"}, {b, "b-note"}} {
		if err := pair.repo.Append(ctx, memory.Entry{ID: pair.id, Scope: raw, Content: "note"}); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := a.CountUncheckedDedup(ctx, raw); err != nil || count != 1 {
		t.Fatalf("dedup count=%d err=%v", count, err)
	}
	if n := a.MarkDedupChecked(ctx, raw, []string{"a-note", "b-note"}); n != 1 {
		t.Fatalf("marked %d entries", n)
	}
	if count, err := b.CountUncheckedDedup(ctx, raw); err != nil || count != 1 {
		t.Fatalf("other bot dedup state changed: %d %v", count, err)
	}
	if a.ArchiveByID(ctx, raw, "b-note") {
		t.Fatal("foreign archive accepted")
	}
	if n := a.MarkCompactSkipped(ctx, raw, []memory.Entry{{ID: "b-note"}}, "test"); n != 0 {
		t.Fatal("foreign skip accepted")
	}
	a.deleteEvicted(ctx, raw, []string{"b-note"}, "test")
	got, err := b.FetchEntries(ctx, []string{"b-note"})
	if err != nil || len(got) != 1 {
		t.Fatal("foreign eviction deleted entry")
	}
	a.storeDedupRun(ctx, raw.Key(), time.Now())
	if _, ok := b.lastDedupRun(ctx, raw.Key()); ok {
		t.Fatal("dedup cooldown crossed bots")
	}
	if _, err := a.DryRunDedupGroups(ctx, DedupDryRunOptions{Reindex: true}); err == nil {
		t.Fatal("bot repository may rebuild global vector index")
	}
}

type isolatedCompactorProbe struct {
	repo    *SQLiteRepository
	entries []memory.Entry
}

func (p *isolatedCompactorProbe) CompactScope(ctx context.Context, scope memory.Scope) error {
	entries, err := p.repo.GetAllActive(ctx, scope)
	p.entries = entries
	return err
}

func TestSQLiteRepositoryCompactionReadsOnlyOwnedScope(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	probe := &isolatedCompactorProbe{}
	repo := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "a", Compactor: probe})
	probe.repo = repo
	for _, row := range []dao.EntryModel{
		{ID: "owned", ScopeKind: "user", ScopeID: "same-user", Content: "one", MetadataJSON: `{"bot_id":"a"}`},
		{ID: "foreign", ScopeKind: "user", ScopeID: "same-user", Content: "other", MetadataJSON: `{"bot_id":"b"}`},
		{ID: "owned-bot", ScopeKind: "bot", ScopeID: "a", Content: "bot", MetadataJSON: `{"bot_id":"a"}`},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.CompactScope(ctx, memory.BotScope("a")); err != nil {
		t.Fatal(err)
	}
	if len(probe.entries) != 1 || probe.entries[0].ID != "owned-bot" {
		t.Fatalf("compactor escaped exact bot scope: %+v", probe.entries)
	}
	got, err := repo.ActiveByIDs(ctx, memory.BotScope("a"), []string{"owned", "foreign", "owned-bot"})
	if err != nil || len(got) != 1 || got[0].ID != "owned-bot" {
		t.Fatalf("dedup IDs escaped owner: %+v %v", got, err)
	}
	got, err = repo.UncheckedDedup(ctx, memory.BotScope("a"), 100)
	if err != nil || len(got) != 1 || got[0].ID != "owned-bot" {
		t.Fatalf("unchecked escaped owner: %+v %v", got, err)
	}
	if chars, err := repo.totalChars(ctx, memory.BotScope("a")); err != nil || chars != 3 {
		t.Fatalf("totalChars=%d %v", chars, err)
	}
	if marked := repo.MarkDedupChecked(ctx, memory.BotScope("a"), []string{"owned", "foreign", "owned-bot"}); marked != 1 {
		t.Fatalf("marked %d entries outside exact scope", marked)
	}
	got, err = repo.RecentCheckedDedup(ctx, memory.BotScope("a"), 100)
	if err != nil || len(got) != 1 || got[0].ID != "owned-bot" {
		t.Fatalf("recent checked escaped owner: %+v %v", got, err)
	}
	if n, err := repo.CountUncheckedDedup(ctx, memory.UserScope("same-user")); err != nil || n != 1 {
		t.Fatalf("bot maintenance touched user scope: %d %v", n, err)
	}
}
