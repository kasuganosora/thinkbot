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
	mu  sync.Mutex
	got []int
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
