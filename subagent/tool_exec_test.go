package subagent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// toolExecMockProvider 是测试用 Provider：第一次调用返回工具调用，第二次返回最终文本。
type toolExecMockProvider struct {
	calls int32
}

func (p *toolExecMockProvider) Name() string { return "test" }

func (p *toolExecMockProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	n := atomic.AddInt32(&p.calls, 1)
	if n == 1 {
		return &llm.GenerateResult{
			Text:         "",
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "call-1", ToolName: "echo_tool", Input: map[string]any{"msg": "hello"}},
			},
		}, nil
	}
	return &llm.GenerateResult{Text: "final-answer", FinishReason: llm.FinishReasonStop}, nil
}

func (p *toolExecMockProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	ch := make(chan llm.StreamPart, 4)
	go func() {
		defer close(ch)
		ch <- &llm.TextDeltaPart{Text: "final-answer"}
		ch <- &llm.FinishPart{FinishReason: llm.FinishReasonStop}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}

// TestSubAgentExecutesInjectedTools 验证：注入带 Execute 的工具后，子 Agent 走多步编排
// 回路自动执行工具并把结果喂回模型，最终返回模型的收尾文本（而不只是纯 LLM 文本）。
func TestSubAgentExecutesInjectedTools(t *testing.T) {
	prov := &toolExecMockProvider{}
	var executed int32

	echoTool := llm.Tool{
		Name:        "echo_tool",
		Description: "echo the input back",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msg": map[string]any{"type": "string"}},
			"required":   []string{"msg"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			atomic.AddInt32(&executed, 1)
			return input, nil
		}),
	}

	sa := New(prov, "test-model", WithTools(echoTool), WithToolSteps(10))
	defer sa.Close()

	res, err := sa.ChatWithResult(context.Background(), "use the tool")
	if err != nil {
		t.Fatalf("ChatWithResult failed: %v", err)
	}
	if res.Text != "final-answer" {
		t.Errorf("expected final-answer, got %q", res.Text)
	}
	if atomic.LoadInt32(&executed) != 1 {
		t.Errorf("expected injected tool executed exactly once, got %d", executed)
	}
	// 工具调用/结果应进入子 Agent 上下文（多轮连贯）。
	if len(res.Messages) == 0 {
		t.Error("expected non-empty result.Messages (tool steps should be recorded)")
	}
}

// TestSubAgentNoToolsStaysPureLLM 验证：未注入工具时，子 Agent 仍是纯 LLM（不触发编排回路）。
func TestSubAgentNoToolsStaysPureLLM(t *testing.T) {
	prov := &toolExecMockProvider{}

	sa := New(prov, "test-model")
	defer sa.Close()

	res, err := sa.ChatWithResult(context.Background(), "just chat")
	if err != nil {
		t.Fatalf("ChatWithResult failed: %v", err)
	}
	// 纯 LLM 路径下不会进入多步编排回路：provider 第一次 DoGenerate 返回 tool-calls +
	// 空文本，应原样返回（而非继续执行第二次得到 "final-answer" + Stop）。
	// 若误触发编排回路，会执行 echo_tool 并返回 "final-answer"，与下断言矛盾。
	if res.Text != "" {
		t.Errorf("expected empty text in pure-LLM path, got %q", res.Text)
	}
	if res.FinishReason != llm.FinishReasonToolCalls {
		t.Errorf("expected raw single-call result (tool-calls), got %q", res.FinishReason)
	}
}

// recordingProvider 记录每次 DoGenerate 收到的完整消息序列，用于校验多轮上下文连贯性。
type recordingProvider struct {
	calls    int32
	mu       sync.Mutex
	callMsgs [][]llm.Message
	echoRan  bool
}

func (p *recordingProvider) Name() string { return "rec" }

func (p *recordingProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	n := atomic.AddInt32(&p.calls, 1)
	p.mu.Lock()
	p.callMsgs = append(p.callMsgs, params.Messages)
	p.mu.Unlock()
	if n == 1 {
		return &llm.GenerateResult{
			Text:         "",
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "call-1", ToolName: "echo_tool", Input: map[string]any{"msg": "hi"}},
			},
		}, nil
	}
	// 后续调用（包括第二轮）：返回最终文本，结束编排。
	return &llm.GenerateResult{Text: "done", FinishReason: llm.FinishReasonStop}, nil
}

func (p *recordingProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	ch := make(chan llm.StreamPart, 4)
	go func() {
		defer close(ch)
		ch <- &llm.TextDeltaPart{Text: "done"}
		ch <- &llm.FinishPart{FinishReason: llm.FinishReasonStop}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}

func hasUserMsg(msgs []llm.Message, want string) bool {
	for _, m := range msgs {
		if m.Role != llm.MessageRoleUser {
			continue
		}
		for _, part := range m.Content {
			if tp, ok := part.(llm.TextPart); ok && tp.Text == want {
				return true
			}
		}
	}
	return false
}

// TestSubAgentToolPathPersistsUserMessageAcrossTurns 验证：带工具的多轮子 Agent，
// 第一轮 user 消息必须保留在上下文中——第二轮调用时 provider 收到的消息里应包含第一轮的
// user 消息。回归：编排路径曾只追加 result.Messages（仅 assistant/tool 消息），丢失 user 消息，
// 导致多轮 spawn 子 Agent 上下文错乱。
func TestSubAgentToolPathPersistsUserMessageAcrossTurns(t *testing.T) {
	prov := &recordingProvider{}
	echoTool := llm.Tool{
		Name:        "echo_tool",
		Description: "echo",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msg": map[string]any{"type": "string"}},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			prov.mu.Lock()
			prov.echoRan = true
			prov.mu.Unlock()
			return input, nil
		}),
	}

	sa := New(prov, "test-model", WithTools(echoTool), WithToolSteps(10))
	defer sa.Close()

	if _, err := sa.ChatWithResult(context.Background(), "turn-1 task"); err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}
	if _, err := sa.ChatWithResult(context.Background(), "turn-2 task"); err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.callMsgs) < 3 {
		t.Fatalf("expected >=3 provider calls (turn1: tool+final, turn2: ...), got %d", len(prov.callMsgs))
	}
	// 第三轮调用（turn-2 的首步）应携带第一轮 user 消息 "turn-1 task"。
	if !hasUserMsg(prov.callMsgs[2], "turn-1 task") {
		t.Errorf("turn-2 context lost turn-1 user message; call[2] msgs = %+v", prov.callMsgs[2])
	}
}
