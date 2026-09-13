package skill

import (
	"context"
	"testing"
)

type memStore struct {
	m map[string]string
}

func newMemStore() *memStore { return &memStore{m: map[string]string{}} }

func (s *memStore) Get(key string) (string, bool) {
	v, ok := s.m[key]
	return v, ok
}
func (s *memStore) Set(_ context.Context, key, value string) error {
	s.m[key] = value
	return nil
}
func (s *memStore) GetBool(key string, def bool) bool {
	v, ok := s.Get(key)
	if !ok {
		return def
	}
	return v == "true"
}

func TestBotSkillStoreAdapter_PerBotOverridesGlobal(t *testing.T) {
	inner := newMemStore()
	_ = inner.Set(context.Background(), "skill.pdf.enabled", "false")
	a := NewBotSkillStoreAdapter(inner, "bot-1")

	val, ok := a.Get("skill.pdf.enabled")
	if !ok || val != "false" {
		t.Fatalf("fallback global: got %q ok=%v", val, ok)
	}

	if err := a.Set(context.Background(), "skill.pdf.enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if inner.m["bot.bot-1.skill.pdf.enabled"] != "true" {
		t.Fatalf("expected per-bot key written, map=%v", inner.m)
	}
	if inner.m["skill.pdf.enabled"] != "false" {
		t.Fatal("global default must stay untouched")
	}
	val, ok = a.Get("skill.pdf.enabled")
	if !ok || val != "true" {
		t.Fatalf("per-bot read: got %q ok=%v", val, ok)
	}
}
