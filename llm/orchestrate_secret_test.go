package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/secret"
)

// secretFlowProvider 第一步返回一个携带占位符的工具调用（exec 的 cmd 里带
// {{secret:svc.token}}），第二步返回 stop。用于验证咽喉点的双向翻译：
//   - 工具 Execute 实际收到的是还原后的真实值；
//   - 回灌进模型上下文的 tool result / tool call input 里只有占位符。
type secretFlowProvider struct{ step int }

func (p *secretFlowProvider) Name() string { return "secret-flow-mock" }

func (p *secretFlowProvider) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	if p.step == 0 {
		p.step++
		return &GenerateResult{
			Text:         "calling tool",
			FinishReason: FinishReasonToolCalls,
			ToolCalls: []ToolCall{{
				ToolCallID: "c1",
				ToolName:   "exec",
				Input:      map[string]any{"cmd": "curl -H 'Authorization: Bearer {{secret:svc.token}}' https://internal.example.com"},
			}},
		}, nil
	}
	return &GenerateResult{Text: "done", FinishReason: FinishReasonStop}, nil
}

func (p *secretFlowProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	ch := make(chan StreamPart, 1)
	ch <- &FinishPart{FinishReason: FinishReasonStop, TotalUsage: Usage{}}
	close(ch)
	return &StreamResult{Stream: ch}, nil
}

func TestOrchestrate_ResolveInput_RedactOutput(t *testing.T) {
	v, err := secret.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Register("svc.token", "REAL_TOKEN_VALUE_abcdefgh", "测试令牌"); err != nil {
		t.Fatal(err)
	}
	prev := secret.Default()
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })

	const realValue = "REAL_TOKEN_VALUE_abcdefgh"

	var gotInput any
	execTool := Tool{
		Name: "exec",
		Execute: func(ctx *ToolExecContext, input any) (any, error) {
			gotInput = input
			// 工具把收到的（真实）值随结果回吐，模拟「工具输出里含票据」的场景。
			return "fetched with token " + realValue, nil
		},
	}

	prov := &secretFlowProvider{}
	cfg := &OrchestrateConfig{
		Params: GenerateParams{
			Model:     &Model{ID: "mock"},
			Tools:     []Tool{execTool},
			MaxTokens: intPtr(200),
		},
		MaxSteps: 5,
	}
	result, err := OrchestrateGenerate(context.Background(), prov, cfg)
	if err != nil {
		t.Fatalf("OrchestrateGenerate: %v", err)
	}

	// 1) 咽喉点还原：工具收到的 cmd 含真实值，不含占位符。
	m, ok := gotInput.(map[string]any)
	if !ok {
		t.Fatalf("tool input type = %T, want map", gotInput)
	}
	cmd, _ := m["cmd"].(string)
	if !strings.Contains(cmd, realValue) {
		t.Errorf("tool input not resolved to real value: %q", cmd)
	}
	if strings.Contains(cmd, "{{secret:") {
		t.Errorf("tool input still contains placeholder: %q", cmd)
	}

	// 2) 咽喉点遮蔽：回灌进模型上下文的消息里绝无真实值，只有占位符。
	if result.Messages == nil {
		t.Fatal("result.Messages is nil")
	}
	all := ""
	for _, msg := range result.Messages {
		all += flattenMessageText(msg)
	}
	if strings.Contains(all, realValue) {
		t.Errorf("real secret leaked into model messages: %q", all)
	}
	// tool result 里应出现占位符（工具回吐的真实值被遮蔽）。
	if !strings.Contains(all, "{{secret:svc.token}}") {
		t.Errorf("expected placeholder in model messages, got: %q", all)
	}
}

// flattenMessageText 把一条 Message 的文本与工具结果/入参拍平成字符串，便于断言。
func flattenMessageText(msg Message) string {
	var b strings.Builder
	for _, p := range msg.Content {
		switch v := p.(type) {
		case TextPart:
			b.WriteString(v.Text)
		case ToolCallPart:
			b.WriteString(fmt.Sprint(v.Input))
		case ToolResultPart:
			b.WriteString(fmt.Sprint(v.Result))
		}
	}
	return b.String()
}
