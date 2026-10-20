package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	gormlogger "gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/db"
)

// Needs sqlite-vec (THINKBOT_SQLITE_VEC); skipped otherwise.
func TestEmbedSchemeChangeRebuildsMemoryEntryVectors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vec.db")
	gdb, err := db.OpenSQLiteWithLogger(path, gormlogger.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := dao.Migrate(gdb); err != nil {
		t.Fatal(err)
	}
	repo := NewSQLiteRepository(gdb)
	if !repo.Vec().Enabled() {
		t.Skip("sqlite-vec not available")
	}
	ctx := context.Background()
	scope := memory.ChannelScope("test:chan")
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := repo.Append(ctx, memory.Entry{ID: id, Scope: scope, Content: "fictional note " + id}); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		var n int
		gdb.Raw("SELECT count(*) FROM memory_vec").Scan(&n)
		return n
	}
	if count() != 3 {
		t.Fatalf("vectors after store = %d", count())
	}
	// Simulate vectors from an older scheme: drop one row, stamp an old version.
	gdb.Exec("DELETE FROM memory_vec WHERE entry_id = 'e2'")
	gdb.Exec("UPDATE memory_vec_meta SET value = 'old' WHERE key = 'embed_version'")
	repo2 := NewSQLiteRepository(gdb) // reopen: scheme mismatch clears memory_vec
	if count() != 0 {
		t.Fatalf("stale vectors must be cleared, got %d", count())
	}
	already, written, err := repo2.BackfillVec(ctx)
	if err != nil || already != 0 || written != 3 || count() != 3 {
		t.Fatalf("backfill already=%d written=%d err=%v rows=%d", already, written, err, count())
	}
	if a, w, _ := repo2.BackfillVec(ctx); a != 3 || w != 0 {
		t.Fatalf("second backfill must be a no-op: already=%d written=%d", a, w)
	}
}

// App-level backfill fills both tables with no bot running (stopped bots).
func TestAppVecBackfillWithoutBots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vec.db")
	gdb, err := db.OpenSQLiteWithLogger(path, gormlogger.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := dao.Migrate(gdb); err != nil {
		t.Fatal(err)
	}
	idx := memory.OpenVecIndex(gdb)
	if !idx.Enabled() {
		t.Skip("sqlite-vec not available")
	}
	now := time.Now()
	gdb.Create(&dao.EntryModel{ID: "m1", ScopeKind: "channel", ScopeID: "c", Source: "note", Content: "fictional entry one", CreatedAt: now})
	gdb.Create(&dao.TieredMemoryModel{ID: "t1", Tier: 1, ScopeKind: "channel", ScopeID: "c", Content: "fictional fact one", CreatedAt: now})
	gdb.Create(&dao.TieredMemoryModel{ID: "t2", Tier: 0, ScopeKind: "channel", ScopeID: "c", Content: "expired note", CreatedAt: now, ExpiresAt: now.Add(-time.Hour)})
	st, err := runAppVecBackfill(context.Background(), gdb, idx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st[0].Missing != 1 || st[0].Written != 1 || st[1].Missing != 1 || st[1].Written != 1 {
		t.Fatalf("stats tiered=%+v entries=%+v", st[0], st[1])
	}
	var n int
	gdb.Raw("SELECT count(*) FROM memory_vec").Scan(&n)
	if n != 2 {
		t.Fatalf("memory_vec rows = %d, want 2 (expired tiered row skipped)", n)
	}
	st, _ = runAppVecBackfill(context.Background(), gdb, idx, nil)
	if st[0].Missing != 0 || st[1].Missing != 0 {
		t.Fatalf("second run must find nothing missing: %+v", st)
	}
}
