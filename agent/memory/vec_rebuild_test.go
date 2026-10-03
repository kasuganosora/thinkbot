package memory

import (
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
