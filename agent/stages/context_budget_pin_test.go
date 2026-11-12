package stages

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestContextBudget_PinnedSnapshotNotLatest(t *testing.T) {
	slot := bot.NewLLMSlot(&bot.LLMSnapshot{MainDef: config.ModelDef{Model: "model-old", ContextLength: 4000, MaxTokens: 200}})
	stage := NewLLMStage("llm", nil, LLMConfig{
		Model: llm.ChatModel("model-old"),
		Compaction: &llm.CompactionConfig{
			Auto: true, ContextLength: 4000, OutputReserve: 200, TriggerRatio: 0.5, TailTokens: 10, MinMessagesToCompact: 1,
		},
	}, nil, zap.NewNop().Sugar())
	stage.SetCompactionLimits(func(ctx context.Context) (int, int) {
		snap := bot.LLMSnapshotFrom(ctx)
		if snap == nil {
			snap = slot.Current()
		}
		if snap == nil {
			return 0, 0
		}
		return snap.MainDef.ContextLength, snap.MainDef.MaxTokens
	})
	env := core.NewEnvelope(core.Message{ID: "m", BotID: "bot-synth"})
	pinned := bot.WithLLMSnapshot(context.Background(), slot.Current())
	slot.Publish(&bot.LLMSnapshot{MainDef: config.ModelDef{Model: "model-new", ContextLength: 32000, MaxTokens: 4000}})
	budget, _, ok := stage.contextBudgetUsage(pinned, env, "system", nil)
	if !ok {
		t.Fatal("budget unavailable")
	}
	latest, _, ok := stage.contextBudgetUsage(context.Background(), env, "system", nil)
	if !ok {
		t.Fatal("latest budget unavailable")
	}
	if budget.Window == latest.Window {
		t.Fatalf("pinned window %d equals latest %d", budget.Window, latest.Window)
	}
	if budget.Window > 8000 {
		t.Fatalf("pinned window followed the new model: %d", budget.Window)
	}
}
