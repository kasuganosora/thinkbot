package llm

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// fakeEventProvider 是仅记录调用、返回固定结果的 Provider 实现，用于验证事件发射。
type fakeEventProvider struct {
	name string
}

func (p *fakeEventProvider) Name() string { return p.name }

func (p *fakeEventProvider) DoGenerate(_ context.Context, _ GenerateParams) (*GenerateResult, error) {
	return &GenerateResult{
		Text:         "ok",
		FinishReason: FinishReasonStop,
		Usage:        Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}, nil
}

func (p *fakeEventProvider) DoStream(_ context.Context, _ GenerateParams) (*StreamResult, error) {
	ch := make(chan StreamPart, 4)
	ch <- &TextDeltaPart{Text: "hello"}
	ch <- &FinishPart{
		FinishReason: FinishReasonStop,
		TotalUsage:   Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10},
	}
	close(ch)
	return &StreamResult{Stream: ch}, nil
}

func TestStatsRecordingProvider_EmitsLLMEvents(t *testing.T) {
	sink := core.NewMemorySink(64)
	ctx := core.WithEventSink(context.Background(), sink)

	p := NewStatsRecordingProvider(&fakeEventProvider{name: "fake"}, nil, "bot-x")

	// 非流式
	if _, err := p.DoGenerate(ctx, GenerateParams{Model: &Model{ID: "glm-test"}}); err != nil {
		t.Fatalf("DoGenerate: %v", err)
	}

	// 流式：需消费流以触发 wrapStream 中流结束的响应事件
	sr, err := p.DoStream(ctx, GenerateParams{Model: &Model{ID: "glm-test"}})
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	for range sr.Stream {
	}

	events := sink.Snapshot()
	if len(events) != 4 {
		t.Fatalf("expected 4 events (2 gen + 2 stream), got %d: %+v", len(events), events)
	}

	wantKinds := []core.EventKind{
		core.EventLLMRequest, core.EventLLMResponse, // DoGenerate
		core.EventLLMRequest, core.EventLLMResponse, // DoStream
	}
	for i, k := range wantKinds {
		if events[i].Kind != k {
			t.Errorf("event %d kind = %q, want %q", i, events[i].Kind, k)
		}
		if events[i].Source != "llm:fake" {
			t.Errorf("event %d source = %q, want llm:fake", i, events[i].Source)
		}
	}

	// 校验 DoGenerate 响应事件载荷含 token 用量
	resp := events[1].Payload.(map[string]any)
	usage, ok := resp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("response payload missing usage: %+v", resp)
	}
	if usage["total"] != 15 {
		t.Errorf("usage.total = %v, want 15", usage["total"])
	}
	if resp["finish_reason"] != string(FinishReasonStop) {
		t.Errorf("finish_reason = %v, want stop", resp["finish_reason"])
	}

	// 校验流式响应事件（最后一条）的总用量
	streamResp := events[3].Payload.(map[string]any)
	streamUsage := streamResp["usage"].(map[string]any)
	if streamUsage["total"] != 10 {
		t.Errorf("stream usage.total = %v, want 10", streamUsage["total"])
	}
}

func TestStatsRecordingProvider_NoSinkNoPanic(t *testing.T) {
	// 无 sink（NoopSink）时不应 panic，事件被静默丢弃。
	p := NewStatsRecordingProvider(&fakeEventProvider{name: "fake"}, nil, "bot-x")
	if _, err := p.DoGenerate(context.Background(), GenerateParams{}); err != nil {
		t.Fatalf("DoGenerate: %v", err)
	}
}
