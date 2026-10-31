package llm

import (
	"context"
	"sort"
	"sync"
	"testing"
)

func TestIncToolTurnBudget_SharedTurn(t *testing.T) {
	ctx := WithToolTurnBudget(context.Background())
	if IncToolTurnBudget(ctx, "memory_deep_recall") != 1 {
		t.Fatal("first count")
	}
	if IncToolTurnBudget(ctx, "memory_deep_recall") != 2 {
		t.Fatal("second count")
	}
	other := WithToolTurnBudget(context.Background())
	if IncToolTurnBudget(other, "memory_deep_recall") != 1 {
		t.Fatal("a new turn must not inherit the previous counter")
	}
	if IncToolTurnBudget(context.Background(), "memory_deep_recall") != 1 {
		t.Fatal("missing counter should not accumulate")
	}
}

// budgetScriptProvider returns two tool calls, then stops.
type budgetScriptProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *budgetScriptProvider) Name() string { return "budget-script" }

func (p *budgetScriptProvider) DoGenerate(_ context.Context, _ GenerateParams) (*GenerateResult, error) {
	p.mu.Lock()
	n := p.calls
	p.calls++
	p.mu.Unlock()
	if n == 0 {
		return &GenerateResult{
			FinishReason: FinishReasonToolCalls,
			ToolCalls: []ToolCall{
				{ToolCallID: "c1", ToolName: "probe", Input: map[string]any{"n": 1}},
				{ToolCallID: "c2", ToolName: "probe", Input: map[string]any{"n": 2}},
			},
		}, nil
	}
	return &GenerateResult{Text: "done", FinishReason: FinishReasonStop}, nil
}

func (p *budgetScriptProvider) DoStream(context.Context, GenerateParams) (*StreamResult, error) {
	return nil, nil
}

func TestOrchestrate_InstallsToolTurnBudget(t *testing.T) {
	var mu sync.Mutex
	var seen []int
	probe := Tool{
		Name:       "probe",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(ctx *ToolExecContext, _ any) (any, error) {
			n := IncToolTurnBudget(ctx, "probe")
			mu.Lock()
			seen = append(seen, n)
			mu.Unlock()
			return "ok", nil
		},
	}
	_, err := OrchestrateGenerate(context.Background(), &budgetScriptProvider{}, &OrchestrateConfig{
		MaxSteps: 2,
		Params: GenerateParams{
			Messages: []Message{UserMessage("hi")},
			Tools:    []Tool{probe},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Ints(seen)
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Fatalf("turn budget counts = %v, want [1 2]", seen)
	}
}
