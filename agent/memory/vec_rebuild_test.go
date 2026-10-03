package memory

import (
	"context"
	"fmt"
	"testing"

	"github.com/kasuganosora/thinkbot/dao"
)

func TestSelectVecRebuildRowsSharedDB(t *testing.T) {
	rows := []dao.TieredMemoryModel{
		{ID: "a", ScopeKind: "bot", ScopeID: "b1", Content: "bot-self"},
		{ID: "b", ScopeKind: "bot", ScopeID: "b2", Content: "other-bot"},
		{ID: "c", ScopeKind: "channel", ScopeID: "ch1", Content: "tagged", MetadataJSON: `{"bot_id":"b1"}`},
		{ID: "d", ScopeKind: "channel", ScopeID: "ch1", Content: "dream-child"},
		{ID: "e", ScopeKind: "user", ScopeID: "u", Content: "mine", MetadataJSON: `{"bot_id":"b1"}`},
		{ID: "f", ScopeKind: "user", ScopeID: "u", Content: "theirs", MetadataJSON: `{"bot_id":"b2"}`},
		{ID: "g", ScopeKind: "channel", ScopeID: "shared", Content: "untagged-shared"},
		{ID: "h", ScopeKind: "channel", ScopeID: "sess1", Content: "exclusive-channel"},
	}
	events := []dao.UserMessageEvent{
		{BotID: "b1", Channel: "sess1", UserID: "only-b1"},
		{BotID: "b2", Channel: "shared", UserID: "u"},
	}
	selected, fullKeys, partialIDs := selectVecRebuildRows("b1", rows, events)
	got := map[string]bool{}
	for _, row := range selected {
		got[row.ID] = true
	}
	for _, id := range []string{"a", "c", "d", "e", "h"} {
		if !got[id] {
			t.Errorf("missing %s", id)
		}
	}
	for _, id := range []string{"b", "f", "g"} {
		if got[id] {
			t.Errorf("should not index other bot row %s", id)
		}
	}
	if len(partialIDs) != 1 || partialIDs[0] != "e" {
		t.Fatalf("partial ids = %#v", partialIDs)
	}
	full := map[string]bool{}
	for _, k := range fullKeys {
		full[k] = true
	}
	for _, k := range []string{"bot:b1", "channel:ch1", "channel:sess1"} {
		if !full[k] {
			t.Errorf("expected full scope %s in %#v", k, fullKeys)
		}
	}
	if full["user:u"] || full["bot:b2"] || full["channel:shared"] {
		t.Errorf("full keys wiped a shared scope: %#v", fullKeys)
	}
}

func TestIndexVecRebuildRowsReportsProgress(t *testing.T) {
	rows := make([]dao.TieredMemoryModel, 25)
	for i := range rows {
		rows[i] = dao.TieredMemoryModel{
			ID:        fmt.Sprintf("e%d", i),
			Content:   "memory",
			ScopeKind: "bot",
			ScopeID:   "b1",
		}
	}
	var got []VecRebuildProgress
	n, err := indexVecRebuildRows(context.Background(), nil, rows, func(p VecRebuildProgress) {
		got = append(got, p)
	})
	if err != nil || n != 25 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(got) < 2 {
		t.Fatalf("reports: %#v", got)
	}
	prev := 0
	for i, p := range got {
		if p.Phase != VecRebuildWrite || p.Total != 25 {
			t.Fatalf("report %d: %#v", i, p)
		}
		if p.Indexed <= prev {
			t.Fatalf("indexed did not increase: %#v", got)
		}
		prev = p.Indexed
	}
	if got[0].Indexed != 20 || got[len(got)-1].Indexed != 25 {
		t.Fatalf("reports: %#v", got)
	}
}

func TestIndexVecRebuildRowsStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows := []dao.TieredMemoryModel{{ID: "e", Content: "x", ScopeKind: "bot", ScopeID: "b"}}
	n, err := indexVecRebuildRows(ctx, nil, rows, nil)
	if err == nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
