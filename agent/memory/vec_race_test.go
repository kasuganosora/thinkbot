package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

func vecDupCount(t *testing.T, gdb *gorm.DB) (rows, distinct int) {
	t.Helper()
	if err := gdb.Raw("SELECT count(*) FROM memory_vec").Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := gdb.Raw("SELECT entry_id FROM memory_vec").Scan(&ids).Error; err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	return rows, len(seen)
}

// The live bug: app-level backfill and a bot's TieredStore backfill ran at the
// same time on separate VecIndex instances and wrote duplicate entry_ids.
func TestConcurrentAppAndTieredBackfillNoDuplicates(t *testing.T) {
	gdb := openVecDB(t)
	if err := gdb.AutoMigrate(&dao.TieredMemoryModel{}); err != nil {
		t.Fatal(err)
	}
	scope := ChannelScope("race")
	const n = 300
	for i := 0; i < n; i++ {
		insertTieredRow(t, gdb, fmt.Sprintf("t-%03d", i), scope, fmt.Sprintf("fictional fact number %d about kettles", i))
	}
	if !OpenVecIndex(gdb).Enabled() {
		t.Skip("sqlite-vec not available")
	}
	for round := 0; round < 3; round++ {
		if err := gdb.Exec("DELETE FROM memory_vec").Error; err != nil {
			t.Fatal(err)
		}
		appIdx := OpenVecIndex(gdb)
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			if _, err := BackfillTieredVectors(context.Background(), gdb, appIdx, nil); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			s := NewTieredStoreWithDB(nil, gdb) // starts its own backfill
			<-s.vecBackfillDone
		}()
		go func() { // plus direct upserts from a third instance
			defer wg.Done()
			other := OpenVecIndex(gdb)
			for i := 0; i < n; i += 3 {
				other.Upsert(scope.Key(), fmt.Sprintf("t-%03d", i), fmt.Sprintf("fictional fact number %d about kettles", i))
			}
		}()
		wg.Wait()
		rows, distinct := vecDupCount(t, gdb)
		if rows != n || distinct != n {
			t.Fatalf("round %d: rows=%d distinct=%d, want %d each", round, rows, distinct, n)
		}
	}
}

func TestVecDedupRowsSelfHeal(t *testing.T) {
	gdb := openVecDB(t)
	idx := OpenVecIndex(gdb)
	if !idx.Enabled() {
		t.Skip("sqlite-vec not available")
	}
	ins := func(id, content string) {
		if err := gdb.Exec("INSERT INTO memory_vec(embedding, scope_key, entry_id) VALUES (?, ?, ?)",
			hashEmbed(content), "channel:a", id).Error; err != nil {
			t.Fatal(err)
		}
	}
	ins("a", "fictional a")
	ins("a", "fictional a")
	ins("a", "fictional a")
	ins("b", "fictional b")
	ins("b", "fictional b")
	ins("c", "fictional c")
	removed, err := idx.DedupRows()
	if err != nil || removed != 3 {
		t.Fatalf("removed=%d err=%v, want 3", removed, err)
	}
	if rows, distinct := vecDupCount(t, gdb); rows != 3 || distinct != 3 {
		t.Fatalf("after heal rows=%d distinct=%d", rows, distinct)
	}
	if hits := idx.Search("channel:a", "fictional b", 3); len(hits) == 0 {
		t.Fatal("kept rows must stay searchable")
	}
	if removed, _ := idx.DedupRows(); removed != 0 {
		t.Fatalf("second heal removed %d", removed)
	}
}

// OpenVecIndex heals duplicates once per DB at startup.
func TestOpenVecIndexHealsDuplicatesOnStartup(t *testing.T) {
	gdb := openVecDB(t)
	if err := gdb.Exec(vecCreateSQL(vecTableName, true)).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec("CREATE TABLE " + vecMetaTable + " (key TEXT PRIMARY KEY, value TEXT NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec("INSERT INTO "+vecMetaTable+" VALUES ('embed_version', ?)", embedVersion).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := gdb.Exec("INSERT INTO memory_vec(embedding, scope_key, entry_id) VALUES (?, ?, ?)",
			hashEmbed("fictional dup"), "channel:a", "dup").Error; err != nil {
			t.Fatal(err)
		}
	}
	OpenVecIndex(gdb)
	if rows, distinct := vecDupCount(t, gdb); rows != 1 || distinct != 1 {
		t.Fatalf("startup heal: rows=%d distinct=%d", rows, distinct)
	}
}
