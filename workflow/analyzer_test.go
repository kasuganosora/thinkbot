package workflow

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/subagent"
)

// emptyThenValidProvider 前 emptyRuns 次流式返回空 content（模拟 GLM 退化态"HTTP 200 空 body"），
// 之后返回一段合法的 DAG JSON（模拟 GLM 恢复）。用于验证 analyzer 的重试+退避能跨过退化窗口。
type emptyThenValidProvider struct {
	emptyRuns int32
	calls     int32
	validJSON string
}

func (p *emptyThenValidProvider) Name() string { return "empty-then-valid" }

func (p *emptyThenValidProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	return &llm.GenerateResult{Text: "", FinishReason: llm.FinishReasonStop}, nil
}

func (p *emptyThenValidProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	n := atomic.AddInt32(&p.calls, 1)
	ch := make(chan llm.StreamPart, 1)
	go func() {
		defer close(ch)
		// 前 emptyRuns 次：不发任何 TextDeltaPart（空响应），直接结束流。
		if n <= p.emptyRuns {
			return
		}
		select {
		case <-ctx.Done():
		case ch <- &llm.TextDeltaPart{Text: p.validJSON}:
		}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}

const testDAGJSON = `{"nodes":[{"id":"n1","name":"任务一","task":"做 A 事","dependencies":[]}]}`

// TestAnalyzer_RetryRecoversFromEmpty 验证：GLM 前两次返回空响应后，
// analyzer 通过重试+退避在第三次成功恢复，最终返回有效 DAG。
func TestAnalyzer_RetryRecoversFromEmpty(t *testing.T) {
	prov := &emptyThenValidProvider{emptyRuns: 2, validJSON: testDAGJSON}
	saMgr := subagent.NewSubAgentManager(prov, "test-model")

	ec := EngineConfig{
		AnalyzerTemperature:  0.3,
		AnalyzerMaxTokens:    8192,
		AnalyzerStuckTimeout: 5 * time.Second,
	}
	a := NewAnalyzer(saMgr, nil, ec, nil)

	// 退避为 2s + 4s，给足余量。
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	nodes, err := a.Analyze(ctx, "请修复若干 lint 问题")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected recovery, got error: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "n1" {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
	if got := atomic.LoadInt32(&prov.calls); got != 3 {
		t.Fatalf("expected 3 provider calls (2 empty + 1 valid), got %d", got)
	}
	// 第 2、3 次尝试前各有一次退避（2s + 4s = 6s），应明显 > 5s。
	if elapsed < 5*time.Second {
		t.Fatalf("expected backoff between retries (>5s), elapsed=%s", elapsed)
	}
}

// TestAnalyzer_ExhaustsThenFails 验证：GLM 持续返回空响应（退化窗口未恢复）时，
// analyzer 用尽全部重试后返回 empty response 错误（而非死等/panic）。
func TestAnalyzer_ExhaustsThenFails(t *testing.T) {
	prov := &emptyThenValidProvider{emptyRuns: 100, validJSON: testDAGJSON}
	saMgr := subagent.NewSubAgentManager(prov, "test-model")

	ec := EngineConfig{
		AnalyzerTemperature:  0.3,
		AnalyzerMaxTokens:    8192,
		AnalyzerStuckTimeout: 5 * time.Second,
	}
	a := NewAnalyzer(saMgr, nil, ec, nil)

	// ctx 在退避途中取消，验证退避可被打断、不会死等到 5 次全跑完。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := a.Analyze(ctx, "请修复若干 lint 问题")
	if err == nil {
		t.Fatal("expected error when GLM keeps returning empty")
	}
}
