package bot

import (
	"testing"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

func TestManagerDetachDoesNotRemoveReplacement(t *testing.T) {
	mgr := NewBotManager(zap.NewNop().Sugar(), noop_trace.NewTracerProvider())
	old := &Bot{ID: "b"}
	next := &Bot{ID: "b"}
	if err := mgr.Register(old); err != nil {
		t.Fatal(err)
	}
	if !mgr.Detach(old) {
		t.Fatal("old instance was not detached")
	}
	if err := mgr.Register(next); err != nil {
		t.Fatal(err)
	}
	// A delayed cleanup must not unregister the newer instance sharing its ID.
	if mgr.Detach(old) {
		t.Fatal("old instance detached its replacement")
	}
	if got, ok := mgr.Get("b"); !ok || got != next {
		t.Fatal("replacement was removed")
	}
	if mgr.Detach(nil) {
		t.Fatal("nil detach succeeded")
	}
}
