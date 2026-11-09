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

// A task/followup must never run on the light tier, even when L1 explicitly
// asks for it: the light tool surface has no exec/file tools and would invite
// a fabricated completion report.
func TestNormalizeTier_TaskCannotUseLight(t *testing.T) {
	for _, intent := range []string{IntentTask, IntentFollowup} {
		gotIntent, gotModel := normalizeTier(intent, ModelLight)
		if gotIntent != intent || gotModel != ModelDefault {
			t.Fatalf("normalizeTier(%q, light) = (%q, %q)", intent, gotIntent, gotModel)
		}
		env := core.NewEnvelope(core.Message{Text: "继续改"})
		SetTier(env, intent, ModelLight)
		if WantsLight(env) {
			t.Fatalf("SetTier(%q, light) must not produce a light turn", intent)
		}
	}
	for _, intent := range []string{IntentChat, IntentProgress} {
		_, gotModel := normalizeTier(intent, ModelLight)
		if gotModel != ModelLight {
			t.Fatalf("chat/progress must still be allowed on the light tier: %q", gotModel)
		}
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
