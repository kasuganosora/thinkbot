package workflow

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

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
		logger: logger.With("stage", "workflow_executor"),
	}
}

// ============================================================================
// workflow_id 的上下文透传
//
// Executor 是无状态的，其 logger 不持有 workflow_id；而 executor 的日志
// （executing node / reviewing node / review completed）此前只有 node_id，
// **无法关联到具体工作流**——2026-08-04 排查时只能靠 scheduler 层日志间接推断
// 节点归属，绕了很大弯路。故由 Scheduler 把 workflow_id 放进 ctx，
// Executor 取出后注入日志。
// ============================================================================

type workflowIDCtxKey struct{}

// withWorkflowID 把 workflow_id 存入 ctx，供下游 Executor 的日志使用。
func withWorkflowID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, workflowIDCtxKey{}, id)
}

// workflowIDFromContext 取出 workflow_id，不存在则返回空串。
func workflowIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(workflowIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// nodeLogger 构造带 workflow_id / node_id 的日志器。
func (e *Executor) nodeLogger(ctx context.Context, node *DAGNode) *zap.SugaredLogger {
	logger := traceid.WithLoggerFrom(ctx, e.logger)
	if wfID := workflowIDFromContext(ctx); wfID != "" {
		logger = logger.With("workflow_id", wfID)
	}
	return logger.With("node_id", node.ID)
}

// nodeStuckTimeout 节点执行/审查的「卡死」判定阈值。
//
// 节点任务是重活（读几十个文件 + 跑构建 + 多轮修改），天然耗时长但持续有输出。
// 故用 DelegateStream 的看门狗语义：**持续吐 token 就不杀，只杀真卡死的**。
// 旧代码用 Delegate（context.WithTimeout 一刀切），叠加 workflow_service 实例
// 未拿到 10min 配置的问题，线上出现 30 次 elapsed=120.000s 的硬超时。
//
// 硬上限由 subagent 内部按 stuck × delegateHardTimeoutFactor(10) 派生 = 30min，
// 是「总运行时间」的绝对兜底（见 subagent/manager.go:delegateHardTimeoutFactor）。
// 注意这是 30min 量级、远大于 bot 侧 10min 委托超时——前者是失控流兜底，
// 后者是单次委托的软超时，二者职责不同，不要混为一谈。
// 注意：**WithCallTimeout 对 DelegateStream 无效**，必须用 WithStuckTimeout。
const nodeStuckTimeout = 3 * time.Minute

// Execute 通过 SubAgent 执行节点任务，返回产物文本。
// 自动注入已完成的依赖节点的产物作为上下文，提升 SubAgent 输出质量。
func (e *Executor) Execute(ctx context.Context, node *DAGNode) (string, error) {
	ctx, span := e.tracer.Start(ctx, "workflow.node.execute",
		trace.WithAttributes(
			attribute.String("node.id", node.ID),
			attribute.String("node.name", node.Name),
		))
	defer span.End()

	logger := e.nodeLogger(ctx, node)
	logger.Debugw("executing node", "name", node.Name)

	result, err := e.saMgr.DelegateStream(ctx, node.SystemPrompt, node.Task,
		subagent.WithStuckTimeout(nodeStuckTimeout))
	if err != nil {
		span.RecordError(err)
		return "", errs.Wrapf(err, "node %q execution failed", node.ID)
	}

	span.SetAttributes(attribute.Int("result.length", len(result)))
	logger.Debugw("node executed", "result_len", len(result))
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

	logger := e.nodeLogger(ctx, node)
	task := buildIterationTask(node.Task, prevResult, feedback)

	logger.Debugw("re-executing node with feedback", "feedback_len", len(feedback))

	result, err := e.saMgr.DelegateStream(ctx, node.SystemPrompt, task,
		subagent.WithStuckTimeout(nodeStuckTimeout))
	if err != nil {
		span.RecordError(err)
		return "", errs.Wrapf(err, "node %q re-execution failed", node.ID)
	}

	span.SetAttributes(attribute.Int("result.length", len(result)))
	logger.Debugw("node re-executed", "result_len", len(result))
	return result, nil
}

// ReviewVerdictSource 标记审查判定的来源，用于区分「模型明确给出结论」与
// 「我们从文本里猜出结论」——两者的可信度差一个量级，DB / UI 必须能区分。
type ReviewVerdictSource string

const (
	// ReviewSourceJSON 模型返回了含 passed 字段的合法 JSON（最可信）。
	ReviewSourceJSON ReviewVerdictSource = "json"
	// ReviewSourceVerdictLine 从「判定/结论/结果: PASS|FAIL」这类显式判定行提取（可信）。
	ReviewSourceVerdictLine ReviewVerdictSource = "verdict_line"
	// ReviewSourceHeuristic 词频启发式兜底（不可信，仅防止静默放行）。
	ReviewSourceHeuristic ReviewVerdictSource = "heuristic"
)

// ReviewResult 是 Review SubAgent 的返回结果。
type ReviewResult struct {
	Passed   bool   `json:"passed"`
	Feedback string `json:"feedback,omitempty"`

	// Source 判定来源。零值（空串）等价于 ReviewSourceJSON，保持旧测试与旧数据兼容。
	Source ReviewVerdictSource `json:"source,omitempty"`
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

	logger := e.nodeLogger(ctx, node)
	reviewPrompt := buildReviewSystemPrompt(node.ReviewPrompt)
	reviewTask := buildReviewTask(node, product)

	logger.Debugw("reviewing node", "product_len", len(product))

	raw, err := e.saMgr.DelegateStream(ctx, reviewPrompt, reviewTask,
		subagent.WithStuckTimeout(nodeStuckTimeout))
	if err != nil {
		span.RecordError(err)
		return nil, errs.Wrapf(err, "node %q review failed", node.ID)
	}

	// 解析 Review 结果（期望 LLM 在最后一行返回 JSON）
	result, usedHeuristic := parseReviewResult(raw)

	// 判定降级到 heuristic 意味着「我们没拿到模型的结论」，而不是「结论是失败」。
	// heuristic 设计上保守判FAIL，若放任它，节点会被无谓地重跑到迭代耗尽——
	// 线上实测90%（19/21）的 review 都掉进这条路径，工作流几乎无法收敛。
	//
	// 根因：Review 走 DelegateStream 且**注入了工具**，因此跑的是多步编排循环。
	// 模型把输出预算花在「我来核实一下…」的中途叙述上，最后一步常以工具调用收尾，
	// 压根没产出约定的 JSON 判定行。
	//
	// 对策：**追加一次无工具的收尾调用**，只要判定，不要过程。
	// 这是「拿不到结论 → 去把结论要回来」，而非「猜一个结论」。
	if result.Source == ReviewSourceHeuristic {
		logger.Warnw("review verdict missing, asking for verdict-only follow-up",
			"heuristic_guess", result.Passed, "raw_tail", reviewRawTail(raw))
		if r2, err2 := e.reviewVerdictOnly(ctx, node, raw); err2 == nil {
			result, usedHeuristic = r2, r2.Source != ReviewSourceJSON
			logger.Infow("verdict-only follow-up succeeded",
				"passed", result.Passed, "source", string(result.Source))
		} else {
			// 收尾调用也失败：保持 heuristic 结论（保守 FAIL），但必须留下原因，
			// 否则又变成静默降级。
			logger.Warnw("verdict-only follow-up failed, keeping heuristic verdict",
				"error", err2, "passed", result.Passed)
		}
	}

	if usedHeuristic {
		// 判定不是模型明确给出的 JSON，而是从文本推断的。
		// source 区分 verdict_line（可信）与 heuristic（不可信），便于评估判定质量。
		// 注意日志取的是**末尾**片段：约定的判定 JSON 在结尾，截开头等于把
		// 最关键的证据丢掉（2026-08-06 排查时因此误判了根因，别再改回head）。
		logger.Warnw("review verdict not from JSON, inferred from text",
			"passed", result.Passed,
			"source", string(result.Source), "raw_tail", reviewRawTail(raw))
	}

	span.SetAttributes(
		attribute.Bool("review.passed", result.Passed),
		attribute.String("review.source", string(result.Source)),
	)
	logger.Debugw("review completed",
		"passed", result.Passed, "source", string(result.Source))
	return result, nil
}

// ============================================================================
// 内部辅助函数
// ============================================================================

// verdictOnlyStuckTimeout 是「收尾判定」调用的卡死阈值。
// 这一步不带工具、只让模型基于已有分析吐一行 JSON，属于轻量纯 LLM 调用，
// 无需与节点执行同量级的3min 预算。
const verdictOnlyStuckTimeout = 90 * time.Second

// reviewVerdictOnlyMaxAnalysis 是回灌给收尾调用的审查分析文本上限。
// 取末尾而非开头：结论性内容在后面。
const reviewVerdictOnlyMaxAnalysis = 6000

// reviewVerdictOnly 在主审查调用没能给出可信判定时，追加一次**无工具**的收尾调用，
// 只索取判定 JSON。
//
// 为什么必须无工具（`WithSkipTools`）：主审查之所以拿不到判定，正是因为它带工具走了
// 多步编排循环、把输出预算花在中途叙述上、最后一步以工具调用收尾。收尾调用若再带工具，
// 会重复同一个失败模式；纯 LLM 单步调用才能保证「这一轮的输出就是最终答案」。
//
// 语义定位：这是「拿不到结论 → 去把结论要回来」，**不是**「猜一个结论」。
// 与 heuristic 兜底的区别在于判定仍由模型给出，可信度回到 ReviewSourceJSON 级别。
func (e *Executor) reviewVerdictOnly(ctx context.Context, node *DAGNode, priorAnalysis string) (*ReviewResult, error) {
	analysis := strings.TrimSpace(priorAnalysis)
	if analysis == "" {
		return nil, errs.Newf("node %q: no prior review analysis to derive a verdict from", node.ID)
	}
	// 取末尾片段：审查结论在后面，截开头会把最有价值的部分丢掉。
	if len(analysis) > reviewVerdictOnlyMaxAnalysis {
		analysis = analysis[len(analysis)-reviewVerdictOnlyMaxAnalysis:]
	}

	sysPrompt := `You convert a code-review analysis into a single machine-readable verdict.

You are given a reviewer's analysis of a task output. The reviewer failed to emit the
required verdict. Your ONLY job is to read that analysis and decide the verdict.

Rules:
- Fail (passed=false) ONLY if the analysis reports a blocking defect: compile/syntax/type
  errors, logic bugs, race conditions, resource leaks, security vulnerabilities, an
  explicitly stated requirement not met, or output that is empty / only a plan.
- Do NOT fail for style, naming, optional refactors, performance suggestions, or minor
  documentation gaps. Those are non-blocking.
- If the analysis shows the build and tests pass and no blocking defect is described,
  the verdict is passed=true.
- Judge ONLY from the analysis given. Do not call tools. Do not re-review the code.

Reply with EXACTLY ONE line of JSON and nothing else:
{"passed": true, "notes": "非阻断观察（可为空）"}
or
{"passed": false, "feedback": "具体的、可操作的修改意见", "notes": ""}
Write "feedback" and "notes" in Chinese (中文); keep the JSON keys exactly as shown.`

	task := fmt.Sprintf("## Reviewed Task\n%s\n\n## Reviewer Analysis (tail)\n%s\n\n"+
		"Output the verdict JSON on a single line now.", node.Name, analysis)

	raw, err := e.saMgr.DelegateStream(ctx, sysPrompt, task,
		subagent.WithSkipTools(),
		subagent.WithStuckTimeout(verdictOnlyStuckTimeout))
	if err != nil {
		return nil, errs.Wrapf(err, "node %q verdict-only review failed", node.ID)
	}

	// 收尾调用只认真正的 JSON 与显式判定行。若它也没给出，就不要再退化到
	// heuristic——那等于用两次 LLM 调用换一个同样不可信的猜测。
	if r, ok := reviewResultFromJSON(lastNonEmptyLine(raw)); ok {
		return r, nil
	}
	if r, ok := reviewResultFromJSON(raw); ok {
		return r, nil
	}
	if passed, ok := verdictFromLine(raw); ok {
		feedback := ""
		if !passed {
			feedback = strutil.Truncate(raw, 500)
		}
		return &ReviewResult{Passed: passed, Feedback: feedback, Source: ReviewSourceVerdictLine}, nil
	}
	return nil, errs.Newf("node %q: verdict-only review returned no parseable verdict (tail=%s)",
		node.ID, reviewRawTail(raw))
}

// reviewRawTail 返回审查原文的**末尾**片段用于日志。
//
// 必须取末尾：约定的判定 JSON 在最后一行。历史上这里用的是
// `strutil.Truncate(raw, 200)`（取开头），导致日志里全是「我来核实一下…」的开场白，
// 判定为何缺失完全不可诊断——2026-08-06 排查时因此误判了根因。别再改回 head。
func reviewRawTail(raw string) string {
	const maxTail = 300
	s := strings.TrimSpace(raw)
	if len(s) <= maxTail {
		return s
	}
	return "..." + s[len(s)-maxTail:]
}

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
	return `You are a strict but pragmatic quality reviewer. You decide whether a task's output is good enough to ship.

## Blocking vs non-blocking

Distinguish two classes of findings. This distinction is the core of your job.

**Blocking defects** — these MUST fail the review:
- Compile / syntax / type errors
- Logic bugs, race conditions, resource leaks, security vulnerabilities
- An explicit, stated requirement of the original task is not met
- The output is empty, is only a plan, or does not actually perform the requested work

**Non-blocking observations** — these MUST NOT fail the review:
- Style, naming, formatting preferences
- Optional refactors or performance tuning that is not required by the task
- Suggestions for future improvement
- Minor documentation or comment gaps

IMPORTANT: Do NOT fail a review just because the output is imperfect. Real code always
has room for improvement. Fail it ONLY when a blocking defect exists.
Report non-blocking findings in "notes" — they do not affect the verdict.

## Output Format

You MUST end your reply with a single JSON object on the LAST line, and nothing after it:
{"passed": true, "notes": "非阻断观察（可为空）"}
or
{"passed": false, "feedback": "具体的、可操作的修改意见", "notes": "非阻断观察（可为空）"}

CRITICAL — the verdict JSON is the only part of your reply that is machine-read. If you
omit it, your entire review is discarded and the task is needlessly re-run. Therefore:
- Budget your tool use. Verify the few claims that actually matter, then stop and decide.
- NEVER end your reply with a tool call. The verdict JSON must be the final thing you emit.
- Even if your verification was incomplete, still emit a verdict based on what you found.

You MAY write your analysis before that final JSON line. The JSON line itself must be
valid JSON with no markdown code fence around it.
Write "feedback" and "notes" values in Chinese (中文); keep the JSON keys exactly as shown.`
}

// buildReviewTask 构建审查任务输入。
func buildReviewTask(node *DAGNode, product string) string {
	return fmt.Sprintf("## Original Task Requirement\n%s\n\n## Node Name\n%s\n\n## Output Under Review\n%s\n\n"+
		"Review the output above. Fail it ONLY if a blocking defect exists (see your review rules); "+
		"report non-blocking findings in \"notes\" instead.\n"+
		"Remember: your reply MUST end with the verdict JSON on its own last line.",
		node.Task, node.Name, product)
}

// verdictLineRe 匹配模型审查报告里的显式判定行，例如：
//
//	## 审查结论：**FAIL**
//	### 审查结果：✅ **PASS**
//	## 判定结果：PASS（附 3 项非阻断观察）
//	Verdict: PASS
//
// 实测对 53 条线上审查样本覆盖 50条，是比词频计数可靠得多的判定来源。
// 中间允许出现 markdown 强调符、空格与emoji（`[^A-Za-z\p{Han}]*` 跳过非字母字符）。
var verdictLineRe = regexp.MustCompile(
	`(?i)(判定结果|审查结论|审查结果|最终结论|最终判定|判定|结论|结果|verdict|result|conclusion)` +
		`\s*[:：]\s*[^A-Za-z\p{Han}]*(PASS|FAIL|通过|不通过|未通过)`)

// parseReviewResult 解析 Review SubAgent 返回的审查结论。
//
// 三级判定，可信度依次递减：
//  1. JSON 且**确实含passed 字段** → ReviewSourceJSON
//  2. 显式判定行（正则）→ ReviewSourceVerdictLine
//  3. 词频启发式兜底 → ReviewSourceHeuristic
//
// 第二返回值 usedHeuristic 标记「判定不是模型明确给出的」（即 2 和 3），用于日志告警。
//
// 关键坑（2026-08-04 修复）：不能只看ExtractJSON 是否返回 nil error。
// ExtractJSON 会从**第一个 `{`** 起做括号配平，而审查报告常含代码片段，
// 第一个 `{` 往往是代码的花括号；又因encoding/json 对未知字段宽容，
// `{"timeout":5000}` 会「解析成功」并得到 Passed=false（bool 零值），
// 且旧代码认为这是可信 JSON、连WARN 都不打 —— 静默把产物判为不通过。
// 故必须先用 map 探测 passed 字段是否真实存在。
func parseReviewResult(raw string) (*ReviewResult, bool) {
	// 1) JSON：必须真的带 passed 字段才认。
	// 先从**末尾**行找（system prompt 要求把 JSON 放最后一行），再退回全文扫描——
	// 顺序很关键：审查报告正文常含代码片段，从前往后扫会先抓到代码的花括号。
	if r, ok := reviewResultFromJSON(lastNonEmptyLine(raw)); ok {
		return r, false
	}
	if r, ok := reviewResultFromJSON(raw); ok {
		return r, false
	}

	// 2) 显式判定行
	if passed, ok := verdictFromLine(raw); ok {
		feedback := ""
		if !passed {
			feedback = strutil.Truncate(raw, 500)
		}
		return &ReviewResult{Passed: passed, Feedback: feedback, Source: ReviewSourceVerdictLine}, true
	}

	// 3) 词频启发式兜底
	passed, feedback := heuristicReviewVerdict(raw)
	return &ReviewResult{Passed: passed, Feedback: feedback, Source: ReviewSourceHeuristic}, true
}

// verdictBoolFromProbe 从解析出的 JSON 对象里提取判定布尔值。
//
// 为什么要认多个字段名（2026-08-06 线上实测）：prompt 明确要求 `{"passed": ...}`，
// 但模型实际大量输出别名 —— 40 条「判定缺失」样本里 `verdict` 出现 23 次、`pass` 4 次，
// 而旧实现只认 `passed`，于是这些**明明给了判定**的回复统统落进 heuristic 兜底，
// 被保守判FAIL、触发无谓重跑。
//
// 同时接受两种值形态：
//   - bool：`{"passed": true}` / `{"pass": false}`
//   - string：`{"verdict": "pass"}` / `{"verdict": "FAIL"}`（模型偏爱这种）
//
// 第二返回值为 false 表示「这个对象里没有可识别的判定字段」——**必须保留这个语义**，
// 否则代码片段里的 `{"timeout":5000}` 会被当成判定（bool 零值 = false），
// 静默把产物判为不通过（2026-08-04 踩过）。
func verdictBoolFromProbe(probe map[string]any) (bool, bool) {
	// 顺序即优先级：先认 prompt 约定的 passed，再认实测常见别名。
	for _, key := range []string{"passed", "pass", "verdict", "result", "status"} {
		v, exists := probe[key]
		if !exists {
			continue
		}
		switch val := v.(type) {
		case bool:
			return val, true
		case string:
			if passed, ok := verdictFromWord(val); ok {
				return passed, true
			}
		}
	}
	return false, false
}

// verdictFromWord 解析判定词（"pass"/"fail"/"通过"/"不通过" 等）。
// 无法识别时第二返回值为 false —— 不认识的词绝不能当成判定，
// 否则又变成「猜一个结论」。
func verdictFromWord(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pass", "passed", "true", "ok", "success", "通过", "已通过":
		return true, true
	case "fail", "failed", "false", "不通过", "未通过", "失败":
		return false, true
	}
	return false, false
}

// reviewResultFromJSON 尝试把 s 解析成审查结论。
// 只有解析出的对象**确实含可识别的判定字段**才算成功——这是防「代码片段被
// 当成判定」的关键校验，不可省略。字段名/值形态的兼容见 verdictBoolFromProbe。
func reviewResultFromJSON(s string) (*ReviewResult, bool) {
	if strings.TrimSpace(s) == "" {
		return nil, false
	}
	var probe map[string]any
	if err := strutil.ExtractJSON(s, &probe); err != nil {
		return nil, false
	}
	passed, ok := verdictBoolFromProbe(probe)
	if !ok {
		return nil, false
	}
	// feedback 也认别名：模型常用 reason 说明失败原因。
	feedback, _ := probe["feedback"].(string)
	if feedback == "" {
		feedback, _ = probe["reason"].(string)
	}
	return &ReviewResult{Passed: passed, Feedback: feedback, Source: ReviewSourceJSON}, true
}

// lastNonEmptyLine 返回最后一个非空行（已去除首尾空白与常见 markdown 代码围栏）。
func lastNonEmptyLine(raw string) string {
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || line == "```" || strings.HasPrefix(line, "```") {
			continue
		}
		return line
	}
	return ""
}

// verdictFromLine 从显式判定行提取结论。第二返回值为 false 表示没找到判定行。
//
// 只取**第一个**匹配：审查报告正文里常有「第 3 项：FAIL」这类分项结论，
// 而开头的总判定才是最终结论。
func verdictFromLine(raw string) (bool, bool) {
	m := verdictLineRe.FindStringSubmatch(raw)
	if m == nil {
		return false, false
	}
	switch strings.ToUpper(m[2]) {
	case "PASS", "通过":
		return true, true
	case "FAIL", "不通过", "未通过":
		return false, true
	}
	return false, false
}

// heuristicReviewVerdict 从纯文本审查结论中推断通过/不通过（**最后的兜底**）。
//
// 仅在「JSON 无 passed 字段」且「无显式判定行」时调用，实测线上样本约 3/53 命中此路径。
//
// 设计原则：宁可误判为「不通过」（触发重跑/收敛），也绝不静默放行未真正审查的产物。
// 判定优先级：
//  1. 不通过信号多于通过信号 → 不通过；
//  2. 仅有明确通过信号 → 通过；
//  3. 模糊或无明确信号 → 保守按不通过处理。
//
// 调用方（reviewLoop / 目标模式闭环）已用节点 MaxIterations 与全局 GoalMaxIterations
// 兜底，故不会因「保守判 fail」而无限循环。
//
// 词表纪律（2026-08-04 修复，别再踩）：
//   - **pass / fail 必须对称**。旧版 failSignals 有裸词 "fail"，passSignals 却没有裸词
//     "pass"，而模型标准输出是「审查结论：PASS ✅」→ PASS 侧一个都不命中、FAIL 侧必中，
//     两边同时为 0 就落 default 判 FAIL。线上 6 条模型判 PASS 的样本被误杀 5 条。
//   - **含否定前缀的短语必须先规范化**，否则「不通过」里的「通过」会同时点亮 pass 信号。
//   - 裸词有子串风险（pass→password/bypass，fail→failure-free），故用 normalizeVerdictText
//     统一替换成不会互相包含的哨兵 token 再匹配。
func heuristicReviewVerdict(raw string) (bool, string) {
	text := normalizeVerdictText(raw)

	passSignals := []string{
		"__PASSTOKEN__", "all checks passed", "审查通过", "已通过", "已验收",
		"满足要求", "符合要求", "符合需求", "验收通过", "全部通过", "通过审查",
		"没有发现问题", "无遗留问题", "无遗留", "acceptable", "looks good",
		"passed review",
	}
	failSignals := []string{
		"__FAILTOKEN__", "不合格", "未满足", "未达标", "不达标",
		"未修复", "缺少", "缺失", "无法执行", "不合规", "未覆盖", "未完全",
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

// normalizeVerdictText 把易互相包含的判定词统一替换成哨兵 token，消除子串误命中。
//
// 处理顺序至关重要：**先长后短、先否定后肯定**。
// 例如必须先把「不通过」替换掉，否则后续匹配「通过」会把它也算成 pass 信号；
// 同理先处理 "not passed" 再处理 "pass"，先处理 "failure-free" 再处理 "fail"。
func normalizeVerdictText(raw string) string {
	text := strings.ToLower(raw)

	// 顺序敏感：每组按「更长/更具否定性」在前
	replacements := []struct{ from, to string }{
		// JSON 风格字段
		{`"passed":false`, "__FAILTOKEN__"},
		{`"passed": false`, "__FAILTOKEN__"},
		{"passed:false", "__FAILTOKEN__"},
		{`"passed":true`, "__PASSTOKEN__"},
		{`"passed": true`, "__PASSTOKEN__"},
		{"passed:true", "__PASSTOKEN__"},

		// 英文否定/干扰词优先，避免裸词误命中
		{"failure-free", "__PASSTOKEN__"},
		{"no failures", "__PASSTOKEN__"},
		{"without failure", "__PASSTOKEN__"},
		{"not passed", "__FAILTOKEN__"},
		{"didn't pass", "__FAILTOKEN__"},
		{"does not pass", "__FAILTOKEN__"},
		{"no issues", "__PASSTOKEN__"},
		{"bypass", "__NEUTRAL__"},
		{"password", "__NEUTRAL__"},
		{"failed", "__FAILTOKEN__"},
		{"fail", "__FAILTOKEN__"},
		{"passes", "__PASSTOKEN__"},
		{"passed", "__PASSTOKEN__"},
		{"pass", "__PASSTOKEN__"},

		// 中文：否定形式必须先于「通过」「问题」被消化
		{"不通过", "__FAILTOKEN__"},
		{"未通过", "__FAILTOKEN__"},
		{"没有问题", "__PASSTOKEN__"},
		{"不存在问题", "__PASSTOKEN__"},
		{"无任何问题", "__PASSTOKEN__"},
		{"未发现问题", "__PASSTOKEN__"},
		{"无问题", "__PASSTOKEN__"},
		{"存在问题", "__FAILTOKEN__"},
		{"仍有问题", "__FAILTOKEN__"},
		{"有问题", "__FAILTOKEN__"},
	}
	for _, r := range replacements {
		text = strings.ReplaceAll(text, r.from, r.to)
	}
	return text
}
