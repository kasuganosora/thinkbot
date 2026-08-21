package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// TestSubAgent_AutoCompactCreatesIsolatedCompactor 验证 WithAutoCompact 让每个
// subagent 各自持有独立 Compactor 实例。Compactor 内部有 previousSummary /
// compactionCount 等跨调用状态，若多 subagent 共享同一实例（如 DelegateMany 并发）
// 会互相污染，故必须隔离。同时验证 WithCompactor 显式覆盖、默认不创建。
func TestSubAgent_AutoCompactCreatesIsolatedCompactor(t *testing.T) {
	prov := newMockProvider()

	// 默认不创建
	sa1 := New(prov, "m")
	if sa1.compactor != nil {
		t.Errorf("expected no compactor by default, got %v", sa1.compactor)
	}
	sa1.Close()

	// WithAutoCompact 自动创建独立实例
	sa2 := New(prov, "m", WithAutoCompact())
	if sa2.compactor == nil {
		t.Fatal("expected auto-created compactor, got nil")
	}
	sa2.Close()

	// 两个 auto subagent 的 compactor 不应是同一指针（隔离）
	sa3 := New(prov, "m", WithAutoCompact())
	sa4 := New(prov, "m", WithAutoCompact())
	if sa3.compactor == sa4.compactor {
		t.Error("auto-compactors of different subagents must be isolated instances")
	}
	sa3.Close()
	sa4.Close()

	// 显式 WithCompactor 覆盖 auto
	explicit := llm.NewCompactor(llm.DefaultCompactionConfig())
	sa5 := New(prov, "m", WithAutoCompact(), WithCompactor(explicit))
	if sa5.compactor != explicit {
		t.Error("explicit WithCompactor should override auto-create")
	}
	sa5.Close()
}

// TestSubAgent_WithAutoCompactEnablesReduction 验证 WithAutoCompact 除了创建
// 独立 Compactor，还默认开启 in-loop 缩减（WithReduction 同款），让单轮工具循环的
// context 爆炸也能被压住——这是「自动上下文管理」的完整语义。语义压缩在单轮下几乎
// 触发不到，缩减才是单轮失控流的核心防线；二者一并开启才完整。
func TestSubAgent_WithAutoCompactEnablesReduction(t *testing.T) {
	prov := newMockProvider()

	sa := New(prov, "m", WithAutoCompact())
	defer sa.Close()

	if sa.reducer == nil {
		t.Error("WithAutoCompact should also enable reduction (PrepareStep Phase 2)")
	}
	if sa.reducerOnToolResults == nil {
		t.Error("WithAutoCompact should also enable reduction (OnToolResults Phase 1)")
	}
	if sa.compactor == nil {
		t.Error("WithAutoCompact should create an isolated compactor")
	}

	// 显式 WithReduction 覆盖 auto 播种的默认缩减配置。
	custom := llm.ReductionConfig{MaxOutputTokens: 123, ClearThresholdTokens: 999, RetainRecentSteps: 2}
	sa2 := New(prov, "m", WithAutoCompact(), WithReduction(custom))
	defer sa2.Close()
	if sa2.reductionConfig.MaxOutputTokens != 123 ||
		sa2.reductionConfig.ClearThresholdTokens != 999 ||
		sa2.reductionConfig.RetainRecentSteps != 2 {
		t.Errorf("explicit WithReduction should override auto default: got %+v", sa2.reductionConfig)
	}
}

// reductionSpyProvider 探测工具结果在回灌给 LLM 前是否被缩减钩子（阶段 1 OnToolResults
// / 阶段 2 ReduceHistory）截断。每步 DoGenerate 时记录 messages 中最大的 tool result
// 字符长度，用于端到端验证 PrepareStep / OnToolResults 真正接入了 OrchestrateGenerate 循环。
type reductionSpyProvider struct {
	mu               sync.Mutex
	calls            int
	maxToolResultLen int
	summarySeen      bool
}

func toolResultLen(v any) int {
	switch s := v.(type) {
	case string:
		return len(s)
	case []byte:
		return len(s)
	default:
		return len(fmt.Sprintf("%v", s))
	}
}

func (p *reductionSpyProvider) recordToolResults(params llm.GenerateParams) {
	if params.System == llm.CompactionSystemPrompt {
		p.summarySeen = true
	}
	for _, m := range params.Messages {
		for _, part := range m.Content {
			if tr, ok := part.(llm.ToolResultPart); ok {
				if l := toolResultLen(tr.Result); l > p.maxToolResultLen {
					p.maxToolResultLen = l
				}
			}
		}
	}
}

func (p *reductionSpyProvider) Name() string { return "reduction-spy" }

func (p *reductionSpyProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.recordToolResults(params)
	p.mu.Unlock()

	if n == 1 {
		// 第一步：请求工具调用，触发工具执行 → OnToolResults 缩减。
		return &llm.GenerateResult{
			Text:         "",
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls: []llm.ToolCall{
				{ToolCallID: "call-1", ToolName: "big_tool", Input: map[string]any{"x": 1}},
			},
		}, nil
	}
	// 第二步：携带（应已被缩减的）工具结果收尾。
	return &llm.GenerateResult{Text: "done", FinishReason: llm.FinishReasonStop}, nil
}

func (p *reductionSpyProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	ch := make(chan llm.StreamPart, 2)
	go func() {
		defer close(ch)
		ch <- &llm.TextDeltaPart{Text: "done"}
		ch <- &llm.FinishPart{FinishReason: llm.FinishReasonStop}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}

// TestSubAgent_ReductionTruncatesOversizedToolResults 验证：带缩减配置时，
// 工具编排回路会在结果回灌给 LLM 前将其截断（阶段 1 OnToolResults）。这正是修复
// 「单轮工具循环 context 爆炸 → 卡死看门狗养到 30min 硬上限」失控流的关键路径。
// 注意：语义压缩（compactor）在单轮委托下几乎触发不到，真正压住的是缩减钩子。
func TestSubAgent_ReductionTruncatesOversizedToolResults(t *testing.T) {
	prov := &reductionSpyProvider{}

	const bigSize = 2000
	bigTool := llm.Tool{
		Name:        "big_tool",
		Description: "returns a huge result to force truncation",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "integer"}},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			return strings.Repeat("x", bigSize), nil
		}),
	}

	// 极小阈值，确保单条结果（~500 tokens）必然触发阶段 1 截断，且总 token 超阶段 2 阈值。
	cfg := llm.ReductionConfig{
		MaxOutputTokens:      100,
		ClearThresholdTokens: 50,
		RetainRecentSteps:    1,
	}

	sa := New(prov, "test-model",
		WithTools(bigTool),
		WithToolSteps(10),
		WithReduction(cfg),
	)
	defer sa.Close()

	if _, err := sa.ChatWithResult(context.Background(), "use the tool"); err != nil {
		t.Fatalf("ChatWithResult failed: %v", err)
	}

	prov.mu.Lock()
	seen := prov.maxToolResultLen
	prov.mu.Unlock()

	if seen >= bigSize {
		t.Errorf("expected oversized tool result (len=%d) to be truncated before reaching LLM, but it was passed through intact (max seen %d)", bigSize, seen)
	}
}

// TestSubAgent_AutoCompactWiresReductionIntoLoop 端到端验证：仅用 WithAutoCompact
// （生产默认路径，manager 即如此调用）时，工具结果同样会被缩减钩子截断——证明自动
// 上下文管理已真正接入编排循环，而非仅创建了 Compactor 实例却从不触发。
func TestSubAgent_AutoCompactWiresReductionIntoLoop(t *testing.T) {
	prov := &reductionSpyProvider{}

	// 用足够大的结果（远超 DefaultReductionConfig.MaxOutputTokens=7500 tokens≈30K 字符），
	// 验证生产默认路径（WithAutoCompact）的缩减钩子确实会截断——而非仅创建了 Compactor。
	const bigSize = 40000
	bigTool := llm.Tool{
		Name:        "big_tool",
		Description: "returns a huge result",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"x": map[string]any{"type": "integer"}},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			return strings.Repeat("x", bigSize), nil
		}),
	}

	sa := New(prov, "test-model",
		WithTools(bigTool),
		WithToolSteps(10),
		WithAutoCompact(), // 生产默认：compactor + default reduction 一并开启
	)
	defer sa.Close()

	if _, err := sa.ChatWithResult(context.Background(), "use the tool"); err != nil {
		t.Fatalf("ChatWithResult failed: %v", err)
	}

	prov.mu.Lock()
	seen := prov.maxToolResultLen
	prov.mu.Unlock()

	if seen >= bigSize {
		t.Errorf("WithAutoCompact should wire reduction into the loop and truncate the %d-char result, but it reached LLM intact (max seen %d)", bigSize, seen)
	}
}
