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
// 暴露 4 个 LLM 工具给主 Agent：
//   - task:          提交需求，异步创建工作流
//   - task_status:   查询工作流状态和进度
//   - task_detail:   查询节点列表（flat / tree）
//   - task_control:  控制操作（重试节点 / 终止工作流）
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
	Content: `# 任务引擎

你可以使用 ` + "`task`" + ` 工具来处理复杂的多步骤任务。任务引擎会自动将需求分解为子任务 DAG 图，并行/串行执行，并支持结果审查和质量迭代。

## 使用流程

1. **提交任务**：使用 ` + "`task`" + ` 提交任务需求，获取 task_id
2. **等待完成**：使用 ` + "`task_status`" + ` 并传 ` + "`wait: true`" + `，服务端会阻塞直到任务进入终态才返回
3. **查看详情**：使用 ` + "`task_detail`" + ` 查看各子任务的执行状态和结果（支持平铺和树状视图）
4. **流程控制**：节点失败时用 ` + "`task_control`" + ` 重试，或终止整个任务

## 等待任务完成：必须用 wait，禁止自行轮询

任务的分析与执行通常需要数分钟到数十分钟。**服务端已提供等待能力，不要自己轮询。**

判断口诀：**想知道任务结果 → 一次 ` + "`task_status(wait: true)`" + ` → 拿到终态再继续。**

正例：
` + "```" + `
task(...)                             → 得到 task_id
task_status(taskId, wait: true)       → 阻塞直到 completed/failed/terminated
（若返回 timedOut: true，说明仍在跑，可再调一次继续等）
` + "```" + `

反例（**严禁**，会浪费大量调用轮次并让对话被无意义的进度卡片淹没）：
` + "```" + `
task_status(taskId)                   → "仍在分析中，让我继续等待"
执行命令 sleep 90
task_status(taskId)                   → "仍在分析中…"
执行命令 sleep 120
task_status(taskId)                   → …（重复十几次）
` + "```" + `

要点：
- **不要用 ` + "`sleep`" + ` 等待任务**。等待交给 ` + "`task_status(wait: true)`" + `。
- 不带 wait 的 ` + "`task_status`" + ` 只用于「顺便看一眼当前进度」，不要用它做循环等待。
- 超时返回不是失败：` + "`timedOut: true`" + ` 表示任务仍在进行，直接再调一次即可。

## 何时必须优先使用 task（而非逐步手动执行工具）

当你面对的任务满足以下**任一**条件时，应优先调用 'task' 工具提交，而不是自己一步步串行调用工具：

- 包含 **3 个及以上相对独立的步骤或子目标**（如"修复所有 lint 问题""重构多个模块""批量处理多个文件"）
- 子任务之间存在**依赖或先后顺序**需要编排
- 预计**步骤较多或耗时较长**（如可能超过 10 次工具调用）
- 涉及**多个文件的改动 / 读取 / 分析**
- 关键产物需要**质量审查或重试**（代码改动、生成内容等）
- 用户明确要求"并行""分批""自动化处理"

简单、单步、即时任务（单条命令、单次查询）直接执行即可，无需走 task。

## 目标模式（goalMode）

` + "`task`" + ` 的 ` + "`goalMode: true`" + ` 参数开启**闭环迭代**：默认模式下子任务审查不通过只会在该节点内部就地修复有限次，仍不通过就失败收场；开启目标模式后，最终产物审查不通过时会**自动回退到上游的工作节点**、带着审查意见重新执行，形成「工作 → 审查 → 修复 → 审查」的循环，直到审查通过或达到最大轮数（默认 5 轮）才停止。

**🚫 强制规则（务必遵守）**：只要需求出现「直到…为止 / 反复打磨 / 审查到没有新问题 / 全部通过才算 / 达标 / 收敛」这类**收敛性验收表述**，你【必须】用 ` + "`task`" + ` 并以 ` + "`goalMode: true`" + ` 提交，**【禁止】改用 subagent / delegate 一次性内联完成**。这类任务的本质就是「不达标不算完」，正是目标模式的设计目的；用 subagent 内联处理会丢失「审查不通过就自动回退重做」的闭环保障。

**应当开启（goalMode: true）**——需求描述里出现「目标 / 直到 / 确保 / 全部 / 彻底 / 反复打磨 / 达到某标准」这类**收敛性要求**时：

- "修复所有测试直到全部通过"
- "重构这个模块，确保 lint 和构建都没有报错"
- "把这篇文章打磨到可以直接发布的质量"
- "清理项目里所有 TypeScript 类型错误"
- "逐个审查每个模块，审查到没有新问题才进行下一个，最后整体审查"

**不要开启（保持默认 false）**——**一次性产出**、没有明确合格线的任务：

- "调研 Redis 和 Memcached 的差异并写一份对比"（产出即完成，无需反复收敛）
- "把这三个文件翻译成英文"
- "统计一下代码库有多少个 Go 文件"

判断口诀：**任务有没有一个「做完了才算数」的验收条件？有 → 开目标模式；只是把活干完就行 → 不开。**

开启后可通过 ` + "`task_status`" + ` 的 ` + "`goalIteration`" + ` / ` + "`goalMaxIterations`" + ` 观察当前处于第几轮闭环。注意目标模式会因反复迭代而消耗更多时间和轮次，不要对简单任务滥用。

## 使用时机

- 任务复杂，需要拆解为多个子任务
- 子任务间有依赖关系（串行/并行）
- 关键任务需要质量审查
- 需要并行处理提高效率
- 任务有明确验收标准、必须迭代到达标（此时额外传 ` + "`goalMode: true`" + `）`,
	Enabled: true,
}

// ============================================================================
// workflow_submit
// ============================================================================

// submitToolDef 创建 task 工具。
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
			Description: "提交复杂多步任务。对于包含多个步骤、多文件改动、有依赖关系或需要质量审查的复杂任务，你应优先使用此工具而非逐步手动调用工具——它会自动分析需求、拆解为子任务 DAG 图并异步并行执行，且支持结果审查与重试。**当需求出现「直到…为止 / 反复打磨 / review 到没有新问题 / 全部通过才算 / 达标」等验收式表述时，你【必须】传 goalMode: true 提交，且【禁止】用 subagent/delegate 自行内联处理**——目标模式会在审查不通过时自动回退重做，形成「工作→审查→修复→审查」循环直到达标，专为「修复所有 X 直到全部通过」「审查每个模块直到没有新问题」这类有明确验收标准的任务设计。立即返回 task_id；随后调用一次 task_status(taskId, wait: true) 即可阻塞等到任务结束，**不要自己反复轮询或用 sleep 等待**。",
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
						"description": "要完成的任务描述。尽量清晰、具体，包含所有约束和期望结果。",
					},
					"maxParallel": map[string]any{
						"type":        "integer",
						"description": "最大并行执行子任务数（可选，默认 3）",
					},
					"goalMode": map[string]any{
						"type":        "boolean",
						"description": "目标模式（闭环迭代，可选，默认 false）。开启后：最终产物的质量检查点若审查不通过，会自动回退到对应工作节点、带着审查意见重新执行，形成「工作→审查→修复→审查」的循环，直到审查通过或达到最大轮数。适合「必须达到某质量标准、反复打磨」的任务；简单任务无需开启。",
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

				result, err := mgr.Submit(ctx, SubmitRequest{
					Requirement: requirement,
					MaxParallel: maxParallel,
					GoalMode:    goalMode,
				})
				if err != nil {
					return nil, err
				}
				return result, nil
			}),
		},
		PromptSection: workflowToolPromptSection,
	}
}

// ============================================================================
// workflow_status
// ============================================================================

// statusToolDef 创建 task_status 工具。
func statusToolDef(mgr *Manager) tools.ToolDef {
	return tools.ToolDef{
		Category: "workflow",
		Scopes:   []string{"private", "group"},
		Tool: llm.Tool{
			Name:         "task_status",
			DeferredLoad: true, // 工作流非日常任务，初始仅暴露名称+描述
			// 注意：DeferredLoad 工具在未加载时只暴露 Name + Description（Parameters 被隐藏），
			// 因此 wait 这个关键能力必须写进 Description，否则模型根本不知道它存在，
			// 只会退回到「自己反复调用 + sleep」的低效轮询。
			Description: "查询任务的当前状态和进度，**并可让服务端代为等待任务结束**。" +
				"返回任务状态（analyzing/running/completed/failed/terminated）、各状态子任务数量统计。" +
				"若任务开启了目标模式，还会返回 goalMode/goalIteration/goalMaxIterations，表示当前处于第几轮闭环迭代。\n" +
				"**强烈建议传 wait: true**：服务端会阻塞直到任务进入终态（成功/失败/终止）才返回，" +
				"你只需调用一次就能拿到最终结果。**禁止自己反复调用本工具轮询、也禁止用 sleep 等待**——" +
				"那会浪费大量调用轮次并让对话被无意义的进度卡片淹没。" +
				"若返回中 timedOut 为 true，表示等待超时而任务仍在进行，此时可再调用一次继续等待。",
			Keywords: []string{"任务状态", "进度", "轮询", "等待", "阻塞等待", "目标模式", "闭环轮次", "workflow"},
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskId": map[string]any{
						"type":        "string",
						"description": "任务 ID（由 task 工具返回）",
					},
					"wait": map[string]any{
						"type": "boolean",
						"description": "是否由服务端阻塞等待任务进入终态后再返回。默认 false（立即返回当前快照）。" +
							"等待任务完成时应传 true，这样只需一次调用，避免自行轮询。",
					},
					"timeoutSeconds": map[string]any{
						"type": "integer",
						"description": "wait=true 时的最长等待秒数。默认 600（10 分钟），上限 1800（30 分钟）。" +
							"超时不算失败，会返回当前快照并置 timedOut=true。",
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

				if !asBool(m["wait"]) {
					return mgr.GetStatus(wfID)
				}

				// wait 模式：服务端代为轮询，agent 只在终态/超时时被唤醒。
				timeout := time.Duration(asInt(m["timeoutSeconds"])) * time.Second

				// 等待期间向前端推送进度，避免界面看起来卡死。
				// SendProgress 仅在流式模式下可用，非流式时为 nil。
				var onProgress func(*StatusResult, time.Duration)
				if ctx.SendProgress != nil {
					onProgress = func(st *StatusResult, waited time.Duration) {
						ctx.SendProgress(map[string]any{
							"stream": "stdout",
							"chunk": fmt.Sprintf("等待任务完成… 状态=%s 已完成 %d/%d 子任务（已等待 %s）\n",
								st.Status, st.Progress.Completed, st.NodeCount,
								waited.Truncate(time.Second)),
						})
					}
				}

				return waitForTerminal(ctx, mgr, wfID, timeout, onProgress)
			}),
		},
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
			Description:  "查询任务中各子任务的详细状态，包括任务描述、执行结果、错误信息、依赖关系等。支持两种返回格式：flat（顺序平铺列表）和 tree（按依赖关系构建的树状结构，适合前端展示）。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskId": map[string]any{
						"type":        "string",
						"description": "任务 ID",
					},
					"format": map[string]any{
						"type":        "string",
						"enum":        []string{"flat", "tree"},
						"description": "返回格式：flat（平铺列表）或 tree（树状结构）。默认 flat。",
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
			Description:  "对任务执行控制操作。支持两种操作：1) retry - 重试指定的失败/跳过子任务；2) terminate - 终止整个任务（所有未完成子任务标记为跳过）。\n\n⚠️ 不要在分析阶段（status=analyzing）调用 terminate：此时工作流正在等待模型推理以分解任务，会较久没有进度输出（尤其推理模型首 token 延迟可达数十秒），这是正常现象，并非卡死。此时终止会杀掉一个本可成功的工作流。请等待其进入 running（已生成子任务）后，若确有节点卡住再考虑 terminate；若只是需求有误，应修正后重新提交，而非终止。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskId": map[string]any{
						"type":        "string",
						"description": "任务 ID",
					},
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"retry", "terminate"},
						"description": "操作类型：retry（重试节点）或 terminate（终止工作流）",
					},
					"nodeId": map[string]any{
						"type":        "string",
						"description": "要重试的子任务节点 ID（仅 action=retry 时需要）",
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
//   - task:          提交复杂多步骤任务
//   - task_status:   查询任务状态和进度
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
		statusToolDef(wfMgr),
		nodesToolDef(wfMgr),
		controlToolDef(wfMgr),
	)
}
