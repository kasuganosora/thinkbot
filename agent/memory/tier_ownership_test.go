package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func ownedTierDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "memory.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&dao.TieredMemoryModel{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func waitOwnedRepair(s *TieredStore) {
	if s.pendingRepairDone != nil {
		<-s.pendingRepairDone
	}
}

func TestBoundTieredStoreLoadsAndClearsOnlyItsOwner(t *testing.T) {
	db := ownedTierDB(t)
	ctx := context.Background()
	raw := NewTieredStoreWithDB(nil, db)
	waitOwnedRepair(raw)
	for _, owner := range []string{"a", "b", ""} {
		e := TieredEntry{Entry: Entry{ID: "entry-" + owner, Scope: ChannelScope("same-group"), Content: "fact for " + owner}, Tier: Tier1LongTerm}
		if owner != "" {
			e.Metadata = map[string]any{"bot_id": owner}
		}
		if err := raw.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	a := NewTieredStoreForBot(nil, db, "a")
	waitOwnedRepair(a)
	b := NewTieredStoreForBot(nil, db, "b")
	waitOwnedRepair(b)
	got, _ := a.Retrieve(ctx, Tier1LongTerm, nil, 100)
	if len(got) != 1 || got[0].ID != "entry-a" {
		t.Fatalf("cross-owner background input: %+v", got)
	}
	if err := a.ClearTier(ctx, Tier1LongTerm); err != nil {
		t.Fatal(err)
	}
	var rows []dao.TieredMemoryModel
	db.Find(&rows)
	if len(rows) != 2 {
		t.Fatalf("ClearTier deleted foreign rows: %+v", rows)
	}
	for _, row := range rows {
		if row.ID == "entry-a" {
			t.Fatal("own row not cleared")
		}
	}
	// A second bot's live cache and subsequent reload both retain its row.
	got, _ = b.Retrieve(ctx, Tier1LongTerm, nil, 100)
	if len(got) != 1 || got[0].ID != "entry-b" {
		t.Fatal("other bot cache changed")
	}
	b2 := NewTieredStoreForBot(nil, db, "b")
	waitOwnedRepair(b2)
	got, _ = b2.Retrieve(ctx, Tier1LongTerm, nil, 100)
	if len(got) != 1 || got[0].ID != "entry-b" {
		t.Fatal("other bot persistence changed")
	}
}

func TestBoundTieredStoreDerivedWritesAndIDsKeepOwnership(t *testing.T) {
	db := ownedTierDB(t)
	ctx := context.Background()
	scope := ChannelScope("shared")
	a := NewTieredStoreForBot(nil, db, "a")
	waitOwnedRepair(a)
	b := NewTieredStoreForBot(nil, db, "b")
	waitOwnedRepair(b)
	if err := b.Append(ctx, TieredEntry{Entry: Entry{ID: "foreign", Scope: scope, Content: "b private fact"}, Tier: Tier1LongTerm}); err != nil {
		t.Fatal(err)
	}
	if err := a.Append(ctx, TieredEntry{Entry: Entry{ID: "foreign", Scope: scope, Content: "overwrite"}, Tier: Tier1LongTerm}); err == nil {
		t.Fatal("foreign ID accepted")
	}
	if err := a.Append(ctx, TieredEntry{Entry: Entry{Scope: scope, Content: "claimed b", Metadata: map[string]any{"bot_id": "b"}}, Tier: Tier1LongTerm}); err == nil {
		t.Fatal("foreign metadata accepted")
	}
	if err := a.Append(ctx, TieredEntry{Entry: Entry{ID: "derived", Scope: scope, Content: "derived fact"}, Tier: Tier1LongTerm}); err != nil {
		t.Fatal(err)
	}
	if err := a.Replace(ctx, Tier1LongTerm, scope, "derived", TieredEntry{Entry: Entry{ID: "replacement", Content: "new derived fact"}}); err != nil {
		t.Fatal(err)
	}
	var row dao.TieredMemoryModel
	if err := OwnedMemoryRows(db, "a").Where("id = ?", "replacement").First(&row).Error; err != nil {
		t.Fatal("derived ownership lost:", err)
	}
	if err := a.Clear(ctx, Tier1LongTerm, scope); err != nil {
		t.Fatal(err)
	}
	var foreign dao.TieredMemoryModel
	if err := db.Where("id = ?", "foreign").First(&foreign).Error; err != nil {
		t.Fatal("scope clear removed another bot")
	}
}

func TestBoundTieredStoreRepairAndTombstonesStayWithinOwner(t *testing.T) {
	db := ownedTierDB(t)
	ctx := context.Background()
	scope := ChannelScope("shared")
	for _, owner := range []string{"a", "b"} {
		te := TieredEntry{Entry: Entry{ID: "pending-" + owner, Scope: scope, Content: "user stated fact", Metadata: map[string]any{"bot_id": owner, "status": StatusPending, "source_entries": []any{map[string]any{"speaker": "user"}}}, CreatedAt: time.Now()}, Tier: Tier1LongTerm}
		row := tieredEntryToModel(te)
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	a := NewTieredStoreForBot(nil, db, "a")
	waitOwnedRepair(a)
	var foreign dao.TieredMemoryModel
	db.Where("id = ?", "pending-b").First(&foreign)
	if StatusOf(modelToTiedEntry(foreign).Entry) != StatusPending {
		t.Fatal("startup repair changed foreign bot")
	}
	if err := a.Delete(ctx, Tier1LongTerm, scope, "pending-a"); err != nil {
		t.Fatal(err)
	}
	b := NewTieredStoreForBot(nil, db, "b")
	waitOwnedRepair(b)
	if b.Forgotten(scope, "user stated fact") {
		t.Fatal("another bot's deletion suppresses this bot")
	}
	if !a.Forgotten(scope, "user stated fact") {
		t.Fatal("own tombstone lost")
	}
	empty := NewTieredStoreForBot(nil, db, "")
	waitOwnedRepair(empty)
	got, _ := empty.Retrieve(ctx, Tier1LongTerm, nil, 100)
	if len(got) != 0 {
		t.Fatal("missing owner loaded rows")
	}
	if err := empty.Append(ctx, TieredEntry{Entry: Entry{Scope: scope, Content: "no owner"}}); err == nil {
		t.Fatal("missing owner wrote a row")
	}
}

func TestBoundTieredStoreConflictAndExpiryNeverMutateForeignRows(t *testing.T) {
	db := ownedTierDB(t)
	ctx := context.Background()
	scope := ChannelScope("shared")
	b := NewTieredStoreForBot(nil, db, "b")
	waitOwnedRepair(b)
	if err := b.Append(ctx, TieredEntry{Entry: Entry{ID: "foreign", Scope: scope, Content: "private b"}, Tier: Tier1LongTerm}); err != nil {
		t.Fatal(err)
	}
	a := NewTieredStoreForBot(nil, db, "a")
	waitOwnedRepair(a)
	// Exercise the atomic conflict check, as if the foreign ID was inserted
	// concurrently after the earlier input check.
	collision := tieredEntryToModel(TieredEntry{Entry: Entry{ID: "foreign", Scope: scope, Content: "overwrite", Metadata: map[string]any{"bot_id": "a"}}, Tier: Tier1LongTerm})
	if err := a.persistOwnedEntry(ctx, &collision); err == nil {
		t.Fatal("conflicting upsert reported success")
	}
	if err := a.Append(ctx, TieredEntry{Entry: Entry{ID: "expired-a", Scope: scope, Content: "old"}, Tier: Tier0Working, ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	a.GC(ctx)
	var rows []dao.TieredMemoryModel
	db.Find(&rows)
	if len(rows) != 1 || rows[0].ID != "foreign" || rows[0].Content != "private b" {
		t.Fatalf("foreign row changed: %+v", rows)
	}
}
