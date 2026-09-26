package llm

import (
	"fmt"
	"testing"
	"time"
)

func sampleDeferredTools() []Tool {
	return []Tool{
		{Name: "deferred_a", DeferredLoad: true},
		{Name: "deferred_b", DeferredLoad: true},
		{Name: "eager", DeferredLoad: false},
	}
}

func TestDeferralStore_SessionIsolation(t *testing.T) {
	store := NewDeferralStore(true)

	a := store.ForSession("session-1")
	b := store.ForSession("session-2")

	if a == nil || b == nil {
		t.Fatal("enabled store must return non-nil deferrals")
	}
	if a == b {
		t.Fatal("different sessions must get different ToolDeferral instances")
	}
	// Same session returns the same instance (state persists across turns).
	if a != store.ForSession("session-1") {
		t.Fatal("repeated ForSession for the same id must return the same instance")
	}

	a.SetTools(sampleDeferredTools())
	b.SetTools(sampleDeferredTools())

	// Load a tool in session A only.
	a.Load("deferred_a")

	if !a.IsLoaded("deferred_a") {
		t.Error("session A should have deferred_a loaded")
	}
	// Session B must NOT see A's loaded state (no cross-talk).
	if b.IsLoaded("deferred_a") {
		t.Error("session B must NOT see session A's loaded tool (isolation violated)")
	}
	// A still has deferred_b hidden; B has both hidden.
	if !a.HasUnloaded() {
		t.Error("session A should still have deferred_b unloaded")
	}
	if !b.HasUnloaded() {
		t.Error("session B should still have unloaded deferred tools")
	}
	// After A loads the remaining deferred tool, it has no unloaded tools.
	a.Load("deferred_b")
	if a.HasUnloaded() {
		t.Error("session A should have no unloaded deferred tools after loading all")
	}
	// B remains unaffected by A's loads.
	if b.IsLoaded("deferred_b") {
		t.Error("session B must NOT see session A's second loaded tool")
	}
}

func TestDeferralStore_Disabled(t *testing.T) {
	store := NewDeferralStore(false)
	if store.ForSession("session-1") != nil {
		t.Error("disabled store must return nil (orchestrator bypasses deferral)")
	}
}

func TestDeferralStore_EmptyKeyIsEphemeral(t *testing.T) {
	store := NewDeferralStore(true)
	// An empty conversation key must NOT resolve to a shared per-bot
	// deferral (2026-09-26: a concurrent Misskey turn replaced a Telegram
	// turn's tool list through that shared fallback). It yields a fresh,
	// unstored deferral instead of disabling deferral.
	f1 := store.ForSession("")
	f2 := store.ForSession("")
	if f1 == nil || f2 == nil {
		t.Fatal("empty key must return a non-nil deferral")
	}
	if f1 == f2 {
		t.Error("empty key must never return a shared instance")
	}
	if store.Len() != 0 {
		t.Errorf("ephemeral deferrals must not be stored, len=%d", store.Len())
	}
	real := store.ForSession("session-x")
	if real == f1 || real == f2 {
		t.Error("ephemeral deferral must be distinct from a real conversation's deferral")
	}
}

func TestDeferralStore_PrunesIdleConversations(t *testing.T) {
	store := NewDeferralStore(true)
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }
	for i := 0; i < deferralStorePruneAbove; i++ {
		store.ForSession(fmt.Sprintf("old-%d", i))
	}
	keep := store.ForSession("old-0") // refreshed below, stays
	now = now.Add(deferralStoreIdleTTL + time.Minute)
	if store.ForSession("old-0") != keep {
		t.Fatal("known key must keep its instance")
	}
	store.ForSession("fresh") // over the threshold → prune idle entries
	if got := store.Len(); got != 2 {
		t.Fatalf("idle conversations should be pruned, len=%d", got)
	}
	if store.ForSession("old-0") != keep {
		t.Fatal("recently used conversation must survive pruning")
	}
}
