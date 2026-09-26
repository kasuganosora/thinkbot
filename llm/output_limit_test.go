package llm

import (
	"context"
	"strings"
	"testing"
)

func TestResolveMaxOutputTokens(t *testing.T) {
	cases := []struct {
		name                         string
		modelMax, opCap, fallback, w int
	}{
		{"configured model limit wins (prod glm-5.3)", 128000, 0, 4096, 128000},
		{"operator cap lowers", 128000, 16384, 4096, 16384},
		{"operator cap cannot raise", 8192, 50000, 4096, 8192},
		{"unknown model → operator cap", 0, 1500, 4096, 1500},
		{"unknown model → fallback", 0, 0, 4096, 4096},
		{"nothing → default", 0, 0, 0, DefaultMaxOutputTokens},
	}
	for _, c := range cases {
		if got := ResolveMaxOutputTokens(c.modelMax, c.opCap, c.fallback); got != c.w {
			t.Errorf("%s: got %d, want %d", c.name, got, c.w)
		}
	}
}

func TestFitOutputToContext(t *testing.T) {
	if got := FitOutputToContext(128000, 1000000, 50000, 1024); got != 128000 {
		t.Errorf("1M window: got %d", got)
	}
	if got := FitOutputToContext(128000, 131072, 30000, 1024); got != 131072-30000-2048 {
		t.Errorf("128k window: got %d", got)
	}
	if got := FitOutputToContext(128000, 0, 30000, 1024); got != 128000 {
		t.Errorf("unknown window must not change: got %d", got)
	}
	if got := FitOutputToContext(128000, 32000, 31000, 1024); got != 1024 {
		t.Errorf("floor: got %d", got)
	}
}

func overflowingConvo() []Message {
	var msgs []Message
	for i := 0; i < 12; i++ {
		msgs = append(msgs, UserMessage(strings.Repeat("question about the project ", 40)))
		msgs = append(msgs, AssistantMessage(strings.Repeat("detailed answer text ", 40)))
	}
	return msgs
}

func smallCompactor() *Compactor {
	return NewCompactor(CompactionConfig{
		MaxTokens:            3000,
		ReservedTokens:       500,
		TailTokens:           400,
		TailTurns:            1,
		MinMessagesToCompact: 2,
		Auto:                 true,
	})
}

// The automatic compactor summarizes with the same model as the main call, so
// it must send that call's configured output limit (params.MaxTokens =
// ModelDef.MaxTokens), not the old hardcoded compaction.summary_max_tokens=4096.
func TestCompactor_SummaryHonorsConfiguredModelMaxTokens(t *testing.T) {
	c := smallCompactor()
	p := &summaryFakeProvider{text: "## Goal\n- summary", finish: FinishReasonStop}
	maxTok := 128000
	params := GenerateParams{Model: ChatModel("glm-5.3"), Messages: overflowingConvo(), MaxTokens: &maxTok}
	out, err := c.Compact(context.Background(), params, p)
	if err != nil {
		t.Fatal(err)
	}
	if p.maxTokens != 128000 {
		t.Fatalf("summary max_tokens=%d, want the configured 128000", p.maxTokens)
	}
	if len(out.Messages) >= len(params.Messages) {
		t.Fatalf("expected compaction, got %d → %d messages", len(params.Messages), len(out.Messages))
	}

	// An explicit compaction.summary_max_tokens only lowers it.
	c2 := smallCompactor()
	cur := c2.liveConfig()
	cur.SummaryMaxTokens = 6000
	c2.SetConfigSource(func() CompactionConfig { return cur })
	if _, err := c2.Compact(context.Background(), params, p); err != nil {
		t.Fatal(err)
	}
	if p.maxTokens != 6000 {
		t.Fatalf("operator cap: max_tokens=%d, want 6000", p.maxTokens)
	}
}

// A summary cut off at the output limit must not replace the messages (it
// would silently drop context and become the anchor for later summaries).
func TestCompactor_TruncatedSummaryLeavesMessages(t *testing.T) {
	c := smallCompactor()
	p := &summaryFakeProvider{text: "## Goal\n- partial", finish: FinishReasonLength}
	maxTok := 128000
	params := GenerateParams{Model: ChatModel("glm-5.3"), Messages: overflowingConvo(), MaxTokens: &maxTok}
	out, err := c.Compact(context.Background(), params, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != len(params.Messages) || c.PreviousSummary() != "" {
		t.Fatalf("truncated summary must be discarded: %d → %d, anchor=%q", len(params.Messages), len(out.Messages), c.PreviousSummary())
	}
}

func TestSummarizeHead_HonorsModelMaxTokensAndRejectsCutoff(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig())
	p := &summaryFakeProvider{text: "## Goal\n- head", finish: FinishReasonStop}
	head := overflowingConvo()[:4]
	sum, err := c.SummarizeHead(context.Background(), p, "glm-5.3", head, 128000)
	if err != nil || sum == "" || p.maxTokens != 128000 {
		t.Fatalf("sum=%q err=%v max_tokens=%d (want 128000)", sum, err, p.maxTokens)
	}
	if _, err := c.SummarizeHead(context.Background(), p, "m", head, 0); err != nil || p.maxTokens != DefaultMaxOutputTokens {
		t.Fatalf("unknown model limit → default fallback, got %d (%v)", p.maxTokens, err)
	}
	p.finish = FinishReasonLength
	if _, err := c.SummarizeHead(context.Background(), p, "glm-5.3", head, 128000); err == nil {
		t.Fatal("cut-off head summary must return an error")
	}
}
