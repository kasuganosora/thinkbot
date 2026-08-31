package llm

import (
	"context"
	"testing"
)

// toolChoiceRecorder 是一个最小 mock Provider，记录每次 DoGenerate 收到的
// ToolChoice，并在第一步返回工具调用、第二步返回 stop，用于验证
// OrchestrateConfig.ToolChoiceForStep 的逐步覆盖与 toolsExecuted 翻转。
type toolChoiceRecorder struct {
	name  string
	calls []any
	step  int
}

func (p *toolChoiceRecorder) Name() string { return p.name }

func (p *toolChoiceRecorder) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	p.calls = append(p.calls, params.ToolChoice)
	if p.step == 0 {
		p.step++
		return &GenerateResult{
			Text:         "need to verify",
			FinishReason: FinishReasonToolCalls,
			ToolCalls:    []ToolCall{{ToolCallID: "c1", ToolName: "exec", Input: map[string]any{"cmd": "which git"}}},
		}, nil
	}
	return &GenerateResult{Text: "git is present", FinishReason: FinishReasonStop}, nil
}

func (p *toolChoiceRecorder) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	ch := make(chan StreamPart, 1)
	ch <- &FinishPart{FinishReason: FinishReasonStop, TotalUsage: Usage{}}
	close(ch)
	return &StreamResult{Stream: ch}, nil
}

func verifyTool() Tool {
	return Tool{
		Name:    "exec",
		Execute: func(ctx *ToolExecContext, input any) (any, error) { return "ok", nil },
	}
}

// TestToolChoiceForStepOverride 验证 ToolChoiceForStep 逐步骤覆盖 ToolChoice，
// 并在首次工具执行后复位（toolsExecuted 翻转）。
func TestToolChoiceForStepOverride(t *testing.T) {
	prov := &toolChoiceRecorder{name: "mock"}
	cfg := &OrchestrateConfig{
		Params: GenerateParams{
			Model:     &Model{ID: "mock"},
			Tools:     []Tool{verifyTool()},
			MaxTokens: intPtr(100),
		},
		MaxSteps: 5,
		ToolChoiceForStep: func(step int, toolsExecuted bool) any {
			if !toolsExecuted {
				return "required"
			}
			return nil
		},
	}

	result, err := OrchestrateGenerate(context.Background(), prov, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.FinishReason != FinishReasonStop {
		t.Errorf("expected stop finish, got %s", result.FinishReason)
	}
	if len(prov.calls) != 2 {
		t.Fatalf("expected 2 DoGenerate calls, got %d (%v)", len(prov.calls), prov.calls)
	}
	// 第一步：toolsExecuted=false → "required"
	if prov.calls[0] != "required" {
		t.Errorf("step0 ToolChoice = %v, want \"required\"", prov.calls[0])
	}
	// 第二步：toolsExecuted=true → nil (auto)
	if prov.calls[1] != nil {
		t.Errorf("step1 ToolChoice = %v, want nil", prov.calls[1])
	}
}

// TestToolChoiceForStepNil 验证未设置回调时不覆盖（保留 Params.ToolChoice）。
func TestToolChoiceForStepNil(t *testing.T) {
	prov := &toolChoiceRecorder{name: "mock"}
	cfg := &OrchestrateConfig{
		Params: GenerateParams{
			Model:      &Model{ID: "mock"},
			Tools:      []Tool{verifyTool()},
			MaxTokens:  intPtr(100),
			ToolChoice: "none",
		},
		MaxSteps: 5,
	}

	result, err := OrchestrateGenerate(context.Background(), prov, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.FinishReason != FinishReasonStop {
		t.Errorf("expected stop finish, got %s", result.FinishReason)
	}
	if len(prov.calls) != 2 {
		t.Fatalf("expected 2 DoGenerate calls, got %d", len(prov.calls))
	}
	// 没有 ToolChoiceForStep → 每步都保留 Params.ToolChoice="none"
	if prov.calls[0] != "none" || prov.calls[1] != "none" {
		t.Errorf("ToolChoice calls = %v, want both \"none\"", prov.calls)
	}
}

func intPtr(i int) *int { return &i }
