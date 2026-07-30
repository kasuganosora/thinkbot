package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

func TestStaleThreshold(t *testing.T) {
	cases := map[WorkflowStatus]time.Duration{
		WorkflowAnalyzing:   analyzingStaleMaxAge,
		WorkflowRunning:     runningStaleMaxAge,
		WorkflowInterrupted: interruptedStaleMaxAge,
		WorkflowCompleted:   0,
		WorkflowFailed:      0,
		WorkflowTerminated:  0,
	}
	for status, want := range cases {
		if got := staleThreshold(status); got != want {
			t.Fatalf("staleThreshold(%s) = %v, want %v", status, got, want)
		}
	}
}

// TestForceFailStale 验证卡死看门狗能把长期无进展的工作流强制失败并落库。
func TestForceFailStale(t *testing.T) {
	repo := NewRepository(nil, zap.NewNop().Sugar())
	m := NewManager(repo, nil, nil, noop_trace.NewTracerProvider(), EngineConfig{}, zap.NewNop().Sugar(), nil)

	wf := NewWorkflow("wf-stale", "需求", nil)
	wf.Status = WorkflowAnalyzing
	wf.UpdatedAt = time.Now().Add(-30 * time.Minute)

	m.forceFailStale(wf, 30*time.Minute)

	if wf.Status != WorkflowFailed {
		t.Fatalf("in-memory status = %v, want failed", wf.Status)
	}
	if !strings.Contains(wf.Error, "卡死") {
		t.Fatalf("error missing stuck message: %q", wf.Error)
	}

	got, err := repo.Get("wf-stale")
	if err != nil {
		t.Fatalf("repo.Get failed: %v", err)
	}
	if got.Status != WorkflowFailed {
		t.Fatalf("persisted status = %v, want failed", got.Status)
	}
}

// TestSweepStaleSkipsFresh 验证看门狗不会误杀刚创建/活跃的工作流。
func TestSweepStaleSkipsFresh(t *testing.T) {
	repo := NewRepository(nil, zap.NewNop().Sugar())
	m := NewManager(repo, nil, nil, noop_trace.NewTracerProvider(), EngineConfig{}, zap.NewNop().Sugar(), nil)

	wf := NewWorkflow("wf-fresh", "需求", nil)
	wf.Status = WorkflowAnalyzing
	wf.UpdatedAt = time.Now() // 刚刚落库，不应被判定卡死
	_ = repo.Save(wf)

	m.SweepStale(context.Background())

	got, err := repo.Get("wf-fresh")
	if err != nil {
		t.Fatalf("repo.Get failed: %v", err)
	}
	if got.Status != WorkflowAnalyzing {
		t.Fatalf("fresh workflow status = %v, want analyzing (should NOT be swept)", got.Status)
	}
}
