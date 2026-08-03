package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// deferFakeProvider records the tool list it receives on each DoGenerate call
// and plays a scripted sequence: call tool_search, then call the now-loaded
// deferred tool, then stop.
type deferFakeProvider struct {
	mu    sync.Mutex
	calls [][]Tool
	n     int
}

func (p *deferFakeProvider) Name() string { return "fake" }

func (p *deferFakeProvider) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, params.Tools)
	n := p.n
	p.n++
	p.mu.Unlock()

	switch n {
	case 0:
		return &GenerateResult{
			FinishReason: FinishReasonToolCalls,
			ToolCalls:    []ToolCall{{ToolCallID: "c1", ToolName: "tool_search", Input: map[string]any{"query": "weather"}}},
		}, nil
	case 1:
		return &GenerateResult{
			FinishReason: FinishReasonToolCalls,
			ToolCalls:    []ToolCall{{ToolCallID: "c2", ToolName: "mcp__weather", Input: map[string]any{"city": "SF"}}},
		}, nil
	default:
		return &GenerateResult{FinishReason: FinishReasonStop, Text: "done"}, nil
	}
}

func (p *deferFakeProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	return nil, errors.New("not implemented")
}

func findTool(tools []Tool, name string) *Tool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func TestOrchestrateDeferredToolsEndToEnd(t *testing.T) {
	prov := new(deferFakeProvider)

	deferred := Tool{
		Name:         "mcp__weather",
		Description:  "Get weather for a city",
		Parameters:   map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []any{"city"}},
		DeferredLoad: true,
		Execute: func(ctx *ToolExecContext, input any) (any, error) {
			return "weather-ok", nil
		},
	}
	normal := Tool{
		Name:        "exec",
		Description: "Run a command",
		Parameters:  map[string]any{"type": "object"},
		Execute: func(ctx *ToolExecContext, input any) (any, error) {
			return "exec-ok", nil
		},
	}

	d := NewToolDeferral(true)
	cfg := &OrchestrateConfig{
		Params: GenerateParams{
			Messages: []Message{UserMessage("get weather")},
			Tools:    []Tool{normal, deferred},
		},
		MaxSteps:     10,
		ToolDeferral: d,
	}

	result, err := OrchestrateGenerate(context.Background(), prov, cfg)
	if err != nil {
		t.Fatalf("OrchestrateGenerate failed: %v", err)
	}
	if result.Text != "done" {
		t.Fatalf("expected final text 'done', got %q", result.Text)
	}

	// Call 0: deferred tool must be hidden (Parameters nil) and tool_search present.
	if len(prov.calls) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", len(prov.calls))
	}
	first := prov.calls[0]
	dt := findTool(first, "mcp__weather")
	if dt == nil {
		t.Fatal("deferred tool should be present in first call")
	}
	if dt.Parameters != nil {
		t.Errorf("deferred tool must hide Parameters in first call, got %v", dt.Parameters)
	}
	if findTool(first, "tool_search") == nil {
		t.Error("tool_search must be injected when deferred tools exist")
	}

	// Call 1: after tool_search loaded it, the deferred tool must show full schema.
	second := prov.calls[1]
	dt2 := findTool(second, "mcp__weather")
	if dt2 == nil {
		t.Fatal("deferred tool should be present in second call")
	}
	if dt2.Parameters == nil {
		t.Error("deferred tool must show full Parameters after being loaded")
	}

	// The deferred tool must have actually executed (loaded + called properly).
	executed := false
	for _, step := range result.Steps {
		for _, tr := range step.ToolResults {
			if tr.ToolName == "mcp__weather" && tr.Output == "weather-ok" {
				executed = true
			}
		}
	}
	if !executed {
		t.Error("deferred tool mcp__weather should have executed with output 'weather-ok'")
	}
}
