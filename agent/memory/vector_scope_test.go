package memory

import (
	"bytes"
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/dao"
)

func TestVecBackfillMigratesScopeWithoutReplacingLiveEmbedding(t *testing.T) {
	gdb := openVecDB(t)
	idx := OpenVecIndex(gdb)
	if !idx.Enabled() {
		t.Skip("sqlite-vec unavailable")
	}
	idx.Upsert("channel:c", "legacy", "newest live content")
	rowBefore, embBefore := vecRowidAndEmbedding(t, gdb, "legacy")
	st, err := idx.BackfillMissing(context.Background(), "memory_entries", []VecItem{{ScopeKey: "bot:a", EntryID: "legacy", Content: "old startup snapshot"}}, nil)
	if err != nil || st.Written != 1 {
		t.Fatalf("migration: %+v %v", st, err)
	}
	rowAfter, embAfter := vecRowidAndEmbedding(t, gdb, "legacy")
	if rowBefore != rowAfter || !bytes.Equal(embBefore, embAfter) {
		t.Fatal("scope migration replaced live embedding")
	}
	if hits := idx.Search("bot:a", "newest live content", 5); len(hits) != 1 || hits[0].EntryID != "legacy" {
		t.Fatalf("migrated vector unavailable: %+v", hits)
	}
	st, err = idx.BackfillMissing(context.Background(), "memory_entries", []VecItem{{ScopeKey: "bot:a", EntryID: "legacy", Content: "old startup snapshot"}}, nil)
	if err != nil || st.Present != 1 || st.Written != 0 {
		t.Fatalf("repeat migration: %+v %v", st, err)
	}
}

func TestTieredOwnedVectorsKeepRawClassificationAndOtherTableVectors(t *testing.T) {
	gdb := openVecDB(t)
	if err := dao.Migrate(gdb); err != nil {
		t.Fatal(err)
	}
	store := NewTieredStoreWithDB(nil, gdb)
	ctx := context.Background()
	scope := UserScope("same-user")
	entry := TieredEntry{Entry: Entry{ID: "tiered-a", Scope: scope, Content: "fictional orchard amber", Metadata: map[string]any{"bot_id": "a"}}, Tier: Tier1LongTerm}
	if err := store.Append(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if hits := store.Vec().Search("bot:a", "orchard amber", 10); len(hits) != 1 || hits[0].EntryID != "tiered-a" {
		t.Fatalf("new tiered vector not bot-wide: %+v", hits)
	}
	if neighbors := store.VecNeighbors(scope, "orchard amber", 10); len(neighbors) != 1 || neighbors[0].Scope != scope {
		t.Fatalf("exact-category neighbors broke: %+v", neighbors)
	}
	store.Vec().Upsert("bot:a", "other-table", "another fictional note")
	if _, err := RebuildBotVectors(ctx, gdb, "a", store, nil); err != nil {
		t.Fatal(err)
	}
	hits := store.Vec().Search("bot:a", "orchard amber", 10)
	if len(hits) != 2 {
		t.Fatalf("tiered rebuild erased another table's vector: %+v", hits)
	}
	var row dao.TieredMemoryModel
	if err := gdb.First(&row, "id = ?", "tiered-a").Error; err != nil || row.ScopeKind != "user" || row.ScopeID != "same-user" {
		t.Fatalf("classification changed: %+v %v", row, err)
	}
}
