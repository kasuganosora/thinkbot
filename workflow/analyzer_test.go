package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	prov := &emptyThenValidProvider{emptyRuns: 2, validJSON: "<result>" + testDAGJSON + "</result>"}
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
	nodes, err := a.Analyze(ctx, "请修复若干 lint 问题", false)
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
	prov := &emptyThenValidProvider{emptyRuns: 100, validJSON: "<result>" + testDAGJSON + "</result>"}
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

	_, err := a.Analyze(ctx, "请修复若干 lint 问题", false)
	if err == nil {
		t.Fatal("expected error when GLM keeps returning empty")
	}
}

// reasonRecordingProvider 按 responses 顺序逐次返回，并完整记录每次请求里
// 所有消息的文本内容（用于断言「重试时是否把失败原因回传给了模型」）。
type reasonRecordingProvider struct {
	mu        sync.Mutex
	calls     int
	responses []string
	allText   []string
}

func (p *reasonRecordingProvider) Name() string { return "reason-rec" }

func (p *reasonRecordingProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	return &llm.GenerateResult{Text: "", FinishReason: llm.FinishReasonStop}, nil
}

func (p *reasonRecordingProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	p.mu.Lock()
	idx := p.calls
	p.calls++
	var resp string
	if idx < len(p.responses) {
		resp = p.responses[idx]
	} else if len(p.responses) > 0 {
		resp = p.responses[len(p.responses)-1]
	}
	var sb strings.Builder
	for _, m := range params.Messages {
		for _, part := range m.Content {
			if tp, ok := part.(llm.TextPart); ok {
				sb.WriteString(tp.Text)
				sb.WriteString("\n")
			}
		}
	}
	p.allText = append(p.allText, sb.String())
	p.mu.Unlock()

	ch := make(chan llm.StreamPart, 1)
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
		case ch <- &llm.TextDeltaPart{Text: resp}:
		}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}

// TestAnalyzer_RetryCarriesValidateReason 验证：当生成的 DAG 未通过结构校验
// （如存在环）时，重试请求里必须带上具体的校验失败原因，让模型知道错在哪、怎么改。
func TestAnalyzer_RetryCarriesValidateReason(t *testing.T) {
	cycleJSON := `<result>{"nodes":[
		{"id":"n1","name":"A","task":"t","dependencies":["n2"]},
		{"id":"n2","name":"B","task":"t","dependencies":["n1"]}
	]}</result>`
	prov := &reasonRecordingProvider{responses: []string{cycleJSON, "<result>" + testDAGJSON + "</result>"}}
	saMgr := subagent.NewSubAgentManager(prov, "test-model")
	a := NewAnalyzer(saMgr, nil, EngineConfig{AnalyzerTemperature: 0.3, AnalyzerMaxTokens: 8192, AnalyzerStuckTimeout: 5 * time.Second}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodes, err := a.Analyze(ctx, "把需求拆成 DAG", false)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
	if got := prov.calls; got != 2 {
		t.Fatalf("expected 2 calls, got %d", got)
	}
	if len(prov.allText) < 2 {
		t.Fatalf("expected at least 2 captured requests, got %d", len(prov.allText))
	}
	second := prov.allText[1]
	if !strings.Contains(second, "未通过结构校验") || !strings.Contains(second, "cycle detected") {
		t.Fatalf("retry task missing validate reason:\n%s", second)
	}
}

// TestAnalyzer_RetryCarriesMissingTagReason 验证：当输出缺失 <result> 标签时，
// 重试请求里必须带上「用 <result> 包裹」的格式纠正提示。
func TestAnalyzer_RetryCarriesMissingTagReason(t *testing.T) {
	prov := &reasonRecordingProvider{responses: []string{testDAGJSON, "<result>" + testDAGJSON + "</result>"}}
	saMgr := subagent.NewSubAgentManager(prov, "test-model")
	a := NewAnalyzer(saMgr, nil, EngineConfig{AnalyzerTemperature: 0.3, AnalyzerMaxTokens: 8192, AnalyzerStuckTimeout: 5 * time.Second}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodes, err := a.Analyze(ctx, "把需求拆成 DAG", false)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("unexpected nodes: %+v", nodes)
	}
	if got := prov.calls; got != 2 {
		t.Fatalf("expected 2 calls, got %d", got)
	}
	if len(prov.allText) < 2 {
		t.Fatalf("expected at least 2 captured requests, got %d", len(prov.allText))
	}
	second := prov.allText[1]
	if !strings.Contains(second, "缺少") || !strings.Contains(second, "<result>") {
		t.Fatalf("retry task missing missing-tag hint:\n%s", second)
	}
}

// TestParseDAGSpec_ResultTag 验证分析器输出包裹在 <result>...</result> 时的解析，
// 以及缺失/未闭合标签时返回 errNoResultTag（触发「打回重做」）。
func TestParseDAGSpec_ResultTag(t *testing.T) {
	// 1. 正常：<result> 包裹 + 前置推理文本
	wrapped := "好的，下面是拆解：\n<result>" + testDAGJSON + "</result>"
	spec, err := parseDAGSpec(wrapped)
	if err != nil {
		t.Fatalf("wrapped parse failed: %v", err)
	}
	if len(spec.Nodes) != 1 || spec.Nodes[0].ID != "n1" {
		t.Fatalf("unexpected nodes: %+v", spec.Nodes)
	}

	// 2. 缺失 <result> 标签（旧式裸 JSON）→ errNoResultTag
	if _, err := parseDAGSpec(testDAGJSON); !errors.Is(err, errNoResultTag) {
		t.Fatalf("expected errNoResultTag for bare JSON, got %v", err)
	}

	// 3. 有开标签无闭标签 → errNoResultTag
	if _, err := parseDAGSpec("<result>" + testDAGJSON); !errors.Is(err, errNoResultTag) {
		t.Fatalf("expected errNoResultTag for unclosed tag, got %v", err)
	}

	// 4. markdown 代码块包裹在 <result> 外层也能提取
	fenced := "```json\n<result>" + testDAGJSON + "</result>\n```"
	if _, err := parseDAGSpec(fenced); err != nil {
		t.Fatalf("fenced+result parse failed: %v", err)
	}

	// 5. extractResultTag 取最后一个闭合对（避免中间步骤误判）
	last := "reasoning <result>{ \"nodes\": [] }</result> final <result>" + testDAGJSON + "</result>"
	if inner, ok := extractResultTag(last); !ok || inner != testDAGJSON {
		t.Fatalf("extractResultTag wrong: ok=%v inner=%q", ok, inner)
	}
}
