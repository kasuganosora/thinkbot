package workflow

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/subagent"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Executor — 节点执行器
//
// 职责：
//   - Execute: 通过 SubAgent 执行节点任务
//   - ExecuteWithFeedback: 带上一轮产物和审查意见重新执行
//   - Review: 通过独立的 Review SubAgent 检查节点产物
//
// Executor 本身是无状态函数集合，不维护节点运行状态。
// 状态由 Scheduler 负责更新。
// ============================================================================

// Executor 执行工作流节点。
type Executor struct {
	saMgr  *subagent.SubAgentManager
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// NewExecutor 创建执行器。
func NewExecutor(saMgr *subagent.SubAgentManager, tp trace.TracerProvider, logger *zap.SugaredLogger) *Executor {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	return &Executor{
		saMgr:  saMgr,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/workflow/executor"),
		logger: logger.With("component", "workflow_executor"),
	}
}

// Execute 通过 SubAgent 执行节点任务，返回产物文本。
// 自动注入已完成的依赖节点的产物作为上下文，提升 SubAgent 输出质量。
func (e *Executor) Execute(ctx context.Context, node *DAGNode) (string, error) {
	ctx, span := e.tracer.Start(ctx, "workflow.node.execute",
		trace.WithAttributes(
			attribute.String("node.id", node.ID),
			attribute.String("node.name", node.Name),
		))
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, e.logger)
	logger.Debugw("executing node", "node_id", node.ID, "name", node.Name)

	result, err := e.saMgr.Delegate(ctx, node.SystemPrompt, node.Task)
	if err != nil {
		span.RecordError(err)
		return "", errs.Wrapf(err, "node %q execution failed", node.ID)
	}

	span.SetAttributes(attribute.Int("result.length", len(result)))
	logger.Debugw("node executed", "node_id", node.ID, "result_len", len(result))
	return result, nil
}

// ExecuteWithFeedback 带上一轮产物和审查意见重新执行节点任务。
// 迭代模式下，SubAgent 输入 = 原始任务 + 上一轮产物 + 审查意见。
func (e *Executor) ExecuteWithFeedback(ctx context.Context, node *DAGNode, prevResult, feedback string) (string, error) {
	ctx, span := e.tracer.Start(ctx, "workflow.node.re_execute",
		trace.WithAttributes(
			attribute.String("node.id", node.ID),
			attribute.Int("prev_result.length", len(prevResult)),
			attribute.Int("feedback.length", len(feedback)),
		))
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, e.logger)
	task := buildIterationTask(node.Task, prevResult, feedback)

	logger.Debugw("re-executing node with feedback",
		"node_id", node.ID, "feedback_len", len(feedback))

	result, err := e.saMgr.Delegate(ctx, node.SystemPrompt, task)
	if err != nil {
		span.RecordError(err)
		return "", errs.Wrapf(err, "node %q re-execution failed", node.ID)
	}

	span.SetAttributes(attribute.Int("result.length", len(result)))
	logger.Debugw("node re-executed", "node_id", node.ID, "result_len", len(result))
	return result, nil
}

// ReviewResult 是 Review SubAgent 的返回结果。
type ReviewResult struct {
	Passed   bool   `json:"passed"`
	Feedback string `json:"feedback,omitempty"`
}

// Review 通过独立的 Review SubAgent 检查节点产物是否符合需求。
//
// Review SubAgent 的 system prompt 定义了审查专家角色。
// 返回 passed=true 表示产物合格，false 表示不合格（附带修改意见）。
func (e *Executor) Review(ctx context.Context, node *DAGNode, product string) (*ReviewResult, error) {
	ctx, span := e.tracer.Start(ctx, "workflow.node.review",
		trace.WithAttributes(
			attribute.String("node.id", node.ID),
			attribute.Int("product.length", len(product)),
		))
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, e.logger)
	reviewPrompt := buildReviewSystemPrompt(node.ReviewPrompt)
	reviewTask := buildReviewTask(node, product)

	logger.Debugw("reviewing node", "node_id", node.ID)

	raw, err := e.saMgr.Delegate(ctx, reviewPrompt, reviewTask)
	if err != nil {
		span.RecordError(err)
		return nil, errs.Wrapf(err, "node %q review failed", node.ID)
	}

	// 解析 Review 结果（期望 LLM 返回 JSON）
	result, usedHeuristic := parseReviewResult(raw)
	if usedHeuristic {
		// 解析失败但已退化为文本启发式判定：宁可误判为不通过（触发重跑），
		// 也绝不静默放行未真正审查的产物。轮次上限（节点 MaxIterations / 目标模式
		// GoalMaxIterations）会防止无限循环。
		logger.Warnw("review result not valid JSON, used heuristic verdict",
			"node_id", node.ID, "passed", result.Passed, "raw", strutil.Truncate(raw, 200))
	}

	span.SetAttributes(attribute.Bool("review.passed", result.Passed))
	logger.Debugw("review completed", "node_id", node.ID, "passed", result.Passed)
	return result, nil
}

// ============================================================================
// 内部辅助函数
// ============================================================================

// buildIterationTask 构建带反馈的迭代执行任务。
func buildIterationTask(originalTask, prevResult, feedback string) string {
	var sb strings.Builder
	sb.WriteString(originalTask)
	sb.WriteString("\n\n---\nPrevious output:\n")
	sb.WriteString(prevResult)
	sb.WriteString("\n\n---\nReview feedback:\n")
	sb.WriteString(feedback)
	sb.WriteString("\n\n---\nRevise your output according to the review feedback above. You MUST address every point raised and satisfy the original requirement. Write your revised output in Chinese (中文).")
	return sb.String()
}

// buildReviewSystemPrompt 构建审查 SubAgent 的 system prompt。
func buildReviewSystemPrompt(customPrompt string) string {
	if customPrompt != "" {
		return customPrompt
	}
	return `You are a strict quality reviewer, a verification agent that decides whether a task's output satisfies the original requirement.

## Review Rules

1. Compare the output against the original requirement carefully and check that it is complete, accurate, and high quality.
2. If the output fully satisfies the requirement, return {"passed": true}.
3. If the output has ANY shortcoming, return {"passed": false, "feedback": "具体的修改意见"}. The feedback MUST be specific and actionable.

## Output Format

You MUST return JSON, and nothing else:
{"passed": true}  or  {"passed": false, "feedback": "需要改进的地方..."}

IMPORTANT: output JSON only — no preamble, no explanation, no markdown code fence.
Write the "feedback" value in Chinese (中文); preserve the JSON keys exactly as shown.`
}

// buildReviewTask 构建审查任务输入。
func buildReviewTask(node *DAGNode, product string) string {
	return fmt.Sprintf("## Original Task Requirement\n%s\n\n## Node Name\n%s\n\n## Output Under Review\n%s\n\nReview the output above and decide whether it satisfies the original task requirement.", node.Task, node.Name, product)
}

// parseReviewResult 解析 Review SubAgent 返回的 JSON。
// 若 JSON 解析失败，退化为文本启发式判定（见 heuristicReviewVerdict）。
// 第二返回值 usedHeuristic 标记本次是否走了启发式兜底（用于日志告警）。
func parseReviewResult(raw string) (*ReviewResult, bool) {
	var result ReviewResult
	if err := strutil.ExtractJSON(raw, &result); err == nil {
		return &result, false
	}
	passed, feedback := heuristicReviewVerdict(raw)
	return &ReviewResult{Passed: passed, Feedback: feedback}, true
}

// heuristicReviewVerdict 从纯文本审查结论中推断通过/不通过。
//
// 设计原则：宁可误判为「不通过」（触发重跑/收敛），也绝不静默放行未真正审查的产物。
// 判定优先级：
//  1. 不通过信号多于通过信号 → 不通过；
//  2. 仅有明确通过信号 → 通过；
//  3. 模糊或无明确信号 → 保守按不通过处理。
//
// 调用方（reviewLoop / 目标模式闭环）已用节点 MaxIterations 与全局 GoalMaxIterations
// 兜底，故不会因「保守判 fail」而无限循环。
func heuristicReviewVerdict(raw string) (bool, string) {
	text := strings.ToLower(raw)

	passSignals := []string{
		"\"passed\":true", "\"passed\": true", "passed:true",
		"all checks passed", "审查通过", "已通过", "已验收", "满足要求", "符合要求",
		"符合需求", "验收通过", "全部通过", "通过审查", "无问题", "没有发现问题",
		"未发现", "无遗留问题", "无遗留", "acceptable", "looks good", "no issues",
		"passed review",
	}
	failSignals := []string{
		"\"passed\":false", "\"passed\": false", "passed:false",
		"fail", "failed", "不通过", "未通过", "不合格", "未满足", "未达标", "不达标",
		"有问题", "未修复", "缺少", "缺失", "无法执行", "不合规", "未覆盖", "未完全",
		"未成功", "未达成", "不符合", "需要改进", "建议修改", "不全面", "有遗漏",
		"未提供", "未能满足", "cannot", "not met", "not satisfied", "incomplete",
	}

	failHits, passHits := 0, 0
	for _, s := range failSignals {
		if strings.Contains(text, s) {
			failHits++
		}
	}
	for _, s := range passSignals {
		if strings.Contains(text, s) {
			passHits++
		}
	}

	switch {
	case failHits > passHits:
		return false, strutil.Truncate(raw, 500)
	case passHits > 0 && failHits == 0:
		return true, ""
	default:
		// 模糊 / 无明确信号：保守按不通过处理，避免放行未审查产物
		return false, strutil.Truncate(raw, 500)
	}
}
