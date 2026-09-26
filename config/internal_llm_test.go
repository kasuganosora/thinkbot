package config

import (
	"context"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestGetInternalLLMSettings(t *testing.T) {
	st := NewStore(nil)
	b := NewBuilder(st, nil)
	s := b.GetInternalLLMSettings()
	if s.DefaultReasoning != "auto" || s.DefaultMaxTokens != 0 || len(s.Reasoning) != 0 || len(s.MaxTokens) != 0 {
		t.Fatalf("defaults: %+v", s)
	}
	if err := st.SetMany(context.Background(), map[string]string{
		"llm.internal_reasoning.default":       "provider",
		"llm.internal_reasoning.memory_dedup":  "low",
		"llm.internal_max_tokens.memory_dedup": "16000",
		"llm.internal_max_tokens.default":      "64000",
	}); err != nil {
		t.Fatal(err)
	}
	s = b.GetInternalLLMSettings()
	if s.DefaultReasoning != "provider" || s.Reasoning[llm.PurposeMemoryDedup] != "low" ||
		s.MaxTokens[llm.PurposeMemoryDedup] != 16000 || s.DefaultMaxTokens != 64000 {
		t.Fatalf("configured: %+v", s)
	}
}

func TestInternalLLMMetaSpecs(t *testing.T) {
	specs := InternalLLMMetaSpecs()
	if len(specs) != 2+2*len(llm.InternalPurposes) {
		t.Fatalf("specs: %d", len(specs))
	}
	global := map[string]bool{}
	for _, s := range GlobalMetaSpecs() {
		global[s.Key] = true
	}
	for _, s := range specs {
		if err := ValidateKey(s.Key); err != nil {
			t.Fatal(err)
		}
		if !global[s.Key] {
			t.Fatalf("%s missing from the settings page", s.Key)
		}
		if !strings.HasPrefix(s.Key, "llm.internal_") {
			t.Fatalf("key %s", s.Key)
		}
	}
	if DefaultMap()["llm.internal_reasoning.default"] != "auto" {
		t.Fatal("default reasoning must be auto")
	}
}

func TestHasCapabilityReasoning(t *testing.T) {
	if !hasCapability([]string{"chat", "Reasoning"}, "reasoning") || hasCapability([]string{"chat"}, "reasoning") {
		t.Fatal("hasCapability")
	}
}

func TestResolveProviderModel_ReasoningCapability(t *testing.T) {
	st := NewStore(nil)
	if err := st.SetMany(context.Background(), map[string]string{
		"provider.p1": `{"enabled":true,"clientType":"OpenAI Compatible","baseUrl":"https://x","apiKey":"k","models":[` +
			`{"id":"o4-mini","capabilities":["chat","reasoning"],"maxTokens":100000},{"id":"gpt-4o","capabilities":["chat"]}]}`,
	}); err != nil {
		t.Fatal(err)
	}
	b := NewBuilder(st, nil)
	if d, ok := b.GetLLMModel("o4-mini"); !ok || !d.Reasoning {
		t.Fatalf("o4-mini: %+v %v", d.Reasoning, ok)
	}
	if d, ok := b.GetLLMModel("gpt-4o"); !ok || d.Reasoning {
		t.Fatalf("gpt-4o: %+v %v", d.Reasoning, ok)
	}
}
