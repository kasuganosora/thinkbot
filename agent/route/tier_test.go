package route

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestNarrowToolsDropsHeavyTools(t *testing.T) {
	in := []llm.Tool{
		{Name: "memory"},
		{Name: "now"},
		{Name: "sandbox_exec"},
		{Name: "workflow_task"},
		{Name: "calculate"},
	}
	got := NarrowTools(in)
	names := map[string]bool{}
	for _, tool := range got {
		names[tool.Name] = true
	}
	if !names["memory"] || !names["now"] || !names["calculate"] {
		t.Fatalf("kept = %#v", names)
	}
	if names["sandbox_exec"] || names["workflow_task"] {
		t.Fatalf("heavy tools leaked: %#v", names)
	}
}

func TestUpgradeToolRequestsFullTierOnce(t *testing.T) {
	env := core.NewEnvelope(core.Message{Text: "run the tests"})
	SetTier(env, IntentChat, ModelLight)
	tool := NewUpgradeTool(env)
	_, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, nil)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if !UpgradeRequested(env) {
		t.Fatal("expected upgrade request")
	}
	MarkUpgraded(env)
	if WantsLight(env) {
		t.Fatal("upgraded turn must leave the light tier")
	}
}
