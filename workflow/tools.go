package workflow

import (
	"fmt"
	"strconv"
	"time"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Workflow 工具定义
//
// 暴露 3 个 LLM 工具给主 Agent：
//   - task:          提交需求并**阻塞**到工作流进入终态（completed/failed/terminated）才返回
//   - task_detail:   查询节点列表（flat / tree）
//   - task_control:  控制操作（重试节点 / 终止工作流）
//
// task 对工作流是「阻塞式」工具：提交后服务端代为等待直到终态再返回最终状态，
// agent 无需再轮询。旧的 task_status(wait:true) 轮询机制已移除。
//
// ── 工具命名策略 ────────────────────────────────────────────────────
// 主工具命名为 "task"，与主流 LLM 预训练中的 agentic 工具名对齐
// （如 Claude 的 Task 工具、LangChain 的 TaskTool），降低 LLM 适配成本。
//
// ── 反嵌套保证 ──────────────────────────────────────────────────────
// 所有 workflow 工具的 Scopes 均为 ["private", "group"]，
// 这两个 scope 在 appliesTo() 中都带 !sctx.IsSubagent 条件，
// 因此 SubAgent 上下文下 workflow 工具不可见，无法递归创建工作流。
//
// 额外保障：workflow 引擎内部使用独立的 SubAgentManager（见 wire.go），
// 该管理器创建的 SubAgent 通过 Delegate 一次性调用执行，
// 不经过主 Agent 的 ToolManager，无法访问任何工具。
// ────────────────────────────────────────────────────────────────────

// workflowToolPromptSection 是工作流工具的提示词段落。
var workflowToolPromptSection = &tools.ToolPromptSection{
	Name:  "workflow_tools",
	Order: 310,
	Content: `# Task Engine

You can use the ` + "`task`" + ` tool to handle complex, multi-step work. The task engine decomposes a requirement into a DAG of sub-tasks, runs them in parallel or in sequence, and supports result review and quality iteration.

## Workflow

1. **Submit**: call ` + "`task`" + ` with the requirement. This call **blocks** — the server runs the workflow to a terminal state (completed / failed / terminated) and returns the final status and progress. No further waiting call is needed.
2. **Inspect (optional)**: call ` + "`task_detail`" + ` to see each sub-task's execution status and result (flat and tree views are supported).
3. **Control**: when a node fails, use ` + "`task_control`" + ` to retry it, or to terminate the whole task.

## task is a blocking tool — never poll, never sleep

Submitting with ` + "`task`" + ` already waits on your behalf until the task is done; the call returns the final result. **There is no separate status-polling tool**, so do not try to call one, and never use ` + "`sleep`" + ` to wait.

Rule of thumb: **want the task result → one ` + "`task`" + ` call → continue once it returns.**

<example>
Correct:
task(...)   → blocks until completed/failed/terminated, then returns the final status
(use task_detail to inspect individual sub-task results if needed)
</example>

<example>
NEVER do this:
task_status(...)        → there is no such tool; task already blocks while it runs
run command: sleep 120  → never sleep to wait for a task
</example>

Key points:
- A timeout is not a failure: if the returned ` + "`timedOut`" + ` is true, the task is still running in the background. Do NOT call ` + "`task`" + ` again (that would start a NEW task). Use ` + "`task_detail`" + ` to inspect progress, or simply tell the user the task is still in progress.
- The ` + "`task`" + ` call may take minutes to tens of minutes; that is expected. The UI shows live progress while it runs.

## When you MUST prefer task over running tools step by step

Submit with the ` + "`task`" + ` tool instead of chaining tool calls yourself whenever **any** of these hold:

- The work has **3 or more relatively independent steps or sub-goals** (e.g. "修复所有 lint 问题", "重构多个模块", "批量处理多个文件").
- Sub-tasks have **dependencies or a required order** that need orchestrating.
- You expect **many steps or a long runtime** (e.g. possibly more than 10 tool calls).
- It involves **editing / reading / analyzing multiple files**.
- Key deliverables need **quality review or retries** (code changes, generated content, ...).
- The user explicitly asks for "并行", "分批", or "自动化处理".

Simple, single-step, immediate work (one command, one lookup) should just be done directly — do not route it through task.

## Goal Mode (goalMode)

Passing ` + "`goalMode: true`" + ` to ` + "`task`" + ` enables **closed-loop iteration**. In the default mode, a sub-task that fails review is only patched in place inside that node a limited number of times, and then fails for good. In goal mode, when the final deliverable fails review the engine **automatically rolls back to the upstream work nodes** and reruns them with the review feedback attached, forming a "work → review → fix → review" loop that stops only when review passes or the maximum number of rounds (default 5) is reached.

**CRITICAL**: whenever the requirement contains a **convergence-style acceptance phrase** such as 「直到…为止 / 反复打磨 / 审查到没有新问题 / 全部通过才算 / 达标 / 收敛」, you MUST submit it with ` + "`task`" + ` and ` + "`goalMode: true`" + `. **NEVER handle it inline with subagent / delegate in one shot.** Such work is by definition "not done until it meets the bar", which is exactly what goal mode exists for; handling it inline loses the automatic roll-back-and-redo guarantee.

**Enable it (goalMode: true)** when the requirement carries a **convergence requirement** such as 「目标 / 直到 / 确保 / 全部 / 彻底 / 反复打磨 / 达到某标准」:

- "修复所有测试直到全部通过"
- "重构这个模块，确保 lint 和构建都没有报错"
- "把这篇文章打磨到可以直接发布的质量"
- "清理项目里所有 TypeScript 类型错误"
- "逐个审查每个模块，审查到没有新问题才进行下一个，最后整体审查"

**Leave it off (default false)** for **one-shot deliverables** with no explicit pass bar:

- "调研 Redis 和 Memcached 的差异并写一份对比" (producing it is finishing it; there is nothing to converge on)
- "把这三个文件翻译成英文"
- "统计一下代码库有多少个 Go 文件"

Rule of thumb: **does the task have a "not done until it passes" acceptance condition? Yes → goal mode. Just get the work done → no goal mode.**

Once enabled, ` + "`task`" + ` reports ` + "`goalIteration`" + ` / ` + "`goalMaxIterations`" + ` in its return value so you can see which loop round is running. IMPORTANT: goal mode consumes more time and turns because it iterates repeatedly — NEVER abuse it on simple tasks.

## When to use

- The work is complex and must be broken into several sub-tasks.
- Sub-tasks have dependencies (sequential/parallel).
- Critical tasks need quality review.
- Parallel execution would improve throughput.
- The task has an explicit acceptance bar and must iterate until it is met (pass ` + "`goalMode: true`" + ` as well).`,
	Enabled: true,
}

// ============================================================================
// workflow_submit
// ============================================================================

// submitToolDef 创建 task 工具。
//
// 提交来源（BotID / 前端会话 ID）在**执行时**从 context 读取
// （`tools.CallOriginFromContext`），由 LLMStage 在编排前注入。
// 不能在注册时捕获：工具是静态注册的（bot 启动时一次），而 bot/会话每次调用才确定。
func submitToolDef(mgr *Manager) tools.ToolDef {
	return tools.ToolDef{
		Category: "workflow",
		Scopes:   []string{"private", "group"},
		Tool: llm.Tool{
			Name:         "task",
			DeferredLoad: true, // 工作流非日常任务，初始仅暴露名称+描述
			// 注意：DeferredLoad 会在工具未加载时隐藏 Parameters，此时模型只能看到
			// 这段 Description。因此 goalMode 这类关键能力必须在描述里点出来，
			// 否则模型无从得知该参数的存在。
			Description: "Submit complex, multi-step work. For tasks with several steps, multi-file changes, dependencies, or a need for quality review, you MUST prefer this tool over calling tools step by step yourself — it analyzes the requirement, decomposes it into a DAG of sub-tasks, and executes them asynchronously in parallel, with result review and retries. **When the requirement uses acceptance-style wording such as 「直到…为止 / 反复打磨 / review 到没有新问题 / 全部通过才算 / 达标」, you MUST submit with goalMode: true, and you MUST NOT handle it inline with subagent/delegate** — goal mode automatically rolls back and redoes the work when review fails, forming a 「工作→审查→修复→审查」 loop until the bar is met. It is designed exactly for tasks with an explicit acceptance bar, such as 「修复所有 X 直到全部通过」 or 「审查每个模块直到没有新问题」. **This call is BLOCKING**: the server runs the workflow to a terminal state (completed / failed / terminated) and returns the final status and progress, so you do NOT need — and there is no — a separate status-polling call. When goalMode is enabled, the returned status carries goalIteration / goalMaxIterations so you can see which closed-loop round is running. **NEVER poll and NEVER use sleep to wait; just submit and continue once it returns.**",
			Keywords: []string{
				"目标模式", "goal mode", "闭环", "迭代", "反复打磨", "直到通过",
				"直到…为止", "验收", "达标", "审查到没有", "review 到没有", "收敛",
				"质量审查", "工作流", "workflow", "任务拆解", "并行执行", "DAG",
			},
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"requirement": map[string]any{
						"type":        "string",
						"description": "Description of the work to complete. Be clear and specific, and include every constraint and expected outcome.",
					},
					"maxParallel": map[string]any{
						"type":        "integer",
						"description": "Maximum number of sub-tasks to run in parallel (optional, default 3).",
					},
					"goalMode": map[string]any{
						"type":        "boolean",
						"description": "Goal mode / 闭环迭代 (closed-loop iteration; optional, default false). When enabled, if the final deliverable fails its quality checkpoint, execution automatically rolls back to the corresponding work node and reruns it with the review feedback attached, forming a 「工作→审查→修复→审查」 loop that continues until review passes or the maximum number of rounds is reached. Use it for work that must reach a quality bar through repeated refinement; leave it off for simple tasks.",
					},
				},
				"required": []string{"requirement"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				requirement, _ := m["requirement"].(string)
				if requirement == "" {
					return nil, fmt.Errorf("requirement is required")
				}
				maxParallel := 0
				if v, ok := m["maxParallel"]; ok {
					if f, ok := v.(float64); ok {
						maxParallel = int(f)
					}
				}
				goalMode := false
				if v, ok := m["goalMode"]; ok {
					if b, ok := v.(bool); ok {
						goalMode = b
					}
				}

				// 提交来源由 LLMStage 在编排前注入 context（静态注册拿不到会话）。
				origin := tools.CallOriginFromContext(ctx)

				result, err := mgr.Submit(ctx, SubmitRequest{
					Requirement: requirement,
					MaxParallel: maxParallel,
					GoalMode:    goalMode,
					// 记录来源，供前端刷新后按会话恢复卡片、排查时定位工作空间。
					BotID:     origin.BotID,
					SessionID: origin.SessionID,
				})
				if err != nil {
					return nil, err
				}

				// 提交即阻塞：服务端把工作流跑到终态（completed/failed/terminated）再返回，
				// 对agent 来说 task 就是一个阻塞工具——无需再调轮询工具。
				// 复用与旧 task_status(wait:true) 相同的服务端等待逻辑，并推送进度避免界面卡死。
				var onProgress func(*StatusResult, time.Duration)
				if ctx.SendProgress != nil {
					// 关键：必须在阻塞开始前立刻推一次带 workflowId 的进度。
					//
					// 前端的工作流面板只从工具事件里提取 workflowId 来决定显示哪个工作流。
					// task 改为阻塞后，tool_result 要等工作流全部跑完（可能数十分钟）才到达，
					// 若只依赖 result，整个执行期间面板都不会出现——用户完全看不到进度。
					ctx.SendProgress(taskProgressPayload(result.WorkflowID,
						fmt.Sprintf("工作流已创建（%s），正在分析需求并分解任务…\n", result.WorkflowID)))

					onProgress = func(st *StatusResult, waited time.Duration) {
						ctx.SendProgress(taskProgressPayload(result.WorkflowID,
							fmt.Sprintf("工作流执行中… 状态=%s 已完成 %d/%d 子任务（已等待 %s）\n",
								st.Status, st.Progress.Completed, st.NodeCount,
								waited.Truncate(time.Second))))
					}
				}
				return waitForTerminal(ctx, mgr, result.WorkflowID, taskBlockingMaxTimeout, onProgress)
			}),
		},
		PromptSection: workflowToolPromptSection,
	}
}

// taskProgressPayload 构造 task 工具的进度事件 payload。
//
// 三个字段都不可缺：
//   - stream/chunk：前端 appendToolProgress 消费的既有字段，用于往工具卡片追加输出；
//   - workflowId：**前端工作流面板挂载的唯一来源**。task 是阻塞工具，tool_result 要等
//     工作流跑到终态才到达，若进度事件不带它，整个执行期间面板都不会出现。
//
// 前端 SSE 层会原样展开该 payload（services.js 里`...parts.payload`），
// 因此自定义字段能安全透传。
func taskProgressPayload(workflowID, chunk string) map[string]any {
	return map[string]any{
		"stream":     "stdout",
		"workflowId": workflowID,
		"chunk":      chunk,
	}
}

// asBool 宽松解析布尔入参。
//
// LLM 生成的 JSON 里布尔值经常写成字符串（"true"）或数字（1），
// 严格断言会让参数被静默忽略、功能看似「没生效」。
func asBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "True" || x == "1" || x == "yes"
	case float64:
		return x != 0
	case int:
		return x != 0
	}
	return false
}

// asInt 宽松解析整数入参（JSON 数字会被解成 float64，LLM 也可能给字符串）。
func asInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case string:
		n, err := strconv.Atoi(x)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// ============================================================================
// workflow_nodes
// ============================================================================

// nodesToolDef 创建 task_detail 工具。
func nodesToolDef(mgr *Manager) tools.ToolDef {
	return tools.ToolDef{
		Category: "workflow",
		Scopes:   []string{"private", "group"},
		Tool: llm.Tool{
			Name:         "task_detail",
			DeferredLoad: true, // 工作流非日常任务，初始仅暴露名称+描述
			Description:  "Query the detailed status of every sub-task in a task, including task description, execution result, error message, and dependencies. Two response formats are supported: flat (a sequential flat list) and tree (a tree built from the dependency graph, suited to UI rendering).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskId": map[string]any{
						"type":        "string",
						"description": "Task ID.",
					},
					"format": map[string]any{
						"type":        "string",
						"enum":        []string{"flat", "tree"},
						"description": "Response format: flat (flat list) or tree (tree structure). Default flat.",
					},
				},
				"required": []string{"taskId"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				wfID, _ := m["taskId"].(string)
				if wfID == "" {
					return nil, fmt.Errorf("taskId is required")
				}
				format, _ := m["format"].(string)
				if format == "" {
					format = "flat"
				}
				return mgr.ListNodes(wfID, format)
			}),
		},
	}
}

// ============================================================================
// workflow_control
// ============================================================================

// controlToolDef 创建 task_control 工具。
func controlToolDef(mgr *Manager) tools.ToolDef {
	return tools.ToolDef{
		Category: "workflow",
		Scopes:   []string{"private", "group"},
		Tool: llm.Tool{
			Name:         "task_control",
			DeferredLoad: true, // 工作流非日常任务，初始仅暴露名称+描述
			Description:  "Perform a control operation on a task. Two actions are supported: 1) retry - rerun a specific failed/skipped sub-task; 2) terminate - terminate the whole task (all unfinished sub-tasks are marked as skipped).\n\nIMPORTANT: NEVER call terminate while the task is in the analysis phase (status=analyzing). During analysis the workflow is waiting on model inference to decompose the requirement, so there is no progress output for a while (first-token latency on reasoning models can reach tens of seconds). This is normal, not a hang, and terminating here kills a workflow that would have succeeded. Wait until it reaches running (sub-tasks generated), and only consider terminate if a node is genuinely stuck. If the requirement itself was wrong, fix it and submit again instead of terminating.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskId": map[string]any{
						"type":        "string",
						"description": "Task ID.",
					},
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"retry", "terminate"},
						"description": "Action type: retry (rerun a node) or terminate (terminate the workflow).",
					},
					"nodeId": map[string]any{
						"type":        "string",
						"description": "ID of the sub-task node to retry (required only when action=retry).",
					},
				},
				"required": []string{"taskId", "action"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				wfID, _ := m["taskId"].(string)
				action, _ := m["action"].(string)
				nodeID, _ := m["nodeId"].(string)
				if wfID == "" {
					return nil, fmt.Errorf("taskId is required")
				}
				if action == "" {
					return nil, fmt.Errorf("action is required")
				}
				return mgr.Control(ctx, wfID, ControlRequest{
					Action: ControlAction(action),
					NodeID: nodeID,
				})
			}),
		},
	}
}

// ============================================================================
// RegisterTools — 注册所有工作流工具
// ============================================================================

// RegisterTools 将工作流工具注册到 ToolManager。
//
// 注册的工具：
//   - task:          提交复杂多步骤任务；该调用会**阻塞**到工作流进入终态才返回
//   - task_detail:   查询子任务详情
//   - task_control:  控制操作（重试/终止）
//
// 使用示例：
//
//	wfMgr := workflow.Setup(wireCfg)
//	workflow.RegisterTools(toolMgr, wfMgr)
func RegisterTools(mgr *tools.ToolManager, wfMgr *Manager) error {
	return mgr.RegisterMany(
		submitToolDef(wfMgr),
		nodesToolDef(wfMgr),
		controlToolDef(wfMgr),
	)
}
