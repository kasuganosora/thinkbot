package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// lengthThenStopProvider returns finish_reason=length for the first `cutoffs`
// calls (like a thinking model that spends the cap on reasoning) and a normal
// summary afterwards. It records the max_tokens of every call.
type lengthThenStopProvider struct {
	mu      sync.Mutex
	cutoffs int
	caps    []int
	efforts []string
}

func (p *lengthThenStopProvider) Name() string { return "len" }
func (p *lengthThenStopProvider) DoGenerate(_ context.Context, params GenerateParams) (*GenerateResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := 0
	if params.MaxTokens != nil {
		c = *params.MaxTokens
	}
	e := ""
	if params.ReasoningEffort != nil {
		e = *params.ReasoningEffort
	}
	p.caps = append(p.caps, c)
	p.efforts = append(p.efforts, e)
	if len(p.caps) <= p.cutoffs {
		return &GenerateResult{Text: "## Topics\n- cut", FinishReason: FinishReasonLength, Usage: Usage{OutputTokens: c}}, nil
	}
	return &GenerateResult{Text: "## Topics\n- full note", FinishReason: FinishReasonStop}, nil
}
func (p *lengthThenStopProvider) DoStream(context.Context, GenerateParams) (*StreamResult, error) {
	return nil, errors.New("not implemented")
}

func TestSummarizeForSelfCompact_RetriesOnceWithLargerCap(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig())
	head := selfCompactConvo()[:4]
	p := &lengthThenStopProvider{cutoffs: 1}
	sum, err := c.SummarizeForSelfCompact(context.Background(), p, ChatModel("glm-5.3"), head, SelfCompactOptions{
		MaxOutputTokens: 16384, RetryMaxOutputTokens: 32768, ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatalf("retry with a larger cap should succeed: %v", err)
	}
	if sum != "## Topics\n- full note" {
		t.Fatalf("summary=%q", sum)
	}
	if len(p.caps) != 2 || p.caps[0] != 16384 || p.caps[1] != 32768 {
		t.Fatalf("want caps [16384 32768], got %v", p.caps)
	}
	if p.efforts[0] != "low" || p.efforts[1] != "low" {
		t.Fatalf("reasoning effort must be passed on both calls: %v", p.efforts)
	}
}

func TestSummarizeForSelfCompact_AtMostOneRetry(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig())
	head := selfCompactConvo()[:4]
	p := &lengthThenStopProvider{cutoffs: 5}
	_, err := c.SummarizeForSelfCompact(context.Background(), p, ChatModel("m"), head, SelfCompactOptions{
		MaxOutputTokens: 4096, RetryMaxOutputTokens: 8192,
	})
	if !errors.Is(err, ErrSelfCompactSummaryTruncated) {
		t.Fatalf("want truncation error, got %v", err)
	}
	if !strings.Contains(err.Error(), "4096") || !strings.Contains(err.Error(), "8192") {
		t.Errorf("error should name the caps tried: %v", err)
	}
	if len(p.caps) != 2 {
		t.Fatalf("exactly one retry expected, calls=%d", len(p.caps))
	}
}

func TestSummarizeForSelfCompact_NoRetryWithoutLargerCap(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig())
	head := selfCompactConvo()[:4]
	for _, retry := range []int{0, 4096, 1000} {
		p := &lengthThenStopProvider{cutoffs: 5}
		_, err := c.SummarizeForSelfCompact(context.Background(), p, ChatModel("m"), head, SelfCompactOptions{
			MaxOutputTokens: 4096, RetryMaxOutputTokens: retry,
		})
		if !errors.Is(err, ErrSelfCompactSummaryTruncated) || len(p.caps) != 1 {
			t.Fatalf("retry=%d: want one call + truncation error, got calls=%d err=%v", retry, len(p.caps), err)
		}
	}
}
