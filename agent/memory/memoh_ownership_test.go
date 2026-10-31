package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
)

func TestMemohImportRespectsKnownLegacyOwnershipBeyondCache(t *testing.T) {
	db := ownedTierDB(t)
	ctx := context.Background()
	old := time.Now().Add(-time.Hour)
	for _, test := range []struct{ id, owner string }{{"known", "A"}, {"ambiguous", ""}, {"other-bot", "B"}} {
		meta := "{}"
		if test.owner != "" {
			meta = fmt.Sprintf(`{"bot_id":%q}`, test.owner)
		}
		row := dao.TieredMemoryModel{ID: memohEntryID(test.id), Tier: int(Tier1LongTerm), ScopeKind: string(ScopeUser), ScopeID: "shared-user", Content: "old " + test.id, MetadataJSON: meta, CreatedAt: old}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Force the owned old import outside the bounded cache. Its persisted identity
	// must still suppress a duplicate, without claiming the other two legacy rows.
	filler := dao.TieredMemoryModel{ID: "recent", Tier: int(Tier1LongTerm), ScopeKind: string(ScopeUser), ScopeID: "shared-user", Content: "recent owned note", MetadataJSON: `{"bot_id":"A"}`, CreatedAt: time.Now()}
	if err := db.Create(&filler).Error; err != nil {
		t.Fatal(err)
	}
	store := NewTieredStoreForBot(map[MemoryTier]TierConfig{Tier1LongTerm: {MaxEntries: 1}}, db, "A")
	waitOwnedRepair(store)
	if err := store.Append(ctx, TieredEntry{Entry: Entry{ID: "latest", Scope: UserScope("shared-user"), Content: "latest owned note"}, Tier: Tier1LongTerm}); err != nil {
		t.Fatal(err)
	}
	if store.HasIDInTier(ctx, Tier1LongTerm, memohEntryID("known")) {
		t.Fatal("test must exercise persisted identity outside cache")
	}
	entries := []memohRawEntry{}
	for _, id := range []string{"known", "ambiguous", "other-bot"} {
		entries = append(entries, memohRawEntry{ID: id, Body: "This archive records useful deployment documentation and maintenance instructions."})
	}
	report, err := applyMemohImport(ctx, store, "A", entries, nil)
	if err != nil || report.Imported != 2 || report.Skipped != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	for _, id := range []string{"known", "ambiguous", "other-bot"} {
		var row dao.TieredMemoryModel
		if err := db.First(&row, "id = ?", memohEntryID(id)).Error; err != nil {
			t.Fatal(err)
		}
		if row.Content != "old "+id {
			t.Fatalf("legacy row changed: %+v", row)
		}
		if id == "ambiguous" && row.MetadataJSON != "{}" {
			t.Fatalf("ambiguous row silently claimed: %+v", row)
		}
	}
	report, err = applyMemohImport(ctx, store, "A", entries, nil)
	if err != nil || report.Imported != 0 || report.Skipped != 3 {
		t.Fatalf("persisted repeat report=%+v err=%v", report, err)
	}
	if _, err := applyMemohImport(ctx, store, "", entries, nil); err == nil {
		t.Fatal("missing target owner accepted")
	}
}
