package memory

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"

	"github.com/kasuganosora/thinkbot/llm"
)

type policyCall struct {
	maxTokens int
	effort    string // "" = not sent
}

// policyRecorder records max_tokens / reasoning_effort per call and answers
// with a fixed text and finish reason.
type policyRecorder struct {
	mu       sync.Mutex
	response string
	finish   llm.FinishReason
	calls    []policyCall
}

func (m *policyRecorder) Name() string { return "policy-rec" }
func (m *policyRecorder) DoGenerate(_ context.Context, p llm.GenerateParams) (*llm.GenerateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := policyCall{maxTokens: -1}
	if p.MaxTokens != nil {
		c.maxTokens = *p.MaxTokens
	}
	if p.ReasoningEffort != nil {
		c.effort = *p.ReasoningEffort
	}
	m.calls = append(m.calls, c)
	fin := m.finish
	if fin == "" {
		fin = llm.FinishReasonStop
	}
	return &llm.GenerateResult{Text: m.response, FinishReason: fin}, nil
}
func (m *policyRecorder) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}
func (m *policyRecorder) last(t *testing.T) policyCall {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		t.Fatal("no LLM call")
	}
	return m.calls[len(m.calls)-1]
}

var prodPolicy = llm.NewInternalPolicy(nil, "medium") // bot 栞娜: reasoning_effort=medium

// memory_dedup was the most expensive internal call (09-26: 32 calls, 242K
// output tokens) because GLM-5.3 reasons at "max" without the parameter.
func TestClusterMerge_ReasoningPolicy(t *testing.T) {
	p := &policyRecorder{response: "[]"}
	in := []ClusterInput{{ID: "a", Content: "x"}, {ID: "b", Content: "y"}}
	if _, err := ClusterMerge(context.Background(), p, llm.ChatModel("glm-5.3"), "", in, 128000, prodPolicy); err != nil {
		t.Fatal(err)
	}
	if c := p.last(t); c.effort != "low" || c.maxTokens != 128000 {
		t.Fatalf("memory_dedup: %+v, want low/128000", c)
	}
	// Bot without reasoning_effort on a model with no known support → not sent.
	if _, err := ClusterMerge(context.Background(), p, llm.ChatModel("gpt-4o"), "", in, 128000, llm.NewInternalPolicy(nil, "")); err != nil {
		t.Fatal(err)
	}
	if c := p.last(t); c.effort != "" {
		t.Fatalf("must not send reasoning_effort: %+v", c)
	}
	// Operator cap for memory_dedup.
	set := llm.InternalSettings{MaxTokens: map[string]int{llm.PurposeMemoryDedup: 16000}}
	capped := llm.NewInternalPolicy(func() llm.InternalSettings { return set }, "medium")
	if _, err := ClusterMerge(context.Background(), p, llm.ChatModel("glm-5.3"), "", in, 128000, capped); err != nil {
		t.Fatal(err)
	}
	if c := p.last(t); c.maxTokens != 16000 {
		t.Fatalf("cap: %+v", c)
	}
}

func TestProfilers_ReasoningPolicy(t *testing.T) {
	p := &policyRecorder{response: "[]"}
	cfg := DefaultLLMProfilerConfig()
	cfg.Provider, cfg.Model, cfg.MaxTokens, cfg.Policy = p, llm.ChatModel("glm-5.3"), 128000, prodPolicy
	if _, err := NewLLMProfiler(cfg, testTracerProvider(), testLogger()).callLLM(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if c := p.last(t); c.effort != "low" || c.maxTokens != 128000 {
		t.Fatalf("user_profiler: %+v", c)
	}

	bp := NewBotProfileProfiler(BotProfileProfilerConfig{
		Provider: p, Model: llm.ChatModel("glm-5.3"), MaxTokens: 128000, Policy: prodPolicy,
	}, testTracerProvider(), testLogger())
	_, _ = bp.ExtractProfile(context.Background(), []TieredEntry{{Entry: Entry{ID: "1", Content: "我喜欢写 Go"}, Tier: Tier1LongTerm}}, nil, nil)
	if c := p.last(t); c.effort != "low" || c.maxTokens != 128000 {
		t.Fatalf("bot_profiler: %+v", c)
	}
}

func newPolicyDreamManager(p llm.Provider, pol *llm.InternalPolicy) *DreamManager {
	cfg := DefaultDreamConfig()
	cfg.Enabled = true
	cfg.Model = "glm-5.3"
	cfg.MaxDreamTokens = 128000
	cfg.Policy = pol
	tm := NewTieredManager(TieredManagerConfig{Store: NewTieredStore(nil)}, noop.NewTracerProvider(), testDreamLogger())
	return NewDreamManager(cfg, tm, p, noop.NewTracerProvider(), testDreamLogger())
}

// dream_score hardcoded max_tokens 2048: on 09-24 and 09-26 the output was
// exactly 2048 tokens (cut off), so scoring always fell back to the heuristic.
func TestDreamScore_UsesModelLimitAndScorerReasoning(t *testing.T) {
	p := &policyRecorder{response: `[{"key":"k1","importance":0.9}]`}
	d := newPolicyDreamManager(p, prodPolicy)
	out := d.scoreImportanceOnce(context.Background(), []*DreamCandidate{{Key: "k1", Content: "用户使用 Go"}})
	if out["k1"] != 0.9 {
		t.Fatalf("scores: %v", out)
	}
	if c := p.last(t); c.maxTokens != 128000 || c.effort != "low" {
		t.Fatalf("dream_score: %+v, want 128000 (not 2048) and low (none → low on glm-5.3)", c)
	}
	// Light model glm-5.2 accepts none → scorer runs without thinking.
	d.model = "glm-5.2"
	_ = d.scoreImportanceOnce(context.Background(), []*DreamCandidate{{Key: "k1", Content: "x"}})
	if c := p.last(t); c.effort != "none" {
		t.Fatalf("dream_score on glm-5.2: %+v", c)
	}
	// A cut-off response is detected (logged) and falls back without panicking.
	p.finish, p.response = llm.FinishReasonLength, `[{"key":"k1","importa`
	if out := d.scoreImportanceOnce(context.Background(), []*DreamCandidate{{Key: "k1", Content: "x"}}); out != nil {
		t.Fatalf("truncated JSON must fall back to heuristic, got %v", out)
	}
}

func TestDreamExtractAndCluster_ReasoningPolicy(t *testing.T) {
	p := &policyRecorder{response: `[{"content":"用户使用 Go","category":"fact"}]`}
	d := newPolicyDreamManager(p, prodPolicy)
	_ = d.extractCandidatesOnce(context.Background(), []rawSnippet{{content: "我平时用 Go 写后端", sourceID: "s1"}})
	if c := p.last(t); c.effort != "low" || c.maxTokens != 128000 {
		t.Fatalf("dream_extract: %+v", c)
	}
	p.response = `[{"key":"k1","tags":["编程"]}]`
	_ = d.clusterByTheme(context.Background(), []*DreamCandidate{{Key: "k1", Content: "用户使用 Go"}})
	if c := p.last(t); c.effort != "low" || c.maxTokens != 128000 { // classifier: none → low on glm-5.3
		t.Fatalf("dream_cluster: %+v", c)
	}
	// No policy → no reasoning_effort (previous behaviour).
	d2 := newPolicyDreamManager(p, nil)
	_ = d2.clusterByTheme(context.Background(), []*DreamCandidate{{Key: "k1", Content: "x"}})
	if c := p.last(t); c.effort != "" {
		t.Fatalf("nil policy: %+v", c)
	}
}
