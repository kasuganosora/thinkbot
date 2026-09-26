package stages

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// chunkStreamProvider 按给定切片逐段流式输出文本。
type chunkStreamProvider struct{ chunks []string }

func (p *chunkStreamProvider) Name() string { return "chunk-stream" }
func (p *chunkStreamProvider) DoGenerate(context.Context, llm.GenerateParams) (*llm.GenerateResult, error) {
	return &llm.GenerateResult{Text: strings.Join(p.chunks, "")}, nil
}
func (p *chunkStreamProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	ch := make(chan llm.StreamPart, len(p.chunks)+2)
	for _, c := range p.chunks {
		ch <- &llm.TextDeltaPart{Text: c}
	}
	usage := llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	ch <- &llm.FinishStepPart{FinishReason: llm.FinishReasonStop, Usage: usage}
	ch <- &llm.FinishPart{FinishReason: llm.FinishReasonStop, TotalUsage: usage}
	close(ch)
	return &llm.StreamResult{Stream: ch}, nil
}

// recordingPublisher 记录推给 EventBus 的文本增量。
type recordingPublisher struct {
	mu     sync.Mutex
	deltas []string
}

func (r *recordingPublisher) PublishTextDelta(_ context.Context, _, _, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deltas = append(r.deltas, text)
}
func (r *recordingPublisher) PublishToolCall(context.Context, string, string, string, string, any) {}
func (r *recordingPublisher) PublishToolProgress(context.Context, string, string, string, string, string, any) {
}
func (r *recordingPublisher) PublishToolResult(context.Context, string, string, string, string, string, any, string) {
}

// TestProcessStream_ReplyControlMarkerNeverPublished 端到端锁定：
// 控制块被切成 token 级多段时，推给 EventBus（→ web SSE / resume / 落库 parts）的增量
// 不含任何控制标记；而返回给下游的 result.Text 仍保留原文，send 信号照常可解析。
func TestProcessStream_ReplyControlMarkerNeverPublished(t *testing.T) {
	chunks := []string{"部署测试收到", "，一切正常 ✅", "\n\n", "@@", "REPLY", "_CONTROL", "@@", "{\"", "send", "\":", " true", "}"}
	pub := &recordingPublisher{}
	s := &LLMStage{
		name:     "llm",
		provider: &chunkStreamProvider{chunks: chunks},
		config:   LLMConfig{StreamPublisher: pub},
		logger:   zap.NewNop().Sugar(),
	}
	env := &core.Envelope{Message: core.Message{ID: "m1", TraceID: "t1", BotID: "b1"}}
	res, err := s.processStream(context.Background(), env, &llm.OrchestrateConfig{}, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("processStream: %v", err)
	}
	streamed := strings.Join(pub.deltas, "")
	for _, d := range pub.deltas {
		if strings.Contains(d, "@@") || strings.Contains(d, "REPLY") || strings.Contains(d, "send") {
			t.Fatalf("control marker fragment leaked into published delta %q (all=%q)", d, pub.deltas)
		}
	}
	if streamed != "部署测试收到，一切正常 ✅" {
		t.Fatalf("streamed = %q", streamed)
	}
	// 控制信号必须仍可从原文解析（fail-closed 门控依赖它）。
	send, clean, ok := parseReplyControl(res.Text)
	if !ok || !send || clean != "部署测试收到，一切正常 ✅" {
		t.Fatalf("parseReplyControl(result.Text) = (%v, %q, %v), want (true, clean, true); text=%q", send, clean, ok, res.Text)
	}
}
