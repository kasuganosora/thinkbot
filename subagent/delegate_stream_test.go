package subagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// DelegateStream 卡死看门狗测试
// ============================================================================

// streamStep 是脚本式流式输出的一个步骤：先静默等待 wait，再 emit token（空则不发）。
type streamStep struct {
	wait  time.Duration
	token string
}

// scriptedStreamProvider 按脚本输出 token，并在静默等待期间尊重 ctx 取消。
type scriptedStreamProvider struct {
	name  string
	steps []streamStep
}

func (p *scriptedStreamProvider) Name() string { return p.name }

func (p *scriptedStreamProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	var sb strings.Builder
	for _, s := range p.steps {
		sb.WriteString(s.token)
	}
	return &llm.GenerateResult{Text: sb.String(), FinishReason: llm.FinishReasonStop}, nil
}

func (p *scriptedStreamProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	ch := make(chan llm.StreamPart)
	go func() {
		defer close(ch)
		for _, s := range p.steps {
			if s.wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(s.wait):
				}
			}
			if s.token != "" {
				select {
				case <-ctx.Done():
					return
				case ch <- &llm.TextDeltaPart{Text: s.token}:
				}
			}
		}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}

// TestDelegateStream_Stuck 有输出后彻底沉默（无 token）超过卡死阈值，应判卡死终止。
func TestDelegateStream_Stuck(t *testing.T) {
	// stuck=2s, hard=6s；先发一个 token，之后彻底沉默（100s 无输出）→ 看门狗应在 ~5s tick 判卡死。
	provider := &scriptedStreamProvider{
		name: "script",
		steps: []streamStep{
			{wait: 0, token: "hi"},         // 首 token
			{wait: 100 * time.Second, token: ""}, // 之后永久沉默（尊重 ctx 取消）
		},
	}
	mgr := NewSubAgentManager(provider, "test-model")

	start := time.Now()
	_, err := mgr.DelegateStream(context.Background(), "", "task",
		WithStuckTimeout(2*time.Second),
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected stuck error, got nil")
	}
	if !strings.Contains(err.Error(), "卡死") {
		t.Errorf("expected 卡死 error, got %q", err.Error())
	}
	// 不应无限等待：应在合理时间内终止（看门狗 tick=5s）
	if elapsed > 20*time.Second {
		t.Errorf("stuck watchdog took too long: %s", elapsed)
	}
}

// TestDelegateStream_Live 持续输出 token（即便慢）不应被杀，应正常返回完整文本。
func TestDelegateStream_Live(t *testing.T) {
	// stuck=2s, hard=6s；每 1s 输出一个 token，共 5 个（总时长 ~4s < hard）。
	steps := []streamStep{
		{wait: 1 * time.Second, token: "a"},
		{wait: 1 * time.Second, token: "b"},
		{wait: 1 * time.Second, token: "c"},
		{wait: 1 * time.Second, token: "d"},
		{wait: 1 * time.Second, token: "e"},
	}
	provider := &scriptedStreamProvider{name: "script", steps: steps}
	mgr := NewSubAgentManager(provider, "test-model")

	text, err := mgr.DelegateStream(context.Background(), "", "task",
		WithStuckTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("live stream should not be killed, got error: %v", err)
	}
	if text != "abcde" {
		t.Errorf("expected 'abcde', got %q", text)
	}
}

// TestDelegateStream_HardCap 持续输出但永不结束，应被总时长硬上限（stuck×3）强制终止。
func TestDelegateStream_HardCap(t *testing.T) {
	// stuck=1s, hard=3s；每 0.5s 输出 token 且永不停止 → 总时长超 hard 后强杀。
	provider := &scriptedStreamProvider{
		name: "script",
		steps: []streamStep{
			{wait: 500 * time.Millisecond, token: "x"}, // 循环追加：用 infinite 模拟
		},
	}
	// 让脚本无限循环：包装一个无限 provider
	infinite := &loopingStreamProvider{inner: provider}

	mgr := NewSubAgentManager(infinite, "test-model")
	start := time.Now()
	_, err := mgr.DelegateStream(context.Background(), "", "task",
		WithStuckTimeout(1*time.Second),
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected hard-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "硬上限") {
		t.Errorf("expected 硬上限 error, got %q", err.Error())
	}
	if elapsed > 15*time.Second {
		t.Errorf("hard cap took too long: %s", elapsed)
	}
}

// loopingStreamProvider 将一个单步脚本无限重复，模拟「持续输出但永不结束」的 LLM。
type loopingStreamProvider struct {
	inner *scriptedStreamProvider
}

func (p *loopingStreamProvider) Name() string { return p.inner.Name() }

func (p *loopingStreamProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	return p.inner.DoGenerate(ctx, params)
}

func (p *loopingStreamProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	ch := make(chan llm.StreamPart)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// 重放 inner 的单个 step（wait + token），循环直到 ctx 取消
			for _, s := range p.inner.steps {
				if s.wait > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(s.wait):
					}
				}
				if s.token != "" {
					select {
					case <-ctx.Done():
						return
					case ch <- &llm.TextDeltaPart{Text: s.token}:
					}
				}
			}
		}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}
