package stages

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestSummaryBudget(t *testing.T) {
	cases := []struct {
		name                           string
		modelMax, opCap, ctxLen, input int
		first, retry                   int
	}{
		// prod: glm-5.3 configured maxTokens 128000 / contextLength 1,000,000,
		// no operator cap → the configured limit, no retry needed.
		{"prod glm-5.3", 128000, 0, 1000000, 30000, 128000, 128000},
		{"model limit unknown", 0, 0, 0, 30000, fallbackSelfCompactSummaryMaxTokens, fallbackSelfCompactSummaryMaxTokens},
		{"small model", 8192, 0, 0, 30000, 8192, 8192},
		// operator cap only lowers; retry doubles up to the model limit
		{"operator cap", 128000, 16384, 1000000, 30000, 16384, 32768},
		{"operator cap near model", 20000, 16384, 0, 30000, 16384, 20000},
		{"operator cap above model", 8192, 50000, 0, 30000, 8192, 8192},
		{"operator cap tiny", 128000, 500, 0, 30000, minSelfCompactSummaryBudget, 2 * minSelfCompactSummaryBudget},
		{"cap w/o model limit", 0, 1500, 0, 30000, 1500, 3000},
		// context fit: 200k window, ~150k prompt → 200000-150000-2048
		{"context fit", 128000, 0, 200000, 150000, 47952, 47952},
		{"context fit + cap", 128000, 16384, 200000, 150000, 16384, 32768},
	}
	for _, c := range cases {
		b := summaryBudget(c.modelMax, c.opCap, c.ctxLen, c.input)
		if b.first != c.first || b.retry != c.retry {
			t.Errorf("%s: summaryBudget(%d,%d,%d,%d)=%+v, want first=%d retry=%d",
				c.name, c.modelMax, c.opCap, c.ctxLen, c.input, b, c.first, c.retry)
		}
	}
}

func TestSummaryReasoningEffort(t *testing.T) {
	cases := []struct{ configured, bot, want string }{
		{"", "medium", "low"}, // prod: bot on medium → summary on low
		{"auto", "high", "low"},
		{"", "max", "low"},
		{"", "", ""}, // bot never sends the param → neither does the summary
		{"", "minimal", "minimal"},
		{"", "none", "none"},
		{"AUTO", "Low", "low"},
		{"provider", "medium", ""},
		{"none", "", "none"}, // explicit value passes through
		{"high", "medium", "high"},
	}
	for _, c := range cases {
		if got := summaryReasoningEffort(c.configured, c.bot); got != c.want {
			t.Errorf("summaryReasoningEffort(%q,%q)=%q, want %q", c.configured, c.bot, got, c.want)
		}
	}
}

// cutoffProvider simulates glm-5.3 on a long transcript: every response stops
// at the output cap (finish_reason=length) until `okAfter` calls happened.
type cutoffProvider struct {
	mu      sync.Mutex
	caps    []int
	efforts []string
	okAfter int // 0 → never ok
}

func (p *cutoffProvider) Name() string { return "cutoff" }
func (p *cutoffProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, e := 0, ""
	if params.MaxTokens != nil {
		c = *params.MaxTokens
	}
	if params.ReasoningEffort != nil {
		e = *params.ReasoningEffort
	}
	p.caps = append(p.caps, c)
	p.efforts = append(p.efforts, e)
	if p.okAfter > 0 && len(p.caps) > p.okAfter {
		return &llm.GenerateResult{Text: "## Topics\n- ok", FinishReason: llm.FinishReasonStop}, nil
	}
	return &llm.GenerateResult{Text: "## Topics\n- cut", FinishReason: llm.FinishReasonLength}, nil
}
func (p *cutoffProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("not implemented")
}
func (p *cutoffProvider) calls() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.caps) }

func newProdLikeCompactStage(p llm.Provider, cfg *SelfCompactConfig) *LLMStage {
	maxTok := 128000
	return NewLLMStage("llm", p, LLMConfig{
		Model:           llm.ChatModel("glm-5.3"),
		MaxSteps:        10,
		MaxTokens:       &maxTok,
		ReasoningEffort: "medium",
		SelfCompact:     cfg,
	}, nil, nil)
}

// Root cause of 2026-09-26: the summary call sent a hardcoded 4096 instead of
// the model's configured maxTokens (glm-5.3: 128000). It must send exactly
// the configured model limit (the value the chat path sends) when no operator
// cap is set.
func TestCompactContext_HonorsConfiguredModelMaxTokens(t *testing.T) {
	p := &cutoffProvider{} // always cut off
	ok := &recordingSummaryProvider{text: "## Topics\n- ok"}
	s := newProdLikeCompactStage(ok, &SelfCompactConfig{})
	ctx, lc := execCtxWith(longHistory(20))
	out, err := s.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx, map[string]any{})
	if err != nil || out.(map[string]any)["status"] != "compacted" || !lc.Compacted() {
		t.Fatalf("%v %+v", err, out)
	}
	if ok.calls != 1 || ok.params.MaxTokens == nil || *ok.params.MaxTokens != 128000 {
		t.Fatalf("summary must send the configured model maxTokens 128000, got calls=%d params=%v", ok.calls, ok.params.MaxTokens)
	}
	if ok.params.ReasoningEffort == nil || *ok.params.ReasoningEffort != "low" {
		t.Fatalf("summary call must lower reasoning to low for a bot on medium: %v", ok.params.ReasoningEffort)
	}
	// Cut off even at the model limit → no pointless retry at the same cap.
	s2 := newProdLikeCompactStage(p, &SelfCompactConfig{})
	ctx2, lc2 := execCtxWith(longHistory(20))
	if _, err := s2.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx2, map[string]any{}); err == nil || lc2.Compacted() {
		t.Fatalf("truncated summary must fail and leave the context unchanged: %v", err)
	}
	if len(p.caps) != 1 || p.caps[0] != 128000 {
		t.Fatalf("want one call at 128000, got %v", p.caps)
	}
}

// An explicit operator cap (agent.self_compact.summary_max_tokens) lowers the
// first attempt; on truncation it retries once at 2x, bounded by the model.
func TestCompactContext_OperatorCapAndRetry(t *testing.T) {
	p := &cutoffProvider{okAfter: 1} // first call cut off, retry succeeds
	s := newProdLikeCompactStage(p, &SelfCompactConfig{SummaryMaxTokens: 16384})
	ctx, lc := execCtxWith(longHistory(20))
	out, err := s.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx, map[string]any{})
	if err != nil || out.(map[string]any)["status"] != "compacted" || !lc.Compacted() {
		t.Fatalf("retry with a larger cap should compact: %v %+v", err, out)
	}
	if len(p.caps) != 2 || p.caps[0] != 16384 || p.caps[1] != 32768 {
		t.Fatalf("want caps [16384 32768], got %v", p.caps)
	}
	if p.efforts[0] != "low" || p.efforts[1] != "low" {
		t.Fatalf("efforts: %v", p.efforts)
	}
}

// The context window of the model (ModelDef.ContextLength) bounds the output
// budget: prompt + max_tokens must fit.
func TestCompactContext_FitsOutputIntoContextWindow(t *testing.T) {
	ok := &recordingSummaryProvider{text: "## Topics\n- ok"}
	maxTok := 128000
	s := NewLLMStage("llm", ok, LLMConfig{
		Model:         llm.ChatModel("glm-5.3"),
		MaxSteps:      10,
		MaxTokens:     &maxTok,
		ContextLength: 131072,
		SelfCompact:   &SelfCompactConfig{},
	}, nil, nil)
	ctx, _ := execCtxWith(longHistory(20))
	if _, err := s.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	got := *ok.params.MaxTokens
	if got >= 128000 || got < 100000 {
		t.Fatalf("budget should be fitted below the 131072 window minus the prompt, got %d", got)
	}
}

// Reproduces 2026-09-26 18:58-18:59: the model called compact_context three
// times in one turn after each failed. Now a failure (incl. its one internal
// retry) blocks further calls this turn and pauses the next turns.
func TestCompactContext_FailureBacksOffAndCapsAttemptsPerTurn(t *testing.T) {
	p := &cutoffProvider{}
	store := &memCheckpointStore{}
	s := newProdLikeCompactStage(p, &SelfCompactConfig{Store: store, FailureBackoff: 5 * time.Minute})
	env := webEnv("sess-1")
	tool := s.newCompactContextTool(env, nil) // one turn = one tool instance

	ctx, lc := execCtxWith(longHistory(20))
	_, err := tool.Execute(ctx, map[string]any{"keep_recent": float64(4)})
	if err == nil || !strings.Contains(err.Error(), "cut off") ||
		!strings.Contains(err.Error(), "Do NOT call compact_context again") || !strings.Contains(err.Error(), "5m0s") {
		t.Fatalf("want a clear failure result, got %v", err)
	}
	if p.calls() != 1 {
		t.Fatalf("one summarization at the model limit (no retry possible), got %d calls", p.calls())
	}
	if lc.Compacted() || len(store.saved) != 0 {
		t.Fatal("failed summary must leave the context unchanged")
	}

	// Same turn, model retries anyway → refused without an LLM call.
	ctx2, _ := execCtxWith(longHistory(20))
	_, err = tool.Execute(ctx2, map[string]any{"keep_recent": float64(2)})
	if err == nil || !strings.Contains(err.Error(), "already failed in this turn") {
		t.Fatalf("second call in the same turn must be refused: %v", err)
	}
	// Next turn within the backoff → refused without an LLM call.
	ctx3, _ := execCtxWith(longHistory(20))
	_, err = s.newCompactContextTool(env, nil).Execute(ctx3, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "paused after a recent failed attempt") {
		t.Fatalf("next turn within the backoff must be refused: %v", err)
	}
	if p.calls() != 1 {
		t.Fatalf("refusals must not call the summarizer, calls=%d", p.calls())
	}
	// Other conversations are not affected.
	ctx4, _ := execCtxWith(longHistory(20))
	if _, err := s.newCompactContextTool(webEnv("sess-2"), nil).Execute(ctx4, map[string]any{}); err == nil ||
		!strings.Contains(err.Error(), "cut off") {
		t.Fatalf("other conversation should run its own attempt (and fail on the cutoff provider): %v", err)
	}

	// Backoff expired → allowed again; a success clears the failure state.
	s.selfCompactFail.mu.Lock()
	st := s.selfCompactFail.m["chat:sess-1"]
	st.until = time.Now().Add(-time.Second)
	s.selfCompactFail.m["chat:sess-1"] = st
	s.selfCompactFail.mu.Unlock()
	p.mu.Lock()
	p.okAfter = len(p.caps) // next call succeeds
	p.mu.Unlock()
	ctx5, lc5 := execCtxWith(longHistory(20))
	out, err := s.newCompactContextTool(env, nil).Execute(ctx5, map[string]any{})
	if err != nil || out.(map[string]any)["status"] != "compacted" || !lc5.Compacted() {
		t.Fatalf("after the backoff: %v %+v", err, out)
	}
	if r := s.selfCompactFail.remaining("chat:sess-1", time.Now()); r != 0 {
		t.Fatalf("success must clear the failure backoff, remaining=%s", r)
	}
}

func TestSelfCompactFailures_ExponentialBackoff(t *testing.T) {
	var f selfCompactFailures
	now := time.Now()
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := f.record("k", 5*time.Minute, now); got != w {
			t.Fatalf("failure %d: backoff %s, want %s", i+1, got, w)
		}
	}
	if r := f.remaining("k", now); r != time.Hour {
		t.Fatalf("remaining=%s", r)
	}
	f.clear("k")
	if f.remaining("k", now) != 0 {
		t.Fatal("clear must reset")
	}
}

func TestCompactContext_LightSummarizerUsesItsOwnModelCap(t *testing.T) {
	light := &cutoffProvider{okAfter: 5}
	s := newProdLikeCompactStage(&cutoffProvider{}, &SelfCompactConfig{
		SummaryProvider:          light,
		SummaryModel:             llm.ChatModel("glm-5.2"),
		SummaryModelMaxTokens:    8192,
		SummaryReasoningFallback: "medium",
	})
	ctx, _ := execCtxWith(longHistory(20))
	_, _ = s.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx, map[string]any{})
	if len(light.caps) != 1 || light.caps[0] != 8192 || light.efforts[0] != "low" {
		t.Fatalf("light summarizer: caps=%v efforts=%v (want one call at its 8192 cap, no retry, effort low)", light.caps, light.efforts)
	}

	// prod: light glm-5.2 configured maxTokens 128000 → that limit, not the main one or 4096.
	light2 := &recordingSummaryProvider{text: "## Topics\n- ok"}
	s2 := newProdLikeCompactStage(&cutoffProvider{}, &SelfCompactConfig{
		SummaryProvider:           light2,
		SummaryModel:              llm.ChatModel("glm-5.2"),
		SummaryModelMaxTokens:     128000,
		SummaryModelContextLength: 1000000,
	})
	ctx2, _ := execCtxWith(longHistory(20))
	if _, err := s2.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx2, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if light2.calls != 1 || *light2.params.MaxTokens != 128000 {
		t.Fatalf("light summarizer must use its configured 128000, got %v", *light2.params.MaxTokens)
	}
}
