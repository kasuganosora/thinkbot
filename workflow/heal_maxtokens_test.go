package workflow

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/subagent"
)

type maxTokStreamRecorder struct {
	mu      sync.Mutex
	got     []int
	efforts []string
}

func (p *maxTokStreamRecorder) Name() string { return "rec" }
func (p *maxTokStreamRecorder) DoGenerate(context.Context, llm.GenerateParams) (*llm.GenerateResult, error) {
	return &llm.GenerateResult{FinishReason: llm.FinishReasonStop}, nil
}
func (p *maxTokStreamRecorder) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	p.mu.Lock()
	v := -1
	if params.MaxTokens != nil {
		v = *params.MaxTokens
	}
	p.got = append(p.got, v)
	e := ""
	if params.ReasoningEffort != nil {
		e = *params.ReasoningEffort
	}
	p.efforts = append(p.efforts, e)
	p.mu.Unlock()
	ch := make(chan llm.StreamPart, 1)
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
		case ch <- &llm.TextDeltaPart{Text: `<result>{"category":"transient","confidence":0.9,"reason":"x","suggestedAction":"retry"}</result>`}:
		}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}

// The healing diagnosis used a hardcoded max_tokens of 2000; it must follow
// the model-driven AnalyzerMaxTokens like the other analyzer calls.
func TestDiagnoseNode_UsesModelDrivenMaxTokens(t *testing.T) {
	p := &maxTokStreamRecorder{}
	saMgr := subagent.NewSubAgentManager(p, "test-model")
	a := NewAnalyzer(saMgr, nil, EngineConfig{AnalyzerMaxTokens: 128000, AnalyzerStuckTimeout: 5 * time.Second}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = a.DiagnoseNode(ctx, &DAGNode{ID: "n1", Name: "n", Task: "t", Error: "boom"}, &Workflow{Requirement: "r"})
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.got) == 0 || p.got[0] != 128000 {
		t.Fatalf("diagnosis max_tokens=%v, want 128000", p.got)
	}
}

// The healing diagnosis runs with the workflow_heal reasoning policy (low on
// GLM-5.3 instead of the provider default "max") and its optional output cap.
func TestDiagnoseNode_ReasoningPolicy(t *testing.T) {
	run := func(pol *llm.InternalPolicy, model string) (int, string) {
		p := &maxTokStreamRecorder{}
		saMgr := subagent.NewSubAgentManager(p, model)
		a := NewAnalyzer(saMgr, nil, EngineConfig{AnalyzerMaxTokens: 128000, AnalyzerStuckTimeout: 5 * time.Second,
			InternalPolicy: pol, AnalyzerModel: model}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = a.DiagnoseNode(ctx, &DAGNode{ID: "n1", Name: "n", Task: "t", Error: "boom"}, &Workflow{Requirement: "r"})
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.got) == 0 {
			t.Fatal("no LLM call")
		}
		return p.got[0], p.efforts[0]
	}
	if mt, e := run(llm.NewInternalPolicy(nil, "medium"), "glm-5.3"); mt != 128000 || e != "low" {
		t.Fatalf("prod: max=%d effort=%q", mt, e)
	}
	set := llm.InternalSettings{MaxTokens: map[string]int{llm.PurposeWorkflowHeal: 8000}}
	if mt, _ := run(llm.NewInternalPolicy(func() llm.InternalSettings { return set }, "medium"), "glm-5.3"); mt != 8000 {
		t.Fatalf("cap: %d", mt)
	}
	if _, e := run(llm.NewInternalPolicy(nil, ""), "gpt-4o"); e != "" {
		t.Fatalf("must not send: %q", e)
	}
	if _, e := run(nil, "glm-5.3"); e != "" {
		t.Fatalf("nil policy: %q", e)
	}
}
