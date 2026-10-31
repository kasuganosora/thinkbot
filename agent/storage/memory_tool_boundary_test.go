package storage

import (
	"context"
	"fmt"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/llm"
)

// Exercise the real tool boundary over a shared database, including the legacy
// raw channel/user scopes consumed by profiles and automatic note writers.
func TestMemoryToolSharedSQLiteBotBoundary(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	raw := NewSQLiteRepository(db)
	a := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "A"})
	b := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "B"})
	for i := 0; i < 25; i++ {
		if err := a.Append(ctx, memory.Entry{ID: fmt.Sprintf("A-%02d", i), Scope: memory.UserScope("same-user"), Content: fmt.Sprintf("archive A item %02d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []memory.Entry{
		{ID: "legacy-owned", Scope: memory.ChannelScope("old-web-private"), Content: "archive owned legacy", Metadata: map[string]any{"bot_id": "A"}},
		{ID: "legacy-unowned", Scope: memory.ChannelScope("old-web-private"), Content: "archive unowned legacy"},
	} {
		if err := raw.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Append(ctx, memory.Entry{ID: "B-private", Scope: memory.UserScope("same-user"), Content: "archive B private"}); err != nil {
		t.Fatal(err)
	}
	tool := memory.Tools(memory.ToolConfig{Repo: a, BotID: "A"})[0].Tool
	exec := &llm.ToolExecContext{Context: ctx}
	result, err := tool.Execute(exec, map[string]any{"action": "count", "scope_kind": "all"})
	if err != nil {
		t.Fatal(err)
	}
	if count := result.(map[string]any)["count"]; count != 26 {
		t.Fatalf("count=%v, want 26", count)
	}
	result, err = tool.Execute(exec, map[string]any{"action": "search", "query": "archive", "scope_kind": "channel", "scope_id": "new-platform-group", "limit": 50})
	if err != nil {
		t.Fatal(err)
	}
	entries := result.(map[string]any)["entries"].([]memory.EntryResult)
	if len(entries) != 26 {
		t.Fatalf("cross-session results=%d, want 26", len(entries))
	}
	for _, e := range entries {
		if e.ID == "B-private" || e.ID == "legacy-unowned" {
			t.Fatalf("unauthorized result=%+v", e)
		}
	}
	// Exact-ID mutation is allowed across this bot's own source scopes.
	result, err = tool.Execute(exec, map[string]any{"action": "replace", "memory_id": "A-00", "scope_kind": "channel", "scope_id": "other-group", "content": "updated own archive"})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["success"] != true {
		t.Fatalf("owned replace failed: %+v", result)
	}
	got, err := a.FetchEntries(ctx, []string{"A-00"})
	if err != nil || len(got) != 1 || got[0].Scope != memory.UserScope("same-user") || got[0].Content != "updated own archive" {
		t.Fatalf("classification lost: %+v, %v", got, err)
	}
	for _, action := range []string{"remove", "replace"} {
		_, _ = tool.Execute(exec, map[string]any{"action": action, "memory_id": "B-private", "scope_kind": "user", "scope_id": "same-user", "content": "must not overwrite B"})
		got, err := b.FetchEntries(ctx, []string{"B-private"})
		if err != nil || len(got) != 1 || got[0].Content != "archive B private" {
			t.Fatalf("foreign %s modified B: %+v %v", action, got, err)
		}
	}
	if _, err := tool.Execute(exec, map[string]any{"action": "search", "query": "archive", "scope_kind": "bot", "scope_id": "B"}); err == nil {
		t.Fatal("model can select a foreign bot")
	}
	// Vector hit hydration uses this same ID boundary.
	fetched, err := memory.ForBotRepository(a, "A").(memory.EntryFetcher).FetchEntries(ctx, []string{"A-01", "B-private", "legacy-unowned", "legacy-owned"})
	if err != nil || len(fetched) != 2 {
		t.Fatalf("vector hydration=%+v, %v", fetched, err)
	}
	if _, err := tool.Execute(exec, map[string]any{"action": "remove", "memory_id": "legacy-owned", "scope_kind": "channel", "scope_id": "other-platform"}); err != nil {
		t.Fatal(err)
	}
	count, err := memory.ForBotRepository(a, "A").Count(ctx, memory.BotScope("A"))
	if err != nil || count != 25 {
		t.Fatalf("count after remove=%d, %v", count, err)
	}
}
