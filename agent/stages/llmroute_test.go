package stages

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// blockProvider 模拟「假活不返回」的 LLM：DoGenerate 阻塞直到 ctx 取消，
// 用于验证 HardTimeout 墙钟兜底能强制终止挂起的编排回路。
type blockProvider struct {
	unblock chan struct{}
}

func (p *blockProvider) Name() string { return "block" }

func (p *blockProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.unblock:
		return &llm.GenerateResult{Text: "ok"}, nil
	}
}

func (p *blockProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	return &llm.StreamResult{}, nil
}

func newHardTimeoutTestStage(provider llm.Provider, hardTimeout time.Duration) *LLMStage {
	cfg := LLMConfig{
		// MaxSteps=0 走 single-step fast path：直接 DoGenerate，避免 loop 守卫干扰断言。
		MaxSteps:     0,
		HardMaxSteps: 0,
		HardTimeout:  hardTimeout,
	}
	return NewLLMStage("llm", provider, cfg, nil, zap.NewNop().Sugar())
}

func newHardTimeoutTestEnv() *core.Envelope {
	return core.NewEnvelope(core.Message{ID: "m1", BotID: "bot1", Text: "hi"})
}

// TestLLMStage_HardTimeout_FiresWithoutUpstreamDeadline
// 无上游 deadline 时，HardTimeout 应被启用，并强制终止挂起的编排。
func TestLLMStage_HardTimeout_FiresWithoutUpstreamDeadline(t *testing.T) {
	prov := &blockProvider{unblock: make(chan struct{})}
	stage := newHardTimeoutTestStage(prov, 50*time.Millisecond)

	start := time.Now()
	_, err := stage.Process(context.Background(), newHardTimeoutTestEnv())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error from hard timeout, got nil")
	}
	pe, ok := err.(*core.PipelineError)
	if !ok {
		t.Fatalf("expected *core.PipelineError, got %T (%v)", err, err)
	}
	// 强制终止必须归因到墙钟上限（DeadlineExceeded），而非普通 provider 错误。
	if !errors.Is(pe.Cause, context.DeadlineExceeded) {
		t.Errorf("PipelineError.Cause should be DeadlineExceeded, got %v", pe.Cause)
	}
	if elapsed > 2*time.Second {
		t.Errorf("hard timeout took too long: %v", elapsed)
	}
}

// TestLLMStage_HardTimeout_RespectsUpstreamDeadline
// 上游已设 deadline 时，HardTimeout 不应覆盖——保持 5s 不生效，由上游 30ms 先触发。
func TestLLMStage_HardTimeout_RespectsUpstreamDeadline(t *testing.T) {
	prov := &blockProvider{unblock: make(chan struct{})}
	stage := newHardTimeoutTestStage(prov, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := stage.Process(ctx, newHardTimeoutTestEnv())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error (upstream deadline), got nil")
	}
	// 关键断言：上游 30ms 先触发 → 整体耗时远小于 HardTimeout(5s)，
	// 证明 HardTimeout 未被叠加、上游 deadline 得到尊重（避免双重截断/误判）。
	if elapsed > 1*time.Second {
		t.Errorf("HardTimeout was applied over upstream deadline; elapsed=%v", elapsed)
	}
}

// TestLLMStage_HardTimeout_ZeroDisablesCap
// HardTimeout=0 时不包墙钟上限；正常 provider 应成功返回（无 cap 干扰）。
func TestLLMStage_HardTimeout_ZeroDisablesCap(t *testing.T) {
	prov := &blockProvider{unblock: make(chan struct{})}
	close(prov.unblock) // 立即返回成功，避免无限阻塞
	stage := newHardTimeoutTestStage(prov, 0)

	env, err := stage.Process(context.Background(), newHardTimeoutTestEnv())
	if err != nil {
		t.Fatalf("expected no error with HardTimeout=0, got %v", err)
	}
	if env == nil {
		t.Fatalf("expected returned envelope, got nil")
	}
}
