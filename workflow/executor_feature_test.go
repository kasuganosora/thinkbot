package workflow

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// 节点内 LLM 调用必须带功能标签，否则日聚合表记成 "unknown"，无法归因也不受功能预算约束。
func TestWithNodeContextSetsWorkflowFeature(t *testing.T) {
	e := &Executor{}
	ctx := e.withNodeContext(context.Background(), &DAGNode{ID: "n1"}, nil)
	if got := llm.StatsFeatureFromContext(ctx); got != "workflow" {
		t.Fatalf("feature = %q, want workflow", got)
	}
	// 上游已有标签时保持不变
	ctx = e.withNodeContext(llm.WithStatsFeature(context.Background(), "cron"), &DAGNode{ID: "n1"}, nil)
	if got := llm.StatsFeatureFromContext(ctx); got != "cron" {
		t.Fatalf("feature = %q, want cron", got)
	}
}
