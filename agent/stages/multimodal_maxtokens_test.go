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
