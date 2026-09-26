package api

import (
	"context"
	"testing"

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
	if _, err := newLLMJudgeAdapter(r, "glm-5.2", 128000).Chat(context.Background(), "sys", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := newLLMJudgeAdapter(r, "unknown", 0).Chat(context.Background(), "sys", "user"); err != nil {
		t.Fatal(err)
	}
	if len(r.got) != 2 || r.got[0] != 128000 || r.got[1] != llm.DefaultMaxOutputTokens {
		t.Fatalf("max_tokens=%v, want [128000 %d]", r.got, llm.DefaultMaxOutputTokens)
	}
	j := NewLazyLLMJudge(r, "glm-5.2", 64000).(*lazyLLMJudge)
	if _, err := j.client.Chat(context.Background(), "sys", "user"); err != nil {
		t.Fatal(err)
	}
	if r.got[2] != 64000 {
		t.Fatalf("lazy judge max_tokens=%d, want 64000", r.got[2])
	}
}
