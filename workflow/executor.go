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
	result, err := parseReviewResult(raw)
	if err != nil {
		logger.Warnw("failed to parse review result, treating as pass",
			"node_id", node.ID, "raw", strutil.Truncate(raw, 200), "error", err)
		// 解析失败时默认通过，避免无限循环
		return &ReviewResult{Passed: true, Feedback: ""}, nil
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
	sb.WriteString("\n\n---\n上一轮产物：\n")
	sb.WriteString(prevResult)
	sb.WriteString("\n\n---\n审查意见：\n")
	sb.WriteString(feedback)
	sb.WriteString("\n\n---\n请根据审查意见修改你的产出，确保满足原始要求。")
	return sb.String()
}

// buildReviewSystemPrompt 构建审查 SubAgent 的 system prompt。
func buildReviewSystemPrompt(customPrompt string) string {
	if customPrompt != "" {
		return customPrompt
	}
	return `你是一个严格的质量审查专家。你的职责是审查任务执行结果是否满足原始需求。

## 审查规则
1. 仔细对照原始需求，检查产物是否完整、准确、高质量
2. 如果产物完全满足要求，返回 {"passed": true}
3. 如果产物有任何不足，返回 {"passed": false, "feedback": "具体的修改意见"}，意见应具体且可执行

## 输出格式
必须返回 JSON：
{"passed": true}  或  {"passed": false, "feedback": "需要改进的地方..."}`
}

// buildReviewTask 构建审查任务输入。
func buildReviewTask(node *DAGNode, product string) string {
	return fmt.Sprintf("## 原始任务需求\n%s\n\n## 节点名称\n%s\n\n## 待审查的产物\n%s\n\n请审查以上产物是否满足原始任务需求。", node.Task, node.Name, product)
}

// parseReviewResult 解析 Review SubAgent 返回的 JSON。
func parseReviewResult(raw string) (*ReviewResult, error) {
	var result ReviewResult
	if err := strutil.ExtractJSON(raw, &result); err != nil {
		return nil, errs.Newf("cannot parse review result: %s", strutil.Truncate(raw, 100))
	}
	return &result, nil
}
