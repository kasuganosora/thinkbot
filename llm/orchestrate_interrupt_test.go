package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// interruptFakeProvider plays a two-step script to exercise the mid-run
// "user append" (Claude-CLI style):
//   - step 0: emit "answer1", then PAUSE on step0Wait (so the test can inject an
//     append while the agent is still "generating"), then finish.
//   - step 1: emit "answer2" (only reachable because the append forced a continue).
//
// It records the message list it received on each DoStream call so the test can
// assert the appended user message made it into the next step's context.
type interruptFakeProvider struct {
	mu           sync.Mutex
	calls        int
	callMessages [][]Message
	step0Wait    chan struct{}
	started      chan struct{}
}

func (p *interruptFakeProvider) Name() string { return "fake" }

func (p *interruptFakeProvider) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	return nil, errors.New("not implemented")
}

func (p *interruptFakeProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	p.mu.Lock()
	n := p.calls
	p.calls++
	cp := make([]Message, len(params.Messages))
	copy(cp, params.Messages)
	p.callMessages = append(p.callMessages, cp)
	p.mu.Unlock()

	ch := make(chan StreamPart, 16)
	go func() {
		if n == 0 {
			ch <- &TextDeltaPart{Text: "answer1"}
			close(p.started)
			<-p.step0Wait // 等待测试注入追加
		} else {
			ch <- &TextDeltaPart{Text: "answer2"}
		}
		ch <- &FinishStepPart{FinishReason: FinishReasonStop}
		ch <- &FinishPart{FinishReason: FinishReasonStop}
		close(ch)
	}()
	return &StreamResult{Stream: ch}, nil
}

// TestOrchestrateInterruptAppendsMidRun 验证：用户在生成过程中（step0 流式输出
// 期间）补充的内容，会被注入同一轮对话并让模型结合新内容继续生成（多走一步）。
func TestOrchestrateInterruptAppendsMidRun(t *testing.T) {
	prov := &interruptFakeProvider{
		step0Wait: make(chan struct{}),
		started:   make(chan struct{}),
	}
	interruptCh := make(chan string, 4)
	cfg := &OrchestrateConfig{
		Params: GenerateParams{
			Messages: []Message{UserMessage("question")},
		},
		MaxSteps:    10,
		InterruptCh: interruptCh,
	}

	done := make(chan *StreamResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := OrchestrateStream(context.Background(), prov, cfg)
		errCh <- err
		done <- res
	}()

	// 等待 step0 开始输出，再注入用户补充。
	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never started step0")
	}
	interruptCh <- "please add detail X"
	close(prov.step0Wait)

	var res *StreamResult
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OrchestrateStream timed out")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("OrchestrateStream err: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	// 消费 stream 直到关闭，确保编排循环（在 OrchestrateStream 的内部 goroutine
	// 中运行）已经完成、res.Steps 已填充。
	for range res.Stream {
	}

	// 关键断言：追加发生在 step0 流式输出期间，触发循环 continue，于是 provider
	// 被再次调用（callMessages 应有 2 次），且下一步的上下文带上追加的用户消息。
	prov.mu.Lock()
	msgs := prov.callMessages
	prov.mu.Unlock()
	if len(msgs) != 2 {
		t.Fatalf("expected provider to receive 2 calls (loop continued after append), got %d", len(msgs))
	}
	found := false
	for _, m := range msgs[1] {
		if m.Role == MessageRoleUser && strings.Contains(TextFromParts(m.Content), "please add detail X") {
			found = true
		}
	}
	if !found {
		t.Fatalf("appended user message not found in continuation context: %+v", msgs[1])
	}

	// 续跑的这一步应产出 answer2。
	full := strings.Builder{}
	for _, s := range res.Steps {
		full.WriteString(s.Text)
	}
	fullStr := full.String()
	if !strings.Contains(fullStr, "answer2") {
		t.Fatalf("expected continuation to produce answer2, got %q", fullStr)
	}
}

// toolChainInterruptProvider 模拟长工具链轮次：
//   - step 0：发一个工具调用，工具执行期间测试注入用户补充；
//   - step >= 1：只输出文本，不再调工具。
//
// ⚠️ 与 interruptFakeProvider 的关键差别就是 step 0 **带工具调用**：旧实现只在
// 「本步没有工具调用」的收尾分支取补充，所以工具链轮次里补充永远取不到 —— 这个
// provider 正是为了让该缺陷可复现（老代码下补充要等到最后一步才进上下文）。
type toolChainInterruptProvider struct {
	mu           sync.Mutex
	calls        int
	callMessages [][]Message
	toolStarted  chan struct{}
	toolGate     chan struct{}
}

func (p *toolChainInterruptProvider) Name() string { return "fake" }

func (p *toolChainInterruptProvider) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	return nil, errors.New("not implemented")
}

func (p *toolChainInterruptProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	p.mu.Lock()
	n := p.calls
	p.calls++
	cp := make([]Message, len(params.Messages))
	copy(cp, params.Messages)
	p.callMessages = append(p.callMessages, cp)
	p.mu.Unlock()

	ch := make(chan StreamPart, 16)
	go func() {
		if n == 0 {
			ch <- &StreamToolCallPart{ToolCallID: "t1", ToolName: "exec", Input: map[string]any{"cmd": "ls"}}
			ch <- &FinishStepPart{FinishReason: FinishReasonToolCalls}
			ch <- &FinishPart{FinishReason: FinishReasonToolCalls}
		} else {
			ch <- &TextDeltaPart{Text: "final answer"}
			ch <- &FinishStepPart{FinishReason: FinishReasonStop}
			ch <- &FinishPart{FinishReason: FinishReasonStop}
		}
		close(ch)
	}()
	return &StreamResult{Stream: ch}, nil
}

// TestOrchestrateStreamInterruptDuringToolChain 验证：用户在**工具链执行期间**
// 补充的内容，必须在**下一个步边界**就进入上下文。
//
// 长工具链期间的补充必须在下一步边界进入上下文。旧实现只在「本步没有工具
// 调用」时取一次，补充会留在通道里直到本轮结束被丢弃。
func TestOrchestrateStreamInterruptDuringToolChain(t *testing.T) {
	prov := &toolChainInterruptProvider{
		toolStarted: make(chan struct{}, 1),
		toolGate:    make(chan struct{}),
	}
	tool := Tool{
		Name:       "exec",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(ctx *ToolExecContext, input any) (any, error) {
			select {
			case prov.toolStarted <- struct{}{}:
			default:
			}
			<-prov.toolGate // 卡在工具执行中，模拟长任务
			return "ok", nil
		},
	}
	interruptCh := make(chan string, 4)
	cfg := &OrchestrateConfig{
		Params: GenerateParams{
			Messages: []Message{UserMessage("question")},
			Tools:    []Tool{tool},
		},
		MaxSteps:    10,
		InterruptCh: interruptCh,
	}

	done := make(chan *StreamResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := OrchestrateStream(context.Background(), prov, cfg)
		errCh <- err
		done <- res
	}()

	// 等工具真的跑起来，再在「工具链执行期间」注入补充。
	select {
	case <-prov.toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}
	interruptCh <- "补充：日志主题是 demo_events"
	close(prov.toolGate)

	var res *StreamResult
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OrchestrateStream timed out")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("OrchestrateStream err: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	for range res.Stream {
	}

	prov.mu.Lock()
	msgs := prov.callMessages
	prov.mu.Unlock()

	// 关键断言：补充必须在**紧接着的下一步**（msgs[1]）就出现在上下文里。
	// 旧实现下它会推迟到工具链跑完后的那一步（msgs[2] 甚至更晚），
	// 若本轮因步数上限结束则彻底丢掉。
	if len(msgs) != 2 {
		t.Fatalf("expected 2 provider calls (tool step + final step), got %d", len(msgs))
	}
	found := false
	for _, m := range msgs[1] {
		if m.Role == MessageRoleUser && strings.Contains(TextFromParts(m.Content), "demo_events") {
			found = true
		}
	}
	if !found {
		t.Fatalf("supplement not injected at the next step boundary: %+v", msgs[1])
	}
}

// TestDrainInterruptMessages 验证 drain 助手函数的非阻塞取空与空通道安全。
func TestDrainInterruptMessages(t *testing.T) {
	var msgs []Message
	if n := drainInterruptMessages(context.Background(), nil, &msgs); n != 0 {
		t.Fatalf("nil channel should drain 0, got %d", n)
	}
	ch := make(chan string, 4)
	ch <- "a"
	ch <- "  " // 空白被忽略
	ch <- "b"
	if n := drainInterruptMessages(context.Background(), ch, &msgs); n != 2 {
		t.Fatalf("expected 2 drained, got %d", n)
	}
	if len(msgs) != 2 || msgs[0].Role != MessageRoleUser || msgs[1].Role != MessageRoleUser {
		t.Fatalf("unexpected drained messages: %+v", msgs)
	}
	// 再次取空不应阻塞
	if n := drainInterruptMessages(context.Background(), ch, &msgs); n != 0 {
		t.Fatalf("empty channel should drain 0, got %d", n)
	}
}
