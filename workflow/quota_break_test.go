package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// quotaExhaustedErr 模拟「GLM 5 小时滚动配额打满」的错误：429 + 额度用尽文案。
// 必须同时含限流状态码（429）与额度特征（使用上限）才能被 IsQuotaExhaustedLoose 识别。
func quotaExhaustedErr() error {
	return errors.New("HTTP 429: 您在当前时间段的请求已达到 5 小时的使用上限，限额将在 2026-08-07 20:06:23 重置")
}

// TestQuotaBreaker_TripsWorkflow 验证：节点因配额耗尽失败时，整条工作流熔断为
// interrupted、故障节点退回 pending（不记 failed）、下游不级联跳过、并记录恢复时刻。
//
// 这是 2026-08-07 事故（wf-71f5988de878028d6a3dcb6a）的直接回归——旧行为会把配额
// 故障当普通节点失败，级联跳过下游，毁掉整条链。
func TestQuotaBreaker_TripsWorkflow(t *testing.T) {
	wf := NewWorkflow("wf-quota", "req", []*DAGNode{
		{ID: "n1", Name: "task1", Task: "do something"},
		{ID: "n2", Name: "task2", Task: "depend on n1", Dependencies: []string{"n1"}},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{execErrors: []error{quotaExhaustedErr()}}
	s := newMockScheduler(wf, exec)

	finalStatus := s.Run(context.Background())

	if finalStatus != WorkflowInterrupted {
		t.Fatalf("expected WorkflowInterrupted, got %s", finalStatus)
	}
	if wf.Status != WorkflowInterrupted {
		t.Errorf("wf.Status = %s, want interrupted", wf.Status)
	}

	n1, _ := wf.GetNode("n1")
	if n1.Status != NodePending {
		t.Errorf("quota-failed node should be reset to pending (preserve for resume), got %s", n1.Status)
	}
	if n1.Status == NodeFailed {
		t.Errorf("quota-failed node must NOT be marked failed (would cascade-skip downstream)")
	}

	n2, _ := wf.GetNode("n2")
	if n2.Status == NodeSkipped {
		t.Errorf("downstream node must NOT be cascade-skipped on quota break, got %s", n2.Status)
	}
	if n2.Status != NodePending {
		t.Errorf("downstream node should stay pending, got %s", n2.Status)
	}

	if wf.QuotaResumeAt == nil {
		t.Errorf("QuotaResumeAt must be set so the watchdog can resume")
	} else if !wf.QuotaResumeAt.After(time.Now()) {
		t.Errorf("QuotaResumeAt should be in the future, got %v", *wf.QuotaResumeAt)
	}
	if wf.QuotaBreaks != 1 {
		t.Errorf("QuotaBreaks = %d, want 1", wf.QuotaBreaks)
	}
}

// TestQuotaBreaker_ExhaustedFails 验证：累计熔断次数超过上限后，按失败收尾且不再设
// 恢复时刻——避免「恢复→立刻又限流→再熔断」无限循环。
func TestQuotaBreaker_ExhaustedFails(t *testing.T) {
	wf := NewWorkflow("wf-quota-ex", "req", []*DAGNode{
		{ID: "n1", Name: "task1", Task: "do something"},
	})
	wf.RebuildIndex()
	// 预置已达上限的熔断计数
	wf.QuotaBreaks = maxQuotaBreaks

	exec := &mockExecutor{execErrors: []error{quotaExhaustedErr()}}
	s := newMockScheduler(wf, exec)

	finalStatus := s.Run(context.Background())

	if finalStatus != WorkflowFailed {
		t.Fatalf("expected WorkflowFailed when breaks exhausted, got %s", finalStatus)
	}
	if wf.QuotaResumeAt != nil {
		t.Errorf("QuotaResumeAt must be cleared on exhausted failure, got %v", *wf.QuotaResumeAt)
	}
}

// TestQuotaBreaker_NoRetryAmplification 验证：配额耗尽错误在节点层被 ShouldRetry 立即
// 放弃（不做 MaxRetries 次重试）。旧行为会撞满重试次数放大请求量。
func TestQuotaBreaker_NoRetryAmplification(t *testing.T) {
	wf := NewWorkflow("wf-quota-rt", "req", []*DAGNode{
		{ID: "n1", Name: "task1", Task: "do something", MaxRetries: 5},
	})
	wf.RebuildIndex()

	exec := &mockExecutor{execErrors: []error{quotaExhaustedErr()}}
	s := newMockScheduler(wf, exec)

	s.Run(context.Background())

	// Execute 应只被调用 1 次（ShouldRetry 在首次配额错误即返回 false）。
	if got := exec.execCalls.Load(); got != 1 {
		t.Errorf("Execute calls = %d, want 1 (no retry amplification on quota exhaustion)", got)
	}
}

// TestQuotaBreaker_RespectsResetTime 验证：服务端明确告知了未来的重置时刻时，
// 熔断器的等待时长**尊重该真实时刻**（不被任何固定上限截断），从而实现
// 「遇到 429 等待到对应时间再重试」的无职守续跑。这是本优化的核心回归。
//
// 旧实现把 maxQuotaWait 硬编码为 90min，GLM 说 20:06 重置也只等 90min 就中奖式
// 续跑、立刻又撞墙——本测试杜绝回归。
func TestQuotaBreaker_RespectsResetTime(t *testing.T) {
	const hours = 4
	reset := time.Now().Add(hours * time.Hour).Format("2006-01-02 15:04:05")
	err := fmt.Errorf("HTTP 429: 您在当前时间段的请求已达到 5 小时的使用上限，限额将在 %s 重置", reset)

	now := time.Now()
	wait := quotaWaitFor(err, now).Sub(now)

	want := time.Duration(hours) * time.Hour
	if wait < want-time.Minute || wait > want+time.Minute {
		t.Fatalf("quotaWaitFor did not honor server reset time: wait=%v, want≈%v", wait, want)
	}
}

// TestQuotaBreaker_ClampsAbsurdResetTime 验证：服务端给出的重置时刻离谱地远
// （解析异常）时，仍被 hardQuotaWaitCap 收住，避免工作流无限挂起。
func TestQuotaBreaker_ClampsAbsurdResetTime(t *testing.T) {
	const hours = 100 // 远超 hardQuotaWaitCap=12h
	reset := time.Now().Add(hours * time.Hour).Format("2006-01-02 15:04:05")
	err := fmt.Errorf("HTTP 429: 已达到使用上限，限额将在 %s 重置", reset)

	now := time.Now()
	wait := quotaWaitFor(err, now).Sub(now)
	if wait > hardQuotaWaitCap+time.Minute {
		t.Fatalf("quotaWaitFor exceeded hardQuotaWaitCap: wait=%v", wait)
	}
	if wait < minQuotaWait {
		t.Fatalf("quotaWaitFor absurd-clamp fell below min: wait=%v", wait)
	}
}
