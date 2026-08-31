package subagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 硬化回归测试：S2 / S3 / S4b
// ============================================================================

// ---------------------------------------------------------------------------
// S2：持久 subagent（Spawn/Chat）此前无任何超时/看门狗，工具挂死或 LLM 假活会
// 无限阻塞调用方 goroutine。现 Chat 在传入 ctx 无 deadline 时套 chatTimeout 墙钟硬上限。
// ---------------------------------------------------------------------------

type blockingProvider struct{}

func (p *blockingProvider) Name() string { return "blocking" }
func (p *blockingProvider) DoGenerate(ctx context.Context, _ llm.GenerateParams) (*llm.GenerateResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *blockingProvider) DoStream(ctx context.Context, _ llm.GenerateParams) (*llm.StreamResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSubAgentManager_ChatHardensWithChatTimeout(t *testing.T) {
	provider := &blockingProvider{}
	mgr := NewSubAgentManager(provider, "m")

	// 用 WithChatTimeout 给持久 subagent 设定 200ms 硬上限。
	id, err := mgr.Spawn("sys", "agent", WithChatTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer mgr.Close(id)

	start := time.Now()
	// 无 deadline 的外部 ctx：Chat 必须自行套上 chatTimeout 并在 ~200ms 后返回错误，
	// 而非无限阻塞。
	_, _, err = mgr.Chat(context.Background(), id, "hello")
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("Chat did not honor chatTimeout: took %v (expected ~200ms)", elapsed)
	}
	if err == nil {
		t.Fatal("expected error from hung subagent Chat (should be cancelled by chatTimeout)")
	}
}

// ---------------------------------------------------------------------------
// S3：ContextManager 超滑窗时改为「压缩优先」——用 LLM 摘要替代纯删除，
// 避免持久 subagent 多轮对话早期上下文永久丢失。
// ---------------------------------------------------------------------------

func TestContextManager_SummarizeHeadCompressFirst(t *testing.T) {
	cm := NewContextManager(5) // 保留最近 5 条
	var summarizedCount int
	var summarized bool
	cm.summarizeHead = func(ctx context.Context, head []llm.Message) (llm.Message, bool) {
		summarizedCount += len(head)
		summarized = true
		return llm.SystemMessage("[summary]"), true
	}

	// 写入 10 轮（20 条），持续超过窗口上限，触发压缩优先。
	for i := 0; i < 10; i++ {
		cm.AppendWithCtx(context.Background(), llm.UserMessage("u"))
		cm.AppendWithCtx(context.Background(), llm.AssistantMessage("a"))
	}

	if !summarized {
		t.Fatal("expected summarizeHead to be invoked on overflow")
	}
	if summarizedCount == 0 {
		t.Fatal("summarizeHead received empty head")
	}
	if cm.Len() > 5 {
		t.Fatalf("expected len <= 5 after compression, got %d", cm.Len())
	}
	// 压缩后首条应为摘要系统消息（而非被删除的早期 user 消息）。
	if cm.Messages()[0].Role != llm.MessageRoleSystem {
		t.Fatalf("expected first message to be the summary, got role %q", cm.Messages()[0].Role)
	}
}

// 无 summarizeHead 时回退纯删除，且不 panic（压缩能力未启用场景）。
func TestContextManager_NoSummaryFallsBackToDrop(t *testing.T) {
	cm := NewContextManager(3)
	for i := 0; i < 10; i++ {
		cm.Append(llm.UserMessage("u"))
		cm.Append(llm.AssistantMessage("a"))
	}
	if cm.Len() > 3 {
		t.Fatalf("expected len <= 3 (drop fallback), got %d", cm.Len())
	}
	if cm.Messages()[0].Role != llm.MessageRoleUser {
		t.Fatalf("expected first message user (drop keeps tail), got %q", cm.Messages()[0].Role)
	}
}

// ---------------------------------------------------------------------------
// S4b：streamWithWatchdog 消费 goroutine 在 provider 不响应 ctx 取消、拒不关闭
// 流 channel 时不能永久泄漏——硬上限 + delegateLeakGrace 后强制截断 drain。
// （此测试故意约 30s，对应 hard + delegateLeakGrace 兜底；证明不会无限挂起。）
// ---------------------------------------------------------------------------

type neverClosingStreamProvider struct{}

func (p *neverClosingStreamProvider) Name() string { return "never" }
func (p *neverClosingStreamProvider) DoGenerate(ctx context.Context, _ llm.GenerateParams) (*llm.GenerateResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *neverClosingStreamProvider) DoStream(ctx context.Context, _ llm.GenerateParams) (*llm.StreamResult, error) {
	ch := make(chan llm.StreamPart, 1)
	ch <- &llm.TextDeltaPart{Text: "x"}
	// 发送首片段后既不关闭 channel、也不响应 ctx 取消（非合规 provider），模拟泄漏场景。
	go func() { <-ctx.Done() }()
	return &llm.StreamResult{Stream: ch}, nil
}

func TestStreamWithWatchdog_NonCompliantProviderNoLeak(t *testing.T) {
	provider := &neverClosingStreamProvider{}
	sa := New(provider, "m")
	defer sa.Close()
	mgr := NewSubAgentManager(provider, "m")

	const stuck = 50 * time.Millisecond
	const hard = 100 * time.Millisecond
	start := time.Now()
	_, err := mgr.streamWithWatchdog(context.Background(), sa, "task", stuck, hard)
	elapsed := time.Since(start)

	// 必须在 hard + delegateLeakGrace 附近返回，绝不能无限挂起。
	if elapsed > 35*time.Second {
		t.Fatalf("drain leaked: took %v (expected ~hard+leakGrace=~30s)", elapsed)
	}
	if err == nil {
		t.Fatal("expected error from forced truncation of non-compliant stream")
	}
	if !strings.Contains(err.Error(), "强制截断") {
		t.Fatalf("expected leak-truncation error, got %v", err)
	}
}

// 合规 provider 在 cancel 后及时关闭 channel：drain 应迅速结束（不等待 leakGrace）。
func TestStreamWithWatchdog_DrainCompletesPromptly(t *testing.T) {
	provider := &closingStreamProvider{}
	sa := New(provider, "m")
	defer sa.Close()
	mgr := NewSubAgentManager(provider, "m")

	const stuck = 5 * time.Second
	const hard = 10 * time.Second
	start := time.Now()
	text, err := mgr.streamWithWatchdog(context.Background(), sa, "task", stuck, hard)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "hello" {
		t.Fatalf("expected 'hello', got %q", text)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("drain did not complete promptly: took %v", elapsed)
	}
}

// closingStreamProvider 发送一个片段并在 50ms 后关闭 channel（忽略 ctx，但会关闭）。
type closingStreamProvider struct{}

func (p *closingStreamProvider) Name() string { return "closing" }
func (p *closingStreamProvider) DoGenerate(ctx context.Context, _ llm.GenerateParams) (*llm.GenerateResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *closingStreamProvider) DoStream(ctx context.Context, _ llm.GenerateParams) (*llm.StreamResult, error) {
	ch := make(chan llm.StreamPart, 1)
	ch <- &llm.TextDeltaPart{Text: "hello"}
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(ch)
	}()
	return &llm.StreamResult{Stream: ch}, nil
}
