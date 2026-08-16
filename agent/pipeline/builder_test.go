package pipeline

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

func noopStage(name string) core.Stage {
	return &core.StageFunc{
		StageName: name,
		Fn: func(_ context.Context, env *core.Envelope) (*core.Envelope, error) {
			return env, nil
		},
	}
}

func TestBuilderOrdering(t *testing.T) {
	pb := NewBuilder()
	pb.Add(100, noopStage("llm"))
	pb.Add(45, noopStage("lurk"))
	pb.Add(90, noopStage("recall"))
	pb.Add(95, noopStage("rhythm"))
	got := pb.Build()
	want := []string{"lurk", "recall", "rhythm", "llm"}
	if len(got) != len(want) {
		t.Fatalf("got %d stages, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Stage.Name() != w {
			t.Errorf("stage %d = %q, want %q", i, got[i].Stage.Name(), w)
		}
		if !got[i].Enabled {
			t.Errorf("stage %q should be enabled", w)
		}
	}
}

func TestBuilderAddIf(t *testing.T) {
	pb := NewBuilder()
	pb.AddIf(false, 40, noopStage("engagement")) // must be skipped
	pb.AddIf(true, 5, noopStage("heartbeat"))
	pb.Add(100, noopStage("llm"))
	got := pb.Build()
	if len(got) != 2 {
		t.Fatalf("AddIf(false) leaked a stage: got %d, want 2", len(got))
	}
	if got[0].Stage.Name() != "heartbeat" || got[1].Stage.Name() != "llm" {
		t.Errorf("ordering wrong: %q, %q", got[0].Stage.Name(), got[1].Stage.Name())
	}
}

func TestBuilderMode(t *testing.T) {
	pb := NewBuilder().WithMode(ModeLurkOnly)
	if pb.Mode() != ModeLurkOnly {
		t.Errorf("Mode() = %q, want %q", pb.Mode(), ModeLurkOnly)
	}
}

func TestModeGroups(t *testing.T) {
	// standard / code：四组全部启用。
	for _, m := range []PipelineMode{ModeStandard, ModeCode} {
		g := ModeGroups(m)
		for _, grp := range []StageGroup{GroupEngagement, GroupHeartbeat, GroupLurk, GroupCode} {
			if !g[grp] {
				t.Errorf("mode %q: group %q should be enabled", m, grp)
			}
		}
	}

	// lurk-only：仅 lurk 组启用，其余（engagement/heartbeat/code）一律关闭。
	g := ModeGroups(ModeLurkOnly)
	if !g[GroupLurk] {
		t.Errorf("lurk-only: GroupLurk should be enabled")
	}
	for _, grp := range []StageGroup{GroupEngagement, GroupHeartbeat, GroupCode} {
		if g[grp] {
			t.Errorf("lurk-only: group %q should be disabled", grp)
		}
	}

	// 未知模式 fail-open：回退到 standard（四组全开）。
	g = ModeGroups(PipelineMode("bogus"))
	for _, grp := range []StageGroup{GroupEngagement, GroupHeartbeat, GroupLurk, GroupCode} {
		if !g[grp] {
			t.Errorf("unknown mode: group %q should be enabled (fail-open)", grp)
		}
	}
}
