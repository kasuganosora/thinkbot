package memory

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

func insertTieredRow(t *testing.T, gdb *gorm.DB, id string, scope Scope, content string) {
	t.Helper()
	now := time.Now()
	row := dao.TieredMemoryModel{
		ID:             id,
		Tier:           int(Tier1LongTerm),
		ScopeKind:      string(scope.Kind),
		ScopeID:        scope.ID,
		Content:        content,
		CreatedAt:      now,
		LastAccessedAt: now,
	}
	if err := gdb.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
}

func vecRowidAndEmbedding(t *testing.T, gdb *gorm.DB, entryID string) (int64, []byte) {
	t.Helper()
	var rowid int64
	var emb []byte
	row := gdb.Raw("SELECT rowid, embedding FROM memory_vec WHERE entry_id = ?", entryID).Row()
	if err := row.Scan(&rowid, &emb); err != nil {
		t.Fatalf("read vec row %s: %v", entryID, err)
	}
	return rowid, emb
}

func TestVecBackfillSkipsExistingAndInsertsMissing(t *testing.T) {
	gdb := openVecDB(t)
	if err := gdb.AutoMigrate(&dao.TieredMemoryModel{}); err != nil {
		t.Fatal(err)
	}
	scope := ChannelScope("alpha-long")
	const keepID = "mem-keep"
	const missingID = "mem-missing"
	keepContent := "keep this memory about the red notebook"
	missingContent := "missing memory about the blue kettle"
	insertTieredRow(t, gdb, keepID, scope, keepContent)
	insertTieredRow(t, gdb, missingID, scope, missingContent)

	idx := OpenVecIndex(gdb)
	if !idx.Enabled() {
		t.Fatal("vec index not enabled")
	}
	sentinel := hashEmbed("sentinel-vector-do-not-rewrite")
	if err := gdb.Exec(
		"INSERT INTO memory_vec(embedding, scope_key, entry_id) VALUES (?, ?, ?)",
		sentinel, scope.Key(), keepID,
	).Error; err != nil {
		t.Fatal(err)
	}
	rowidBefore, embBefore := vecRowidAndEmbedding(t, gdb, keepID)
	if !bytes.Equal(embBefore, []byte(sentinel)) {
		t.Fatal("sentinel embedding was not stored")
	}

	store := NewTieredStoreWithDB(nil, gdb)
	if store.vecBackfillDone == nil {
		t.Fatal("backfill did not start")
	}
	select {
	case <-store.vecBackfillDone:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill did not finish")
	}

	rowidAfter, embAfter := vecRowidAndEmbedding(t, gdb, keepID)
	if rowidAfter != rowidBefore {
		t.Fatalf("existing vec row rewritten: rowid %d -> %d", rowidBefore, rowidAfter)
	}
	if !bytes.Equal(embAfter, embBefore) {
		t.Fatal("existing embedding bytes changed")
	}
	hits := store.Vec().Search(scope.Key(), missingContent, 5)
	found := false
	for _, h := range hits {
		if h.EntryID == missingID {
			found = true
		}
		if h.EntryID == keepID && bytes.Equal(embAfter, []byte(hashEmbed(keepContent))) {
			t.Fatal("keep row was re-embedded from its content")
		}
	}
	if !found {
		t.Fatalf("missing entry was not inserted, hits=%v", hitIDs(hits))
	}
	if _, err := store.Vec().listEntryIDs(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewTieredStoreWithDBReturnsBeforeVecUpsert(t *testing.T) {
	gdb := openVecDB(t)
	if err := gdb.AutoMigrate(&dao.TieredMemoryModel{}); err != nil {
		t.Fatal(err)
	}
	scope := ChannelScope("early-return")
	const id = "mem-late"
	content := "indexed only after the constructor returns"
	insertTieredRow(t, gdb, id, scope, content)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseFn()
		vecBackfillGate = nil
	})
	vecBackfillGate = func() { <-release }

	type result struct{ store *TieredStore }
	ch := make(chan result, 1)
	go func() {
		ch <- result{store: NewTieredStoreWithDB(nil, gdb)}
	}()
	var store *TieredStore
	select {
	case got := <-ch:
		store = got.store
	case <-time.After(2 * time.Second):
		t.Fatal("NewTieredStoreWithDB blocked on vec backfill")
	}
	loaded, err := store.GetAll(context.Background(), Tier1LongTerm, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].ID != id {
		t.Fatalf("buckets not populated before backfill: %+v", loaded)
	}
	if store.vecBackfillStarted == nil {
		t.Fatal("backfill started channel was not set")
	}
	select {
	case <-store.vecBackfillStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("backfill goroutine did not start")
	}
	n, err := store.Vec().vecCount(vecTableName)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("vec upsert ran before the gate released, count=%d", n)
	}
	// Existing (none) and in-progress backfill must not block Search.
	if hits := store.Vec().Search(scope.Key(), content, 5); len(hits) != 0 {
		t.Fatalf("search blocked or saw a row before upsert: %v", hitIDs(hits))
	}

	releaseFn()
	select {
	case <-store.vecBackfillDone:
	case <-time.After(5 * time.Second):
		t.Fatal("backfill did not finish after release")
	}
	hits := store.Vec().Search(scope.Key(), content, 5)
	if len(hits) != 1 || hits[0].EntryID != id {
		t.Fatalf("backfill did not insert the row, hits=%v", hitIDs(hits))
	}
}
