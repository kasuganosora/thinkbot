package llm

import (
	"context"
	"testing"
)

// The automatic compactor and SummarizeHead must send the policy's
// reasoning_effort (GLM-5.3 defaults to "max" when it is absent) and honor a
// per-purpose output cap; without a usable signal nothing is sent.
func TestCompactor_InternalPolicyReasoningAndCap(t *testing.T) {
	maxTok := 128000
	params := GenerateParams{Model: ChatModel("glm-5.3"), Messages: overflowingConvo(), MaxTokens: &maxTok}

	p := &summaryFakeProvider{text: "## Goal\n- s", finish: FinishReasonStop}
	c := smallCompactor().SetInternalPolicy(NewInternalPolicy(nil, "medium"))
	if _, err := c.Compact(context.Background(), params, p); err != nil {
		t.Fatal(err)
	}
	if p.effort != "low" || p.maxTokens != 128000 {
		t.Fatalf("auto-compact: effort=%q max=%d, want low/128000", p.effort, p.maxTokens)
	}

	set := InternalSettings{MaxTokens: map[string]int{PurposeAutoCompact: 20000, PurposeSummarizeHead: 12000}}
	c2 := smallCompactor().SetInternalPolicy(NewInternalPolicy(func() InternalSettings { return set }, "medium"))
	if _, err := c2.Compact(context.Background(), params, p); err != nil {
		t.Fatal(err)
	}
	if p.maxTokens != 20000 {
		t.Fatalf("auto_compact cap: %d", p.maxTokens)
	}
	if _, err := c2.SummarizeHead(context.Background(), p, "glm-5.3", overflowingConvo()[:4], 128000); err != nil {
		t.Fatal(err)
	}
	if p.effort != "low" || p.maxTokens != 12000 {
		t.Fatalf("summarize_head: effort=%q max=%d", p.effort, p.maxTokens)
	}

	// Bot without reasoning_effort on an unknown model → parameter not sent.
	unknown := GenerateParams{Model: ChatModel("gpt-4o"), Messages: overflowingConvo(), MaxTokens: &maxTok}
	c3 := smallCompactor().SetInternalPolicy(NewInternalPolicy(nil, ""))
	if _, err := c3.Compact(context.Background(), unknown, p); err != nil {
		t.Fatal(err)
	}
	if p.effort != "" {
		t.Fatalf("must not send reasoning_effort, got %q", p.effort)
	}
	if _, err := smallCompactor().SummarizeHead(context.Background(), p, "glm-5.3", overflowingConvo()[:4], 128000); err != nil || p.effort != "" {
		t.Fatalf("no policy → not sent, got %q (%v)", p.effort, err)
	}
}
