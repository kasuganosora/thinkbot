package stages

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kasuganosora/thinkbot/llm"
)

// prodLikeHistory mimics the first production call (2026-09-26): 21 short
// Telegram messages, ~3k estimated tokens.
func prodLikeHistory() []llm.Message {
	var out []llm.Message
	body := strings.Repeat("聊天内容", 35)
	for i := 0; i < 21; i++ {
		if i%2 == 0 {
			out = append(out, llm.UserMessage(body))
		} else {
			out = append(out, llm.AssistantMessage(body))
		}
	}
	return out
}

func TestCompactContext_CostBenefitGate(t *testing.T) {
	p := &selfCompactSummaryProvider{text: "S"}
	s := newSelfCompactStage(p, &SelfCompactConfig{})

	// Production case: short history → no-op before any LLM call.
	h := prodLikeHistory()
	if est := llm.EstimateMessagesTokens(h); est >= defaultSelfCompactMinTokens {
		t.Fatalf("fixture too large: %d", est)
	}
	ctx, lc := execCtxWith(h)
	out, err := s.newCompactContextTool(webEnv("sess-a"), nil).Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if res := out.(map[string]any); res["status"] != "noop" || !strings.Contains(res["reason"].(string), "already short") {
		t.Fatalf("want short-history noop, got %+v", res)
	}
	if p.calls != 0 || lc.Compacted() {
		t.Fatal("no-op must not call the summarizer")
	}

	// Large context, but keep_recent leaves only a small head → "not worth it".
	big := longHistory(20)
	ctx2, lc2 := execCtxWith(big)
	out, err = s.newCompactContextTool(webEnv("sess-b"), nil).Execute(ctx2, map[string]any{"keep_recent": float64(16)})
	if err != nil {
		t.Fatal(err)
	}
	res := out.(map[string]any)
	if res["status"] != "noop" || !strings.Contains(res["reason"].(string), "not worth it") ||
		!strings.Contains(res["reason"].(string), "summary call itself costs") {
		t.Fatalf("want cost/benefit noop, got %+v", res)
	}
	if p.calls != 0 || lc2.Compacted() {
		t.Fatal("refused compaction must not call the summarizer")
	}

	// Same history with the default keep_recent is worth it.
	ctx3, lc3 := execCtxWith(big)
	out, err = s.newCompactContextTool(webEnv("sess-c"), nil).Execute(ctx3, map[string]any{})
	if err != nil || out.(map[string]any)["status"] != "compacted" || !lc3.Compacted() {
		t.Fatalf("expected compaction: %v %+v", err, out)
	}
}

func TestEstimateSelfCompactBenefit(t *testing.T) {
	cfg := &SelfCompactConfig{}
	b := estimateSelfCompactBenefit(1400, cfg)
	if b.summaryTokens != minSelfCompactSummaryTokens || b.savings != 1100 {
		t.Fatalf("small head: %+v", b)
	}
	if b.refusal(3066, cfg) == "" {
		t.Fatal("the 2026-09-26 case (save ~1.1k of ~3k) must be refused")
	}
	b = estimateSelfCompactBenefit(20000, cfg)
	if b.summaryTokens != maxSelfCompactSummaryTokens || b.refusal(26000, cfg) != "" {
		t.Fatalf("large head should pass: %+v", b)
	}
	// Relative threshold: 5k saving on a 40k context is below 30%.
	b = estimateSelfCompactBenefit(6000, cfg)
	if b.refusal(40000, cfg) == "" {
		t.Fatal("saving below the ratio must be refused")
	}
	if b.costOutputCap != defaultSelfCompactSummaryMaxTokens || b.targetWords <= 0 {
		t.Fatalf("cost fields: %+v", b)
	}
}

func TestCompactContext_SummaryNotSmallerIsRejected(t *testing.T) {
	p := &selfCompactSummaryProvider{text: strings.Repeat("very long summary text ", 3000)}
	store := &memCheckpointStore{}
	s := newSelfCompactStage(p, &SelfCompactConfig{Store: store})
	ctx, lc := execCtxWith(longHistory(20))
	out, err := s.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if res := out.(map[string]any); res["status"] != "noop" || !strings.Contains(res["reason"].(string), "not meaningfully smaller") {
		t.Fatalf("got %+v", res)
	}
	if lc.Compacted() || len(store.saved) != 0 {
		t.Fatal("an oversized summary must not replace history")
	}
}

func TestCompactContext_UsesConfiguredSummarizerAndOptions(t *testing.T) {
	mainP := &selfCompactSummaryProvider{text: "main"}
	light := &recordingSummaryProvider{text: "## Topics\n- light"}
	s := newSelfCompactStage(mainP, &SelfCompactConfig{
		SummaryProvider:        light,
		SummaryModel:           llm.ChatModel("light-model"),
		SummaryMaxTokens:       1500,
		SummaryReasoningEffort: "low",
	})
	ctx, _ := execCtxWith(longHistory(20))
	out, err := s.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx, map[string]any{})
	if err != nil || out.(map[string]any)["status"] != "compacted" {
		t.Fatalf("%v %+v", err, out)
	}
	if mainP.calls != 0 || light.calls != 1 {
		t.Fatalf("summarizer override not used: main=%d light=%d", mainP.calls, light.calls)
	}
	if light.params.Model == nil || light.params.Model.ID != "light-model" ||
		light.params.MaxTokens == nil || *light.params.MaxTokens != 1500 ||
		light.params.ReasoningEffort == nil || *light.params.ReasoningEffort != "low" ||
		light.params.System != llm.SelfCompactSystemPrompt {
		t.Fatalf("summarizer params wrong: %+v", light.params)
	}
}

func TestCompactContext_TruncatedSummaryLeavesContext(t *testing.T) {
	p := &recordingSummaryProvider{text: "## Topics\n- cut", finish: llm.FinishReasonLength}
	store := &memCheckpointStore{}
	s := newSelfCompactStage(p, &SelfCompactConfig{Store: store})
	ctx, lc := execCtxWith(longHistory(20))
	_, err := s.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "cut off") {
		t.Fatalf("want truncation error, got %v", err)
	}
	if lc.Compacted() || len(store.saved) != 0 {
		t.Fatal("truncated summary must not be applied or persisted")
	}
}

// --- observability ------------------------------------------------------------

func TestCompactContext_LogsFullArguments(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	s := NewLLMStage("llm", &selfCompactSummaryProvider{text: "S"}, LLMConfig{
		Model: llm.ChatModel("m"), MaxSteps: 10, SelfCompact: &SelfCompactConfig{},
	}, nil, zap.New(core).Sugar())
	focus := strings.Repeat("branch fix/social-intent-grounding, path llm/intent_judge.go; ", 12) // > 300 runes
	ctx, _ := execCtxWith(longHistory(20))
	if _, err := s.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx, map[string]any{"keep_recent": float64(5), "focus": focus}); err != nil {
		t.Fatal(err)
	}
	entries := logs.FilterMessage("context_compact").All()
	if len(entries) != 1 {
		t.Fatalf("want one context_compact line, got %d", len(entries))
	}
	f := entries[0].ContextMap()
	if f["status"] != "compacted" || f["focus"] != strings.TrimSpace(focus) || f["keep_recent"] != int64(5) {
		t.Fatalf("full args missing from log: status=%v keep=%v focus_len=%d", f["status"], f["keep_recent"], len(f["focus"].(string)))
	}
}

type recordingSummaryProvider struct {
	calls  int
	params llm.GenerateParams
	text   string
	finish llm.FinishReason
}

func (p *recordingSummaryProvider) Name() string { return "rec" }
func (p *recordingSummaryProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.calls++
	p.params = params
	return &llm.GenerateResult{Text: p.text, FinishReason: p.finish}, nil
}
func (p *recordingSummaryProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("not implemented")
}
