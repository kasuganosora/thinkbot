package memory

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestDedupCallStats_CountsTruncated(t *testing.T) {
	c0, t0 := DedupCallStats()
	p := &policyRecorder{response: `[]`}
	_, _ = ClusterMerge(context.Background(), p, llm.ChatModel("glm-5.3"), "", guardInputs, 128000, nil)
	p.finish, p.response = llm.FinishReasonLength, `[{"source_ids":["a"`
	_, err := ClusterMerge(context.Background(), p, llm.ChatModel("glm-5.3"), "", guardInputs, 128000, nil)
	if err == nil {
		t.Fatal("truncated answer must return an error")
	}
	c1, t1 := DedupCallStats()
	if c1-c0 != 2 || t1-t0 != 1 {
		t.Fatalf("calls +%d truncated +%d, want +2 +1", c1-c0, t1-t0)
	}
}
