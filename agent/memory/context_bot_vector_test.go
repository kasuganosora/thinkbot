package memory

import (
	"context"
	"testing"
)

// Deliberately return all requested rows to verify that fusion also checks
// ownership after hydration, independent of the storage implementation.
type fusionBotRetriever struct {
	*MemoryRepository
	entries []Entry
}

func (r *fusionBotRetriever) BoundBotID() string { return "a" }
func (r *fusionBotRetriever) FetchEntries(_ context.Context, ids []string) ([]Entry, error) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []Entry
	for _, e := range r.entries {
		if want[e.ID] {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestVecContextFusionBotOwnershipAndOriginalScopes(t *testing.T) {
	gdb := openVecDB(t)
	idx := OpenVecIndex(gdb)
	if !idx.Enabled() {
		t.Skip("sqlite-vec unavailable")
	}
	entries := []Entry{
		{ID: "a-user", Scope: UserScope("u"), Content: "fictional orchard amber", Metadata: map[string]any{"bot_id": "a"}},
		{ID: "a-channel", Scope: ChannelScope("c"), Content: "fictional orchard amber", Metadata: map[string]any{"bot_id": "a"}},
		{ID: "b-user", Scope: UserScope("u"), Content: "fictional orchard amber", Metadata: map[string]any{"bot_id": "b"}},
		{ID: "unknown", Scope: UserScope("u"), Content: "fictional orchard amber"},
		{ID: "conflict", Scope: BotScope("b"), Content: "fictional orchard amber", Metadata: map[string]any{"bot_id": "a"}},
	}
	for _, e := range entries {
		idx.Upsert("bot:a", e.ID, e.Content)
	} // include corrupt index ownership
	repo := &fusionBotRetriever{MemoryRepository: NewMemoryRepository(), entries: entries}
	mgr := NewContextManager(repo, NewContextBuilder(ContextBuilderConfig{}), nil, nil)
	mgr.UseVec(idx)
	for _, tc := range []struct {
		scope Scope
		want  []string
	}{
		{BotScope("a"), []string{"a-user", "a-channel"}},
		{UserScope("u"), []string{"a-user"}},
		{ChannelScope("c"), []string{"a-channel"}},
		{BotScope("b"), nil},
	} {
		got := mgr.fuseWithVec(context.Background(), []Entry{entries[2]}, []Scope{tc.scope}, "fictional orchard amber")
		ids := map[string]bool{}
		for _, e := range got {
			ids[e.ID] = true
		}
		if len(ids) != len(tc.want) {
			t.Fatalf("scope %s got %v want %v", tc.scope.Key(), ids, tc.want)
		}
		for _, id := range tc.want {
			if !ids[id] {
				t.Fatalf("scope %s missing %s", tc.scope.Key(), id)
			}
		}
	}
}
