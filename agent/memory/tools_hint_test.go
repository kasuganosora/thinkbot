package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func memExec(t *testing.T, repo Repository, args map[string]any) (any, error) {
	t.Helper()
	tool := Tools(ToolConfig{Repo: repo, BotID: "botX"})[0].Tool
	return tool.Execute(&llm.ToolExecContext{Context: context.Background()}, args)
}

func TestMemoryReplace_NoMatchListsClosestAndReplaceByID(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	_ = repo.Append(ctx, Entry{ID: "m1", Scope: ChannelScope("ch1"), Content: "tokyo-transit技能(/data/skills/tokyo-transit,SKILL.md+odpt_query.js+odpt_station.js,零依赖)"})
	_ = repo.Append(ctx, Entry{ID: "m2", Scope: ChannelScope("ch1"), Content: "用户喜欢猫"})

	// old_text is an older/paraphrased version of m1 → no substring match.
	res, err := memExec(t, repo, map[string]any{
		"action": "replace", "scope_kind": "channel", "scope_id": "ch1",
		"old_text": "tokyo-transit技能(/data/skills/tokyo-transit,已发布github)",
		"content":  "tokyo-transit技能 v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	cands, _ := m["closest_entries"].([]map[string]string)
	if m["success"] != false || len(cands) == 0 || cands[0]["id"] != "m1" {
		t.Fatalf("expected closest candidate m1, got %#v", m)
	}
	if !strings.Contains(m["error"].(string), "memory_id") {
		t.Errorf("error should suggest memory_id: %v", m["error"])
	}

	// Retry by id succeeds.
	res, err = memExec(t, repo, map[string]any{
		"action": "replace", "scope_kind": "channel", "scope_id": "ch1",
		"memory_id": "m1", "content": "tokyo-transit技能 v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["success"] != true {
		t.Fatalf("replace by id failed: %#v", res)
	}
	got, _ := repo.Retrieve(ctx, Query{Scopes: []Scope{ChannelScope("ch1")}, Limit: 10})
	found := false
	for _, e := range got {
		if e.ID == "m1" && e.Content == "tokyo-transit技能 v2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("entry not replaced: %#v", got)
	}
}

func TestMemoryTool_MissingActionIsActionable(t *testing.T) {
	repo := NewMemoryRepository()
	_, err := memExec(t, repo, map[string]any{"old_text": "a", "content": "b"})
	if err == nil || !strings.Contains(err.Error(), `"action":"replace"`) || !strings.Contains(err.Error(), "add, replace, remove") {
		t.Fatalf("unexpected: %v", err)
	}
	_, err = memExec(t, repo, map[string]any{"content": "b"})
	if err == nil || !strings.Contains(err.Error(), `"action":"add"`) {
		t.Fatalf("unexpected: %v", err)
	}
}
