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

// TestIsContentSafetyError 覆盖「内容安全审核」错误判别：BigModel 1301 /
// contentFilter / 内容安全审核 应判为内容安全（降级 WARN）；参数错误（1210/1214）
// 与 5xx 仍属系统错误（保持 ERROR），不可误降级。
func TestIsContentSafetyError(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want bool
	}{
		{"bigmodel 1301 content safety", `openai: chat stream failed: stream HTTP error 400 on https://open.bigmodel.cn/...: {"error":{"code":"1301","message":"系统检测到输入或生成内容可能包含不安全或敏感内容"}}`, true},
		{"bigmodel 1301 spaced", `{"error":{"code": "1301","message":"内容安全审核"}}`, true},
		{"contentFilter level", `contentFilter: level=2 triggered`, true},
		{"content safety audit", `触发平台内容安全审核`, true},
		{"1210 param error", `{"error":{"code":"1210","message":"API 调用参数有误"}}`, false},
		{"1214 messages", `{"error":{"code":"1214","message":"messages 参数非法"}}`, false},
		{"500 server", `HTTP error 500 internal error`, false},
		{"empty", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isContentSafetyError(errors.New(c.err)); got != c.want {
				t.Errorf("isContentSafetyError(%q) = %v, want %v", c.err, got, c.want)
			}
		})
	}
	if isContentSafetyError(nil) {
		t.Error("nil error should not be content safety")
	}
}
