package idgen

import (
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	id := New("msg")
	if !strings.HasPrefix(id, "msg-") {
		t.Errorf("expected prefix 'msg-', got %s", id)
	}
	// prefix + "-" + 24 hex chars = 4 + 24 = 28
	if len(id) != len("msg-")+24 {
		t.Errorf("expected length 28, got %d (%s)", len(id), id)
	}
}

func TestNewUniqueness(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := New("test")
		if seen[id] {
			t.Fatalf("duplicate ID generated: %s", id)
		}
		seen[id] = true
	}
}

func TestNewDifferentPrefixes(t *testing.T) {
	id1 := New("msg")
	id2 := New("mem")
	id3 := New("note")

	if !strings.HasPrefix(id1, "msg-") {
		t.Errorf("expected msg- prefix, got %s", id1)
	}
	if !strings.HasPrefix(id2, "mem-") {
		t.Errorf("expected mem- prefix, got %s", id2)
	}
	if !strings.HasPrefix(id3, "note-") {
		t.Errorf("expected note- prefix, got %s", id3)
	}
}
