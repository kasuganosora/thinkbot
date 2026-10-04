package memory

import (
	"context"
	"testing"
	"time"
)

func TestDeleteLeavesTombstoneThatBlocksRewrite(t *testing.T) {
	s := NewTieredStore(nil)
	ctx := context.Background()
	scope := ChannelScope("c1")
	if err := s.Append(ctx, TieredEntry{Entry: Entry{ID: "m1", Scope: scope, Content: "用户养了一只鹦鹉", Source: "note"}, Tier: Tier1LongTerm}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, Tier1LongTerm, scope, "m1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, TieredEntry{Entry: Entry{ID: "m2", Scope: scope, Content: "用户养了一只鹦鹉", Source: "dreaming"}, Tier: Tier1LongTerm}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAll(ctx, Tier1LongTerm, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted fact was written back: %+v", got)
	}
	if err := s.Append(ctx, TieredEntry{Entry: Entry{ID: "m3", Scope: scope, Content: "用户改养金鱼", Source: "note"}, Tier: Tier1LongTerm}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAll(ctx, Tier1LongTerm, scope)
	if len(got) != 1 || got[0].ID != "m3" {
		t.Fatalf("new fact should be stored, got %+v", got)
	}
}

func TestSupersedeKeepsOldRowOutOfRecall(t *testing.T) {
	s := NewTieredStore(nil)
	ctx := context.Background()
	scope := UserScope("u1")
	old := TieredEntry{Entry: Entry{ID: "old", Scope: scope, Content: "住在上海", Source: "note"}, Tier: Tier1LongTerm}
	if err := s.Append(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Supersede(ctx, scope, "old", TieredEntry{Entry: Entry{ID: "new", Scope: scope, Content: "住在杭州", Source: "note"}}); err != nil {
		t.Fatal(err)
	}
	all, err := s.GetAll(ctx, Tier1LongTerm, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want old+new, got %d", len(all))
	}
	visible := FilterRecall(tieredToEntries(all))
	if len(visible) != 1 || visible[0].Content != "住在杭州" {
		t.Fatalf("recall = %+v", visible)
	}
	if !s.BlocksNewFact(scope, "住在上海") {
		t.Fatal("superseded statement should not come back as a new fact")
	}
	if s.Forgotten(scope, "住在上海") {
		t.Fatal("supersede must not tombstone")
	}
}

func TestPendingIsHiddenUntilConfirm(t *testing.T) {
	s := NewTieredStore(nil)
	ctx := context.Background()
	scope := BotScope("b1")
	e := Entry{ID: "p1", Scope: scope, Content: "可能是后端工程师", Category: "observation", Source: "dreaming"}
	MarkInferred(&e)
	if err := s.Append(ctx, TieredEntry{Entry: e, Tier: Tier1LongTerm}); err != nil {
		t.Fatal(err)
	}
	stated := Entry{ID: "u1", Scope: scope, Content: "用户说自己用 Go", Category: "fact", Source: "formation"}
	if err := s.Append(ctx, TieredEntry{Entry: stated, Tier: Tier1LongTerm}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.GetAll(ctx, Tier1LongTerm, scope)
	visible := FilterRecall(tieredToEntries(all))
	if len(visible) != 1 || visible[0].ID != "u1" {
		t.Fatalf("pending leaked into recall: %+v", visible)
	}
	ok, err := s.Confirm(ctx, Tier1LongTerm, scope, "p1")
	if err != nil || !ok {
		t.Fatalf("confirm: %v %v", ok, err)
	}
	all, _ = s.GetAll(ctx, Tier1LongTerm, scope)
	visible = FilterRecall(tieredToEntries(all))
	if len(visible) != 2 {
		t.Fatalf("after confirm want 2, got %+v", visible)
	}
}

func TestFuseRankingsPrefersAgreement(t *testing.T) {
	got := fuseRankings([]string{"a", "b", "c"}, []string{"b", "c", "d"})
	if len(got) == 0 || got[0] != "b" {
		t.Fatalf("fused order = %v", got)
	}
	if cosineFromDistance(0) < 0.99 {
		t.Fatal("identical vectors should be cosine 1")
	}
	if cosineFromDistance(1) < minCosine-0.01 || cosineFromDistance(1) > minCosine+0.01 {
		t.Fatalf("distance 1 cosine = %v", cosineFromDistance(1))
	}
}

func TestRemoveDoesNotTombstone(t *testing.T) {
	s := NewTieredStore(nil)
	ctx := context.Background()
	scope := BotScope("b")
	_ = s.Append(ctx, TieredEntry{Entry: Entry{ID: "p", Scope: scope, Content: "安静", Category: "bot_personality"}, Tier: Tier3Profile})
	if err := s.Remove(ctx, Tier3Profile, scope, "p"); err != nil {
		t.Fatal(err)
	}
	if s.Forgotten(scope, "安静") {
		t.Fatal("profile refresh must not tombstone")
	}
}

func tieredToEntries(in []TieredEntry) []Entry {
	out := make([]Entry, len(in))
	for i := range in {
		out[i] = in[i].Entry
	}
	return out
}

func TestFingerprintIgnoresPunctuation(t *testing.T) {
	if MemoryFingerprint("住在上海") != MemoryFingerprint("住在上海。") {
		t.Fatal("punctuation should not change the fingerprint")
	}
	_ = time.Second
}
