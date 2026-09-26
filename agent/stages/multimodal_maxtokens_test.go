package stages

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// The vision call must send the vision model's configured maxTokens
// (wired from VisionDef.MaxTokens); 1024 is only the unconfigured fallback.
func TestMultimodal_TranscribeHonorsConfiguredMaxTokens(t *testing.T) {
	att := core.Attachment{Type: core.AttachmentTypeImage, MimeType: "image/png", URL: "https://example.com/cat.png"}
	for _, c := range []struct {
		cfg  *int
		want int
	}{
		{intPtr(32768), 32768},
		{nil, 1024},
		{intPtr(0), 1024},
	} {
		p := &mockVisionProvider{name: "vision"}
		s := newTestMultimodalStage(t, MultimodalConfig{VisionProvider: p, VisionModel: llm.ChatModel("glm-4v"), MaxTokens: c.cfg})
		if _, err := s.transcribeAttachment(context.Background(), att, "what is this?"); err != nil {
			t.Fatal(err)
		}
		if p.lastParam == nil || p.lastParam.MaxTokens == nil || *p.lastParam.MaxTokens != c.want {
			t.Fatalf("cfg=%v: max_tokens=%v, want %d", c.cfg, p.lastParam.MaxTokens, c.want)
		}
	}
}

func intPtr(v int) *int { return &v }

// Vision runs with the vision reasoning policy (low) and an optional cap.
func TestMultimodal_ReasoningPolicy(t *testing.T) {
	att := core.Attachment{Type: core.AttachmentTypeImage, MimeType: "image/png", URL: "https://example.com/cat.png"}
	set := llm.InternalSettings{MaxTokens: map[string]int{llm.PurposeVision: 4000}}
	for _, c := range []struct {
		pol        *llm.InternalPolicy
		model      string
		wantEffort string
		wantMax    int
	}{
		{llm.NewInternalPolicy(nil, "medium"), "glm-5.3", "low", 32768},
		{llm.NewInternalPolicy(func() llm.InternalSettings { return set }, "medium"), "glm-5.3", "low", 4000},
		{llm.NewInternalPolicy(nil, ""), "gpt-4o", "", 32768},
		{nil, "glm-5.3", "", 32768},
	} {
		p := &mockVisionProvider{name: "vision"}
		s := newTestMultimodalStage(t, MultimodalConfig{VisionProvider: p, VisionModel: llm.ChatModel(c.model), MaxTokens: intPtr(32768), InternalPolicy: c.pol})
		if _, err := s.transcribeAttachment(context.Background(), att, "?"); err != nil {
			t.Fatal(err)
		}
		e := ""
		if p.lastParam.ReasoningEffort != nil {
			e = *p.lastParam.ReasoningEffort
		}
		if e != c.wantEffort || *p.lastParam.MaxTokens != c.wantMax {
			t.Fatalf("%s: effort=%q max=%d, want %q/%d", c.model, e, *p.lastParam.MaxTokens, c.wantEffort, c.wantMax)
		}
	}
}
