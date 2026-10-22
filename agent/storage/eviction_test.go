package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/dao"
)

type fakeCompactor struct {
	repo  *SQLiteRepository
	err   error
	calls int
}

// CompactScope archives every active entry (simulating a merge that keeps the content elsewhere).
func (f *fakeCompactor) CompactScope(ctx context.Context, scope memory.Scope) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	entries, _ := f.repo.GetAllActive(ctx, scope)
	for _, e := range entries {
		f.repo.ArchiveByID(ctx, scope, e.ID)
	}
	return nil
}

func seed(t *testing.T, repo *SQLiteRepository, scope memory.Scope, prefix string, n int, start time.Time, archived bool, source string) {
	t.Helper()
	for i := 0; i < n; i++ {
		e := memory.Entry{ID: fmt.Sprintf("%s%d", prefix, i), Scope: scope, Content: prefix, Source: source,
			CreatedAt: start.Add(time.Duration(i) * time.Minute)}
		if archived {
			e.Metadata = map[string]any{"archived": true}
		}
		m := entryToModel(e)
		if err := repo.db.Create(&m).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func ids(t *testing.T, repo *SQLiteRepository, scope memory.Scope) map[string]bool {
	var out []string
	repo.db.Model(&dao.EntryModel{}).Where("scope_kind = ? AND scope_id = ?", string(scope.Kind), scope.ID).Pluck("id", &out)
	m := map[string]bool{}
	for _, id := range out {
		m[id] = true
	}
	return m
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestEvict_DigestNeverEvictedNorCounted(t *testing.T) {
	repo := NewSQLiteRepository(testDB(t), SQLiteRepositoryConfig{MaxEntriesPerScope: 3})
	sc := memory.ChannelScope("misskey:timeline")
	seed(t, repo, sc, "dg", 5, t0, false, DailyDigestSource) // oldest
	seed(t, repo, sc, "n", 3, t0.Add(time.Hour), false, "note")
	repo.evictIfNeeded(context.Background(), sc)
	if got := len(ids(t, repo, sc)); got != 8 {
		t.Fatalf("want 8 kept (digests uncounted), got %d", got)
	}
	seed(t, repo, sc, "m", 2, t0.Add(2*time.Hour), false, "note")
	repo.evictIfNeeded(context.Background(), sc)
	got := ids(t, repo, sc)
	for i := 0; i < 5; i++ {
		if !got[fmt.Sprintf("dg%d", i)] {
			t.Fatalf("digest dg%d evicted", i)
		}
	}
	if got["n0"] || got["n1"] || !got["n2"] {
		t.Fatalf("oldest notes should be evicted: %v", got)
	}
}

func TestEvict_ArchivedFirst(t *testing.T) {
	repo := NewSQLiteRepository(testDB(t), SQLiteRepositoryConfig{MaxEntriesPerScope: 3})
	sc := memory.ChannelScope("c")
	seed(t, repo, sc, "a", 2, t0.Add(time.Hour), true, "note") // archived but newer
	seed(t, repo, sc, "x", 3, t0, false, "note")               // active, older
	repo.evictIfNeeded(context.Background(), sc)
	got := ids(t, repo, sc)
	if len(got) != 3 || got["a0"] || got["a1"] {
		t.Fatalf("archived should be evicted first, got %v", got)
	}
}

func TestEvict_CompactsBeforeEvict(t *testing.T) {
	fc := &fakeCompactor{}
	repo := NewSQLiteRepository(testDB(t), SQLiteRepositoryConfig{MaxEntriesPerScope: 3, Compactor: fc})
	fc.repo = repo
	sc := memory.ChannelScope("c")
	seed(t, repo, sc, "x", 5, t0, false, "note")
	repo.evictIfNeeded(context.Background(), sc)
	if fc.calls != 1 {
		t.Fatalf("compactor calls = %d", fc.calls)
	}
	if got := len(ids(t, repo, sc)); got != 3 {
		t.Fatalf("want 3 rows after compaction+archived purge, got %d", got)
	}
}

func TestEvict_CompactionFailureKeepsEntries(t *testing.T) {
	fc := &fakeCompactor{err: errors.New("llm down")}
	repo := NewSQLiteRepository(testDB(t), SQLiteRepositoryConfig{MaxEntriesPerScope: 3, Compactor: fc})
	fc.repo = repo
	sc := memory.ChannelScope("c")
	seed(t, repo, sc, "x", 5, t0, false, "note")
	repo.evictIfNeeded(context.Background(), sc)
	if got := len(ids(t, repo, sc)); got != 5 {
		t.Fatalf("entries must be kept on compaction failure, got %d", got)
	}
}

func TestEvict_XchMirrorsFirst(t *testing.T) {
	repo := NewSQLiteRepository(testDB(t), SQLiteRepositoryConfig{MaxEntriesPerScope: 3})
	sc := memory.BotScope("b")
	seed(t, repo, sc, "own", 3, t0, false, "")
	seed(t, repo, sc, "xch:m", 2, t0.Add(time.Hour), false, "")
	repo.evictIfNeeded(context.Background(), sc)
	got := ids(t, repo, sc)
	if got["xch:m0"] || got["xch:m1"] || len(got) != 3 {
		t.Fatalf("xch mirrors should be evicted first, got %v", got)
	}
}

func TestDefaultMaxEntriesPerScope(t *testing.T) {
	if DefaultSQLiteRepositoryConfig().MaxEntriesPerScope != 1000 {
		t.Fatal("default cap changed")
	}
}
