package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

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

func TestLexicalOldTextTreatsWildcardAsLiteral(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	repo := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "A", MaxEntriesPerScope: 100})
	for _, e := range []memory.Entry{
		{ID: "pct", Scope: memory.UserScope("u"), Content: "score is 100% done"},
		{ID: "digits", Scope: memory.UserScope("u"), Content: "score is 1000 done"},
	} {
		if err := repo.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	tool := memory.Tools(memory.ToolConfig{Repo: repo, BotID: "A"})[0].Tool
	result, err := tool.Execute(&llm.ToolExecContext{Context: ctx}, map[string]any{
		"action": "replace", "scope_kind": "channel", "scope_id": "other",
		"old_text": "100%", "content": "score is complete",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["success"] != true {
		t.Fatalf("literal percent was not an exact substring: %+v", result)
	}
	got, err := repo.FetchEntries(ctx, []string{"pct", "digits"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.ID == "digits" && e.Content != "score is 1000 done" {
			t.Fatalf("wildcard replace edited %s: %+v", e.ID, got)
		}
		if e.ID == "pct" && (e.Content != "score is complete" || e.Scope != memory.UserScope("u")) {
			t.Fatalf("percent row: %+v", e)
		}
	}
}

// Shared bot reads page the newest rows across every owned scope. Exact edits
// must still reach an older row the recent page no longer contains.
func TestMemoryToolFindsOwnedEntriesOutsideBotWideRecentPage(t *testing.T) {
	source := memory.UserScope("original-user")
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		fillers int
		action  string
		extra   map[string]any
	}{
		{name: "substring", fillers: 150, action: "replace", extra: map[string]any{"old_text": "unique archive token", "memory_id": ""}},
		{name: "id", fillers: 1100, action: "remove", extra: map[string]any{}},
		{name: "batch-substring", fillers: 1100, action: "batch-replace", extra: map[string]any{"old_text": "unique archive token"}},
		{name: "batch-id", fillers: 1100, action: "batch-remove", extra: map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := testDB(t)
			a := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "A", MaxEntriesPerScope: 5000})
			b := NewSQLiteRepository(db, SQLiteRepositoryConfig{BotID: "B", MaxEntriesPerScope: 5000})
			target := memory.Entry{
				ID: "old-target", Scope: source, Content: "unique archive token",
				CreatedAt: base, LastAccessedAt: base,
			}
			if err := a.Append(ctx, target); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.fillers; i++ {
				created := base.Add(time.Duration(i+1) * time.Second)
				if err := a.Append(ctx, memory.Entry{
					ID: fmt.Sprintf("new-%04d", i), Scope: memory.ChannelScope("busy"),
					Content: fmt.Sprintf("newer filler %d", i), CreatedAt: created, LastAccessedAt: created,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Append(ctx, memory.Entry{
				ID: "foreign", Scope: source, Content: "other bot archive token",
				CreatedAt: base.Add(time.Hour), LastAccessedAt: base.Add(time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			tool := memory.Tools(memory.ToolConfig{Repo: a, BotID: "A"})[0].Tool
			exec := &llm.ToolExecContext{Context: ctx}
			args := map[string]any{
				"scope_kind": "channel", "scope_id": "different-platform",
				"memory_id": "old-target", "content": "updated archive token",
			}
			for k, v := range tc.extra {
				args[k] = v
			}
			switch tc.action {
			case "batch-replace":
				args = map[string]any{
					"action": "batch", "scope_kind": "channel", "scope_id": "different-platform",
					"operations": []any{map[string]any{"action": "replace", "old_text": "unique archive token", "content": "updated archive token"}},
				}
			case "batch-remove":
				args = map[string]any{
					"action": "batch", "scope_kind": "channel", "scope_id": "different-platform",
					"operations": []any{map[string]any{"action": "remove", "memory_id": "old-target"}},
				}
			default:
				args["action"] = tc.action
			}
			result, err := tool.Execute(exec, args)
			if err != nil {
				t.Fatal(err)
			}
			if result.(map[string]any)["success"] != true {
				t.Fatalf("old owned entry was invisible to %s: %+v", tc.action, result)
			}
			got, err := a.FetchEntries(ctx, []string{"old-target"})
			if err != nil {
				t.Fatal(err)
			}
			switch tc.action {
			case "remove", "batch-remove":
				if len(got) != 0 {
					t.Fatalf("old entry survived %s: %+v", tc.action, got)
				}
			default:
				if len(got) != 1 || got[0].Scope != source || got[0].Content != "updated archive token" || got[0].Metadata["bot_id"] != "A" {
					t.Fatalf("edit lost the original scope or owner: %+v", got)
				}
			}
			foreign, err := b.FetchEntries(ctx, []string{"foreign"})
			if err != nil || len(foreign) != 1 || foreign[0].Content != "other bot archive token" {
				t.Fatalf("other bot changed: %+v, %v", foreign, err)
			}
		})
	}
}
