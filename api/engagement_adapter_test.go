package api

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/llm"
)

type judgeMaxTokRecorder struct{ got []int }

func (r *judgeMaxTokRecorder) Name() string { return "rec" }
func (r *judgeMaxTokRecorder) DoGenerate(_ context.Context, p llm.GenerateParams) (*llm.GenerateResult, error) {
	v := -1
	if p.MaxTokens != nil {
		v = *p.MaxTokens
	}
	r.got = append(r.got, v)
	return &llm.GenerateResult{Text: `{"lazy":false}`, FinishReason: llm.FinishReasonStop}, nil
}
func (r *judgeMaxTokRecorder) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

// The engagement / lazy judges used a hardcoded max_tokens of 100, which a
// thinking model (glm-5.2) exhausts during reasoning. They must send the
// judge model's configured maxTokens.
func TestLLMJudgeAdapter_HonorsConfiguredModelMaxTokens(t *testing.T) {
	r := &judgeMaxTokRecorder{}
	if _, err := newLLMJudgeAdapter(r, "glm-5.2", 128000, nil, llm.PurposeEngagementJudge).Chat(context.Background(), "sys", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := newLLMJudgeAdapter(r, "unknown", 0, nil, llm.PurposeEngagementJudge).Chat(context.Background(), "sys", "user"); err != nil {
		t.Fatal(err)
	}
	if len(r.got) != 2 || r.got[0] != 128000 || r.got[1] != llm.DefaultMaxOutputTokens {
		t.Fatalf("max_tokens=%v, want [128000 %d]", r.got, llm.DefaultMaxOutputTokens)
	}
	j := NewLazyLLMJudge(r, "glm-5.2", 64000, nil).(*lazyLLMJudge)
	if _, err := j.client.Chat(context.Background(), "sys", "user"); err != nil {
		t.Fatal(err)
	}
	if r.got[2] != 64000 {
		t.Fatalf("lazy judge max_tokens=%d, want 64000", r.got[2])
	}
}

type judgeEffortRecorder struct{ efforts []string }

func (r *judgeEffortRecorder) Name() string { return "rec" }
func (r *judgeEffortRecorder) DoGenerate(_ context.Context, p llm.GenerateParams) (*llm.GenerateResult, error) {
	e := "<unset>"
	if p.ReasoningEffort != nil {
		e = *p.ReasoningEffort
	}
	r.efforts = append(r.efforts, e)
	return &llm.GenerateResult{Text: `{"lazy":false}`, FinishReason: llm.FinishReasonStop}, nil
}
func (r *judgeEffortRecorder) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

// Judges are classifiers: reasoning_effort none where the model accepts it
// (light glm-5.2), low on glm-5.3 (which rejects none), nothing at all when
// neither the bot nor the model says the parameter is supported.
func TestLLMJudges_ReasoningPolicy(t *testing.T) {
	r := &judgeEffortRecorder{}
	prod := newInternalPolicy(nil, nil, "medium", nil)
	ctx := context.Background()
	_, _ = newLLMJudgeAdapter(r, "glm-5.2", 128000, prod, llm.PurposeEngagementJudge).Chat(ctx, "s", "u")
	_, _ = NewLazyLLMJudge(r, "glm-5.2", 128000, prod).(*lazyLLMJudge).client.Chat(ctx, "s", "u")
	_, _ = NewLazyLLMJudge(r, "glm-5.3", 128000, prod).(*lazyLLMJudge).client.Chat(ctx, "s", "u")
	_, _ = NewLazyLLMJudge(r, "gpt-4o", 16384, newInternalPolicy(nil, nil, "", nil)).(*lazyLLMJudge).client.Chat(ctx, "s", "u")
	want := []string{"none", "none", "low", "<unset>"}
	for i, w := range want {
		if r.efforts[i] != w {
			t.Fatalf("call %d: effort %q, want %q (all: %v)", i, r.efforts[i], w, r.efforts)
		}
	}
}

// Models with the "reasoning" capability in the provider config may receive
// the parameter even when the bot sends none.
func TestNewInternalPolicy_UsesReasoningCapability(t *testing.T) {
	b := &bot.LLMBundle{}
	b.MainDef.Model, b.MainDef.Reasoning = "o4-mini", true
	b.LightDef.Model = "gpt-4o-mini"
	p := newInternalPolicy(nil, nil, "", b)
	if got := p.ReasoningEffort(llm.PurposeMemoryDedup, "o4-mini"); got != "low" {
		t.Fatalf("reasoning-capable main: %q", got)
	}
	if got := p.ReasoningEffort(llm.PurposeLazyJudge, "gpt-4o-mini"); got != "" {
		t.Fatalf("light without capability: %q", got)
	}
}
