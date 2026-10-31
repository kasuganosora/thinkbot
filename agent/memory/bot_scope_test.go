package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

func seedOwnedMemories(t *testing.T) *MemoryRepository {
	t.Helper()
	repo := NewMemoryRepository()
	for _, e := range []Entry{
		{ID: "own-bot", Scope: BotScope("A"), Content: "lighthouse bot note"},
		{ID: "own-user", Scope: UserScope("u1"), Content: "lighthouse user preference", Metadata: map[string]any{"bot_id": "A"}},
		{ID: "own-channel", Scope: ChannelScope("private-web"), Content: "lighthouse channel event", Metadata: map[string]any{"bot_id": "A"}},
		{ID: "foreign", Scope: BotScope("B"), Content: "FOREIGN_B lighthouse", Metadata: map[string]any{"bot_id": "A"}},
		{ID: "foreign-user", Scope: UserScope("u1"), Content: "FOREIGN_USER lighthouse", Metadata: map[string]any{"bot_id": "B"}},
		{ID: "legacy", Scope: ChannelScope("private-web"), Content: "UNOWNED_LEGACY lighthouse"},
	} {
		if err := repo.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func TestBotRepositoryPreservesOwnershipAndSourceClassification(t *testing.T) {
	ctx := context.Background()
	repo := seedOwnedMemories(t)
	bound := ForBotRepository(repo, "A")
	for _, scope := range []Scope{BotScope("A"), ChannelScope("different-group"), UserScope("different-user"), Scope{Kind: ScopeAll}} {
		entries, err := bound.Retrieve(ctx, Query{Scopes: []Scope{scope}})
		if err != nil || len(entries) != 3 {
			t.Fatalf("scope %v: %+v, %v", scope, entries, err)
		}
		n, err := bound.Count(ctx, scope)
		if err != nil || n != 3 {
			t.Fatalf("count %v: %d, %v", scope, n, err)
		}
	}
	e := Entry{ID: "new-user", Scope: UserScope("u2"), Content: "new preference", Metadata: map[string]any{"bot_id": "B", "user_id": "u2"}}
	if err := bound.Append(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.FetchEntries(ctx, []string{e.ID})
	if len(got) != 1 || got[0].Scope != e.Scope || got[0].Metadata["bot_id"] != "A" || got[0].Metadata["user_id"] != "u2" {
		t.Fatalf("source classification lost: %+v", got)
	}
	if err := bound.Append(ctx, Entry{Scope: BotScope("B"), Content: "forbidden"}); err == nil {
		t.Fatal("foreign bot scope accepted")
	}
	if got, _ := bound.(EntryFetcher).FetchEntries(ctx, []string{"foreign", "foreign-user", "legacy"}); len(got) != 0 {
		t.Fatalf("foreign ID fetch: %+v", got)
	}
	if stats, err := ForBotRepository(repo, "").(MemoryStatsProvider).MemoryStats(ctx, nil); err == nil || stats.Total != 0 {
		t.Fatalf("missing identity stats=%+v err=%v", stats, err)
	}
	if got, _ := ForBotRepository(repo, "").Retrieve(ctx, Query{}); len(got) != 0 {
		t.Fatalf("missing identity fetched %+v", got)
	}
}

func TestBotRepositoryIDMutationsNeverTouchAnotherOwner(t *testing.T) {
	ctx := context.Background()
	repo := seedOwnedMemories(t)
	bound := ForBotRepository(repo, "A")
	if err := bound.Delete(ctx, UserScope("u1"), "foreign-user"); err != nil {
		t.Fatal(err)
	}
	// Both bots use the same raw scope. A replace of a foreign ID must never
	// forward that ID into the underlying raw-scope delete operation.
	if err := bound.(Replacer).Replace(ctx, UserScope("u1"), "foreign-user", Entry{ID: "own-new", Scope: UserScope("u1"), Content: "A replacement"}); err != nil {
		t.Fatal(err)
	}
	if err := bound.(Replacer).Replace(ctx, ChannelScope("elsewhere"), "own-user", Entry{ID: "own-user", Scope: ChannelScope("elsewhere"), Content: "updated preference"}); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.FetchEntries(ctx, []string{"own-user"})
	if len(got) != 1 || got[0].Scope != UserScope("u1") {
		t.Fatalf("replace reclassified a user profile input: %+v", got)
	}
	if err := bound.Clear(ctx, BotScope("A")); err != nil {
		t.Fatal(err)
	}
	foreign, _ := repo.FetchEntries(ctx, []string{"foreign", "foreign-user", "legacy"})
	if len(foreign) != 3 {
		t.Fatalf("foreign or legacy data changed: %+v", foreign)
	}
	for _, e := range foreign {
		if strings.Contains(e.Content, "replacement") {
			t.Fatalf("foreign entry replaced: %+v", e)
		}
	}
}

func TestBotRepositoryCountAndFindBeyondDefaultPage(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	bound := ForBotRepository(repo, "A")
	for i := 0; i < 25; i++ {
		if err := bound.Append(ctx, Entry{ID: fmt.Sprintf("row-%02d", i), Scope: UserScope("u"), Content: "row"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := bound.Count(ctx, BotScope("A"))
	if err != nil || n != 25 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	stats, err := bound.(MemoryStatsProvider).MemoryStats(ctx, nil)
	if err != nil || stats.Total != 25 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	if err := bound.Delete(ctx, BotScope("A"), "row-00"); err != nil {
		t.Fatal(err)
	}
	n, _ = bound.Count(ctx, BotScope("A"))
	if n != 24 {
		t.Fatalf("old row was not deleted: count=%d", n)
	}
}

func TestMemoryToolSharedBotBoundaryForEveryChatContext(t *testing.T) {
	for _, chatType := range []string{"private", "group", "", "unknown"} {
		t.Run("chat_"+chatType, func(t *testing.T) {
			repo := seedOwnedMemories(t)
			prov := &recallLLM{keywords: `{"keywords":["lighthouse"]}`, refine: `{"keep":["own-bot","own-user","own-channel","foreign","foreign-user","legacy"]}`,
				inspect: func(p llm.GenerateParams) {
					for _, m := range p.Messages {
						if strings.Contains(llm.TextFromParts(m.Content), "FOREIGN_") || strings.Contains(llm.TextFromParts(m.Content), "UNOWNED_LEGACY") {
							t.Fatal("unowned memory reached refinement provider")
						}
					}
				},
			}
			tool := Tools(ToolConfig{Repo: repo, BotID: "A", Recall: DeepRecallConfig{Provider: prov}})[0].Tool
			ctx := recallTurns(llm.UserMessage("what was the archive project?"))
			if chatType != "" {
				ctx = agenttools.ContextWithSessionContext(ctx, agenttools.ToolSessionContext{ChatType: chatType})
			}
			for _, args := range []map[string]any{
				{"action": "count", "scope_kind": "all"},
				{"action": "recent", "scope_kind": "channel", "scope_id": "another-platform"},
				{"action": "search", "query": "lighthouse", "scope_kind": "user", "scope_id": "another-user"},
				{"action": "search", "scope_kind": "all"},
			} {
				result, err := tool.Execute(&llm.ToolExecContext{Context: ctx, UserRequest: "what was the archive project?"}, args)
				if err != nil {
					t.Fatal(err)
				}
				m := result.(map[string]any)
				if m["count"] != 3 {
					t.Fatalf("args=%v result=%+v", args, m)
				}
			}
			if _, err := tool.Execute(&llm.ToolExecContext{Context: ctx}, map[string]any{"action": "recent", "scope_kind": "bot", "scope_id": "B"}); err == nil {
				t.Fatal("model selected another bot")
			}
		})
	}
}

func TestMemoryDeepVectorFetchChecksBotOwnerBeforeRefinement(t *testing.T) {
	repo := seedOwnedMemories(t)
	hidden := hideRetrieveRepo{MemoryRepository: repo, hide: "own-user"}
	vec := &fakeVec{hits: map[string][]Hit{"lighthouse": {{EntryID: "own-user", Distance: 0}, {EntryID: "foreign", Distance: 0}, {EntryID: "foreign-user", Distance: 0}, {EntryID: "legacy", Distance: 0}}}}
	prov := &recallLLM{keywords: `{"keywords":["lighthouse"]}`, refine: `{"keep":["own-user","foreign","foreign-user","legacy"]}`,
		inspect: func(p llm.GenerateParams) {
			for _, m := range p.Messages {
				if strings.Contains(llm.TextFromParts(m.Content), "FOREIGN_") || strings.Contains(llm.TextFromParts(m.Content), "UNOWNED_LEGACY") {
					t.Fatal("foreign vector hit reached provider")
				}
			}
		},
	}
	tool := Tools(ToolConfig{Repo: hidden, BotID: "A", Recall: DeepRecallConfig{Provider: prov, Vec: vec}})[0].Tool
	result, err := tool.Execute(&llm.ToolExecContext{Context: recallTurns(llm.UserMessage("the old project")), UserRequest: "the old project"}, map[string]any{"action": "search", "scope_kind": "all"})
	if err != nil {
		t.Fatal(err)
	}
	entries := result.(map[string]any)["entries"].([]EntryResult)
	if len(entries) != 1 || entries[0].ID != "own-user" {
		t.Fatalf("vector results=%+v", entries)
	}
	keys := vecScopeKeys(BotScope("A"), []Entry{{Scope: UserScope("u"), Metadata: map[string]any{"bot_id": "A"}}, {Scope: BotScope("B")}})
	if len(keys) != 2 || keys[0] != BotScope("A").Key() || keys[1] != UserScope("u").Key() {
		t.Fatalf("vector scopes=%v", keys)
	}
}

func TestMemoryToolSharedMutationsKeepScopeAndMirrorLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryRepository()
	tool := Tools(ToolConfig{Repo: repo, BotID: "A"})[0].Tool
	exec := &llm.ToolExecContext{Context: ctx}
	call := func(args map[string]any) map[string]any {
		t.Helper()
		r, err := tool.Execute(exec, args)
		if err != nil {
			t.Fatal(err)
		}
		return r.(map[string]any)
	}
	call(map[string]any{"action": "add", "scope_kind": "channel", "scope_id": "old-platform", "content": "archive shared note"})
	entries, _ := repo.Retrieve(ctx, Query{Scopes: []Scope{ChannelScope("old-platform")}})
	if len(entries) != 1 {
		t.Fatalf("original=%+v", entries)
	}
	id := entries[0].ID
	call(map[string]any{"action": "replace", "scope_kind": "user", "scope_id": "different-user", "old_text": "archive shared note", "content": "updated archive shared note"})
	mirror, _ := repo.FetchEntries(ctx, []string{crossChannelMirrorPrefix + id})
	if len(mirror) != 1 || !strings.Contains(mirror[0].Content, "updated archive") {
		t.Fatalf("mirror not updated: %+v", mirror)
	}
	call(map[string]any{"action": "remove", "scope_kind": "user", "scope_id": "different-user", "memory_id": id})
	if entries, _ := repo.Retrieve(ctx, Query{}); len(entries) != 0 {
		t.Fatalf("deleted note survived as a mirror: %+v", entries)
	}

	bound := ForBotRepository(repo, "A")
	if err := bound.Append(ctx, Entry{ID: "user-original", Scope: UserScope("u"), Content: "original user fact"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(ctx, Entry{ID: "foreign", Scope: UserScope("u"), Content: "foreign user fact", Metadata: map[string]any{"bot_id": "B"}}); err != nil {
		t.Fatal(err)
	}
	result := call(map[string]any{"action": "batch", "scope_kind": "channel", "scope_id": "group", "operations": []any{map[string]any{"action": "replace", "old_text": "original user fact", "content": "updated user fact"}}})
	if result["success"] != true {
		t.Fatalf("batch result=%+v", result)
	}
	got, _ := repo.FetchEntries(ctx, []string{"user-original", crossChannelMirrorPrefix + "user-original"})
	if len(got) != 1 || got[0].Scope != UserScope("u") || got[0].Content != "updated user fact" {
		t.Fatalf("batch reclassified user input: %+v", got)
	}
	result = call(map[string]any{"action": "batch", "scope_kind": "user", "scope_id": "u", "operations": []any{map[string]any{"action": "remove", "memory_id": "foreign"}}})
	if result["success"] != false {
		t.Fatalf("foreign batch did not fail: %+v", result)
	}
	foreign, _ := repo.FetchEntries(ctx, []string{"foreign"})
	if len(foreign) != 1 {
		t.Fatal("foreign batch removed another bot's entry")
	}
}
