package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/kasuganosora/thinkbot/agent/outbound"
	utilhttp "github.com/kasuganosora/thinkbot/util/http"
)

// ============================================================================
// 上游配额熔断（quota circuit breaker）
//
// 解决的问题（真实事故 wf-71f5988de878028d6a3dcb6a，2026-08-07 16:20）：
//
//	GLM 5 小时滚动配额打满 → HTTP 429 + body code 1308「已达到 5 小时的使用上限，
//	20:06:23 重置」。旧行为：
//	  · HTTP 流式层把 429 无条件当瞬时限流，重试 5 次
//	  · 节点层再包一层重试 3 次
//	  → 单节点撞墙 15 次，8 个失败节点合计 120 次**注定失败**的请求，
//	    还反过来加剧限流；失败后级联跳过下游，19 个已完成节点的成果就此停摆。
//
// 新行为分三级，缺一不可：
//  1. HTTP 层：utilhttp.StreamShouldRetry 识别配额耗尽 → 立即放弃（5 次 → 1 次）
//  2. 节点层：ShouldRetry 同样识别 → 立即放弃（3 次 → 1 次）
//  3. 工作流层：**首个**配额失败即熔断整条工作流，节点退回 pending、不级联跳过、
//     工作流置 interrupted 并记录恢复时刻，由看门狗到点自动续跑。
//
// 关键设计取舍：
//   - 配额失败的节点**不能记为 failed**。它没有真正执行过，记 failed 会触发级联
//     跳过并让最终状态变成 WorkflowFailed —— 那正是这次事故里「一个上游故障
//     毁掉整条链」的机制。退回 pending 才能在恢复后原地续跑。
//   - 熔断**不复用 Terminate()**。Terminate 会把所有非终态节点设成 skipped 并让
//     最终状态变成 terminated（语义是「用户主动终止」），与「等配额恢复」完全不同。
// ============================================================================

const (
	// defaultQuotaWait 是**服务端没告知重置时刻时**的兜底等待时长。
	//
	// 只在这里编造默认值：utilhttp.QuotaResetAt 刻意不给默认值，
	// 好让调用方能区分「服务端明确告知」和「我猜的」。
	defaultQuotaWait = 15 * time.Minute

	// maxQuotaWait 是单次熔断的最长等待。即便服务端说 5 小时后才恢复，也不会
	// 让工作流静默挂那么久——到点先试一次，不行再熔断一轮（有 maxQuotaBreaks 兜底）。
	maxQuotaWait = 90 * time.Minute

	// minQuotaWait 防止服务端给出的重置时刻近乎当下导致空转重试。
	minQuotaWait = 30 * time.Second

	// maxQuotaBreaks 是一条工作流允许的累计熔断次数上限。
	// 超过说明配额短期内不可能恢复，继续等下去没有意义，按失败收尾并保留已有产物。
	maxQuotaBreaks = 5
)

// isQuotaExhausted 判断错误是否为「上游额度耗尽」。
//
// 必须用 Loose 版本：错误链跨 节点重试 → SubAgent → LLM 流式重试 多层包装后
// 已退化为纯文本，errors.As 拿不到 *StreamHTTPError（同 review_error.go 的教训）。
func isQuotaExhausted(err error) bool {
	return utilhttp.IsQuotaExhaustedLoose(err)
}

// quotaWaitFor 计算本次熔断应等待多久，并返回恢复时刻。
//
// 优先采用服务端告知的重置时刻（Retry-After 头或 body 里的绝对时间），
// 拿不到时退回 defaultQuotaWait。结果被夹在 [minQuotaWait, maxQuotaWait] 之间。
func quotaWaitFor(err error, now time.Time) time.Time {
	wait := defaultQuotaWait
	if resetAt, ok := utilhttp.QuotaResetAtLoose(err); ok {
		wait = resetAt.Sub(now)
	}
	if wait < minQuotaWait {
		wait = minQuotaWait
	}
	if wait > maxQuotaWait {
		wait = maxQuotaWait
	}
	return now.Add(wait)
}

// quotaBroken 报告熔断器是否已跳闸。
func (s *Scheduler) quotaBroken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quotaTripped
}

// tripQuotaBreaker 跳闸熔断器；返回是否为本次运行中的第一次（用于只记一次日志/事件）。
func (s *Scheduler) tripQuotaBreaker(err error) bool {
	s.mu.Lock()
	if s.quotaTripped {
		s.mu.Unlock()
		return false
	}
	s.quotaTripped = true
	s.quotaErr = err.Error()
	s.quotaResumeAt = quotaWaitFor(err, time.Now())
	s.mu.Unlock()

	// close 必须在锁外，避免与持锁的读取方形成顺序依赖。
	// quotaTripped 已置位，重复 close 不可能发生。
	close(s.quotaBreakCh)
	return true
}

// handleQuotaBreak 处理某节点因上游配额耗尽而失败：
// 节点退回 pending（未真正执行，不算失败）、不级联跳过、跳闸熔断器。
func (s *Scheduler) handleQuotaBreak(ctx context.Context, node *DAGNode, err error) {
	first := s.tripQuotaBreaker(err)

	s.mu.Lock()
	node.Status = NodePending
	node.RetryCount = 0
	node.StartedAt = nil
	node.CompletedAt = nil
	node.Error = "上游配额耗尽，已暂停等待恢复"
	resumeAt := s.quotaResumeAt
	s.mu.Unlock()
	s.persist()

	if !first {
		return
	}

	s.logger.Warnw("quota exhausted, tripping workflow circuit breaker",
		"node_id", node.ID,
		"resume_at", resumeAt.Format(time.RFC3339),
		"error", err)

	s.emitNodeEvent(ctx, outbound.EventWorkflowNodeRetrying, map[string]any{
		"node_id":   node.ID,
		"reason":    "quota_exhausted",
		"resume_at": resumeAt.Format(time.RFC3339),
		"error":     err.Error(),
	})
}

// applyQuotaBreak 在 Run 收尾时把熔断信息写回工作流，供看门狗调度续跑。
// 返回 false 表示熔断次数已耗尽，应按失败收尾。
func (s *Scheduler) applyQuotaBreak() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.wf.QuotaBreaks++
	if s.wf.QuotaBreaks > maxQuotaBreaks {
		s.wf.QuotaResumeAt = nil
		s.wf.Error = fmt.Sprintf("上游配额持续不可用（已自动等待 %d 轮仍未恢复），已停止重试。已完成节点的产物均已保留，配额恢复后可手动重试。", maxQuotaBreaks)
		return false
	}
	resumeAt := s.quotaResumeAt
	s.wf.QuotaResumeAt = &resumeAt
	s.wf.Error = fmt.Sprintf("上游配额耗尽，将于 %s 自动继续执行（第 %d/%d 轮等待）",
		resumeAt.Format("15:04:05"), s.wf.QuotaBreaks, maxQuotaBreaks)
	return true
}
