package memory

import (
	"context"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// maxTokRecorder records the max_tokens of every call.
type maxTokRecorder struct {
	mu       sync.Mutex
	response string
	got      []int
}

func (m *maxTokRecorder) Name() string { return "rec" }
func (m *maxTokRecorder) DoGenerate(_ context.Context, p llm.GenerateParams) (*llm.GenerateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := -1 // not sent
	if p.MaxTokens != nil {
		v = *p.MaxTokens
	}
	m.got = append(m.got, v)
	return &llm.GenerateResult{Text: m.response, FinishReason: llm.FinishReasonStop}, nil
}
func (m *maxTokRecorder) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

// memory_dedup used to send no max_tokens at all (server default).
func TestClusterMerge_SendsConfiguredModelMaxTokens(t *testing.T) {
	p := &maxTokRecorder{response: `[]`}
	in := []ClusterInput{{ID: "a", Content: "x"}, {ID: "b", Content: "y"}}
	if _, err := ClusterMerge(context.Background(), p, llm.ChatModel("glm-5.3"), "", in, 128000, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ClusterMerge(context.Background(), p, llm.ChatModel("glm-5.3"), "", in, 0, nil); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 2 || p.got[0] != 128000 || p.got[1] != DefaultGenerationMaxTokens {
		t.Fatalf("max_tokens=%v, want [128000 %d]", p.got, DefaultGenerationMaxTokens)
	}
}

// user_profiler used the fixed DefaultGenerationMaxTokens constant.
func TestLLMProfiler_HonorsConfiguredMaxTokens(t *testing.T) {
	p := &maxTokRecorder{response: `[]`}
	cfg := DefaultLLMProfilerConfig()
	cfg.Provider = p
	cfg.Model = llm.ChatModel("glm-5.3")
	cfg.MaxTokens = 128000
	prof := NewLLMProfiler(cfg, testTracerProvider(), testLogger())
	if _, err := prof.callLLM(context.Background(), "extract"); err != nil {
		t.Fatal(err)
	}
	cfg.MaxTokens = 0
	prof = NewLLMProfiler(cfg, testTracerProvider(), testLogger())
	if _, err := prof.callLLM(context.Background(), "extract"); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 2 || p.got[0] != 128000 || p.got[1] != DefaultGenerationMaxTokens {
		t.Fatalf("max_tokens=%v", p.got)
	}
}
