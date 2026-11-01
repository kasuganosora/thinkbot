package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

// scriptedVecSearch returns hits in nearest-first order, truncated to k.
// Identical texts share distance 0 and crowd out a same-scope neighbour.
type scriptedVecSearch struct {
	k    int
	hits []memory.Hit
}

func (s *scriptedVecSearch) Search(scopeKey, query string, k int) []memory.Hit {
	s.k = k
	if k >= len(s.hits) {
		return s.hits
	}
	return s.hits[:k]
}

func TestCompactionNeighboursSurviveSharedVectorBucket(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	repo := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "a", MaxEntriesPerScope: 100})
	scope := memory.ChannelScope("shared")
	for _, e := range []memory.Entry{
		{ID: "seed", Scope: scope, Content: "fictional orchard amber"},
		{ID: "neighbor", Scope: scope, Content: "fictional orchard amber grove"},
	} {
		if err := repo.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	hits := []memory.Hit{{EntryID: "seed", Distance: 0}}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("other-%d", i)
		if err := repo.Append(ctx, memory.Entry{ID: id, Scope: memory.UserScope("u"), Content: "fictional orchard amber"}); err != nil {
			t.Fatal(err)
		}
		hits = append(hits, memory.Hit{EntryID: id, Distance: 0})
	}
	hits = append(hits, memory.Hit{EntryID: "neighbor", Distance: 0.2})
	vec := &scriptedVecSearch{hits: hits}
	seed, err := repo.FetchEntries(ctx, []string{"seed"})
	if err != nil || len(seed) != 1 {
		t.Fatalf("seed=%+v err=%v", seed, err)
	}
	c := &SQLiteCompactor{repo: repo, sel: dedupSelectConfig{topK: 5, minSim: 0.1, maxMergedChars: 1000}}
	got := c.vecNeighbours(ctx, scope, vec, seed[0])
	if len(got) != 1 || got[0].ID != "neighbor" {
		t.Fatalf("neighbors=%+v scanned=%d", got, vec.k)
	}
}
