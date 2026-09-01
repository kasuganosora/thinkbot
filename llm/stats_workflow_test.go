package llm

import (
	"context"
	"testing"
)

// ============================================================================
// 工作流维度的 ctx 传递
// ============================================================================

func TestWithStatsWorkflow_RoundTrip(t *testing.T) {
	ctx := WithStatsWorkflow(context.Background(), "wf-1", "n1")

	wfID, nodeID := statsWorkflowFromContext(ctx)
	if wfID != "wf-1" {
		t.Errorf("WorkflowID: got %q, want %q", wfID, "wf-1")
	}
	if nodeID != "n1" {
		t.Errorf("NodeID: got %q, want %q", nodeID, "n1")
	}
}

func TestStatsWorkflowFromContext_EmptyByDefault(t *testing.T) {
	wfID, nodeID := statsWorkflowFromContext(context.Background())
	if wfID != "" || nodeID != "" {
		t.Errorf("expected empty for a bare context, got (%q, %q)", wfID, nodeID)
	}
}

// TestWithStatsWorkflow_DoesNotClearSkip 工作流维度**不**清除 skip 标志。
//
// 与 WithStatsFeature 的语义刻意不同：feature 表示「我要记录」故清 skip，
// 而 workflow 只是给已决定要记录的调用补充归因维度。
// 若它也清 skip，会让 pipeline 内部本应跳过的调用被重复计数。
func TestWithStatsWorkflow_DoesNotClearSkip(t *testing.T) {
	ctx := WithStatsSkip(context.Background())
	ctx = WithStatsFeature(ctx, "reply")
	ctx = WithStatsWorkflow(ctx, "wf-1", "n1")

	// feature 应当已清除 skip
	if shouldSkipStats(ctx) {
		t.Error("WithStatsFeature should have cleared the skip flag")
	}

	// 单独用 workflow 时不应改变 skip 状态
	skipped := WithStatsWorkflow(WithStatsSkip(context.Background()), "wf-1", "n1")
	if !shouldSkipStats(skipped) {
		t.Error("WithStatsWorkflow alone must NOT clear the skip flag")
	}
}

// TestWithStatsWorkflow_Overwrite 后设置的值覆盖先前的（节点切换场景）。
func TestWithStatsWorkflow_Overwrite(t *testing.T) {
	ctx := WithStatsWorkflow(context.Background(), "wf-1", "n1")
	ctx = WithStatsWorkflow(ctx, "wf-1", "n2")

	wfID, nodeID := statsWorkflowFromContext(ctx)
	if wfID != "wf-1" || nodeID != "n2" {
		t.Errorf("expected (wf-1, n2), got (%q, %q)", wfID, nodeID)
	}
}
