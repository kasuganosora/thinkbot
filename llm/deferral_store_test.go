package llm

import (
	"testing"
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

func TestDeferralStore_EmptySessionFallback(t *testing.T) {
	store := NewDeferralStore(true)
	// Empty session id must fall back to a single shared deferral (not nil,
	// and stable across calls) rather than disabling deferral.
	f1 := store.ForSession("")
	f2 := store.ForSession("")
	if f1 == nil {
		t.Fatal("empty session must return a non-nil fallback deferral")
	}
	if f1 != f2 {
		t.Error("empty session must return the same shared fallback instance")
	}

	// The fallback is independent from a real session's deferral.
	real := store.ForSession("session-x")
	if real == f1 {
		t.Error("fallback deferral must be distinct from a real session's deferral")
	}
}
