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

// Observe the scope passed by the tool wrapper, while still executing real SQL.
// Shared BotScope reads must not turn exact source mutations into broad writes.
type recordingMemoryDeletes struct {
	*SQLiteRepository
	deleted    map[string]memory.Scope
	clearCalls int
}

func (r *recordingMemoryDeletes) Delete(ctx context.Context, scope memory.Scope, id string) error {
	r.deleted[id] = scope
	return r.SQLiteRepository.Delete(ctx, scope, id)
}

func (r *recordingMemoryDeletes) Clear(ctx context.Context, scope memory.Scope) error {
	r.clearCalls++
	return r.SQLiteRepository.Clear(ctx, scope)
}

func TestMemoryToolCrossSessionMutationsPreserveSourceScope(t *testing.T) {
	for _, source := range []memory.Scope{memory.UserScope("original-user"), memory.ChannelScope("original-channel")} {
		t.Run(source.Key(), func(t *testing.T) {
			ctx := context.Background()
			db := testDB(t)
			a := &recordingMemoryDeletes{
				SQLiteRepository: NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "A"}),
				deleted:          make(map[string]memory.Scope),
			}
			b := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "B"})
			for _, e := range []memory.Entry{
				{ID: "target", Scope: source, Content: "Original source memory"},
				{ID: "neighbor", Scope: source, Content: "Neighbor memory must survive"},
			} {
				if err := a.Append(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Append(ctx, memory.Entry{ID: "foreign", Scope: source, Content: "Other bot memory must survive"}); err != nil {
				t.Fatal(err)
			}
			tool := memory.Tools(memory.ToolConfig{Repo: a, BotID: "A"})[0].Tool
			exec := &llm.ToolExecContext{Context: ctx}
			for _, action := range []string{"replace", "remove"} {
				result, err := tool.Execute(exec, map[string]any{
					"action": action, "memory_id": "target", "content": "Updated source memory",
					"scope_kind": "channel", "scope_id": "different-platform-conversation",
				})
				if err != nil {
					t.Fatal(err)
				}
				response := result.(map[string]any)
				if response["success"] != true || response["scope"] != source.Key() {
					t.Fatalf("%s response lost original scope: %+v", action, response)
				}
				entries, err := a.FetchEntries(ctx, []string{"target"})
				if err != nil {
					t.Fatal(err)
				}
				if action == "replace" {
					if len(entries) != 1 || entries[0].Scope != source || entries[0].Content != "Updated source memory" || entries[0].Metadata["bot_id"] != "A" {
						t.Fatalf("cross-session replacement changed source classification/owner: %+v", entries)
					}
				} else if len(entries) != 0 || a.deleted["target"] != source {
					t.Fatalf("cross-session delete did not target original scope: entries=%+v deleted=%+v", entries, a.deleted)
				}
			}
			for _, check := range []struct {
				repo    *SQLiteRepository
				id      string
				content string
			}{
				{a.SQLiteRepository, "neighbor", "Neighbor memory must survive"},
				{b, "foreign", "Other bot memory must survive"},
			} {
				entries, err := check.repo.FetchEntries(ctx, []string{check.id})
				if err != nil || len(entries) != 1 || entries[0].Scope != source || entries[0].Content != check.content {
					t.Fatalf("unrelated entry %s changed: %+v, %v", check.id, entries, err)
				}
			}
		})
	}
}

func TestMemoryBotWrapperClearDeletesAllOwnedSourceScopesOnly(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	raw := NewSQLiteRepository(db)
	a := &recordingMemoryDeletes{
		SQLiteRepository: NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "A"}),
		deleted:          make(map[string]memory.Scope),
	}
	b := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "B"})
	expected := make(map[string]memory.Scope)
	var foreignIDs []string
	for i, scope := range []memory.Scope{memory.UserScope("shared-user"), memory.ChannelScope("shared-channel"), memory.GlobalScope(), memory.BotScope("A")} {
		id := fmt.Sprintf("A-scope-%d", i)
		expected[id] = scope
		if err := a.Append(ctx, memory.Entry{ID: id, Scope: scope, Content: "Own memory " + id}); err != nil {
			t.Fatal(err)
		}
		foreignScope := scope
		if scope.Kind == memory.ScopeBot {
			foreignScope = memory.BotScope("B")
		}
		foreignID := fmt.Sprintf("B-scope-%d", i)
		foreignIDs = append(foreignIDs, foreignID)
		if err := b.Append(ctx, memory.Entry{ID: foreignID, Scope: foreignScope, Content: "Foreign memory " + foreignID}); err != nil {
			t.Fatal(err)
		}
	}
	legacyScope := memory.ChannelScope("legacy-channel")
	expected["legacy-owned"] = legacyScope
	for _, entry := range []memory.Entry{
		{ID: "legacy-owned", Scope: legacyScope, Content: "Owned legacy memory", Metadata: map[string]any{"bot_id": "A"}},
		{ID: "legacy-unowned", Scope: legacyScope, Content: "Unattributed legacy memory"},
	} {
		if err := raw.Append(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	bound := memory.ForBotRepository(a, "A")
	if err := bound.Clear(ctx, memory.BotScope("A")); err != nil {
		t.Fatal(err)
	}
	if a.clearCalls != 0 || len(a.deleted) != len(expected) {
		t.Fatalf("wrapper must delete each owned ID, not bulk-clear a shared view: clears=%d deleted=%+v", a.clearCalls, a.deleted)
	}
	for id, scope := range expected {
		if a.deleted[id] != scope {
			t.Fatalf("delete %s used scope %+v, want original %+v", id, a.deleted[id], scope)
		}
	}
	if entries, err := bound.Retrieve(ctx, memory.Query{}); err != nil || len(entries) != 0 {
		t.Fatalf("owned source scopes remain after clear: %+v, %v", entries, err)
	}
	if entries, err := b.FetchEntries(ctx, foreignIDs); err != nil || len(entries) != len(foreignIDs) {
		t.Fatalf("clear damaged another bot's source scopes: %+v, %v", entries, err)
	}
	if entries, err := raw.FetchEntries(ctx, []string{"legacy-unowned"}); err != nil || len(entries) != 1 || entries[0].Scope != legacyScope {
		t.Fatalf("clear damaged unowned legacy memory: %+v, %v", entries, err)
	}
}
