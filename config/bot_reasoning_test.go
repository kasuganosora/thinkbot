package config

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestBotInternalLLMSettingsOverrides(t *testing.T) {
	st := NewStore(nil)
	b := NewBuilder(st, nil)
	ctx := context.Background()
	_ = st.Set(ctx, "llm.internal_reasoning.memory_dedup", "high")
	_ = st.Set(ctx, BotReasoningKey("b1", llm.PurposeMemoryDedup), "low")
	_ = st.Set(ctx, BotReasoningKey("b1", "dream"), "provider")
	_ = st.Set(ctx, BotReasoningKey("b1", "heartbeat"), "none")

	s := b.GetBotInternalLLMSettings("b1")
	if s.Reasoning[llm.PurposeMemoryDedup] != "low" {
		t.Fatalf("bot override must win: %+v", s.Reasoning)
	}
	for _, p := range []string{llm.PurposeDreamExtract, llm.PurposeDreamCluster, llm.PurposeDreamScore} {
		if s.Reasoning[p] != "provider" {
			t.Fatalf("dream applies to %s: %+v", p, s.Reasoning)
		}
	}
	if _, ok := s.Reasoning["heartbeat"]; ok {
		t.Fatal("heartbeat is not an internal purpose")
	}
	if other := b.GetBotInternalLLMSettings("b2"); other.Reasoning[llm.PurposeMemoryDedup] != "high" {
		t.Fatalf("other bots keep the global value: %+v", other.Reasoning)
	}
	if got := b.GetBotReasoning("b1")["heartbeat"]; got != "none" {
		t.Fatalf("heartbeat setting: %q", got)
	}
	for _, v := range []string{"", "provider", "none", "low", "medium", "high"} {
		if !ValidBotReasoning(v) {
			t.Fatalf("%q must be valid", v)
		}
	}
	if ValidBotReasoning("ultra") {
		t.Fatal("ultra must be invalid")
	}
}
