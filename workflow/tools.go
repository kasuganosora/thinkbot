package workflow

import (
	"fmt"

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
2. **轮询进度**：使用 ` + "`task_status`" + ` 查询任务状态（analyzing → running → completed/failed/terminated）
3. **查看详情**：使用 ` + "`task_detail`" + ` 查看各子任务的执行状态和结果（支持平铺和树状视图）
4. **流程控制**：节点失败时用 ` + "`task_control`" + ` 重试，或终止整个任务

## 使用时机

- 任务复杂，需要拆解为多个子任务
- 子任务间有依赖关系（串行/并行）
- 关键任务需要质量审查
- 需要并行处理提高效率`,
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
			Name:        "task",
			Description: "提交复杂多步骤任务。自动分析需求、拆解为子任务 DAG 图并异步执行。适用于需要拆解为多个步骤、有依赖关系或需要质量审查的复杂任务。立即返回 task_id，后续通过 task_status 轮询进度。",
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

				result, err := mgr.Submit(ctx, SubmitRequest{
					Requirement: requirement,
					MaxParallel: maxParallel,
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
			Name:        "task_status",
			Description: "查询任务的当前状态和进度。返回任务状态（analyzing/running/completed/failed/terminated）、各状态子任务数量统计。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskId": map[string]any{
						"type":        "string",
						"description": "任务 ID（由 task 工具返回）",
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
				return mgr.GetStatus(wfID)
			}),
		},
	}
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
			Name:        "task_detail",
			Description: "查询任务中各子任务的详细状态，包括任务描述、执行结果、错误信息、依赖关系等。支持两种返回格式：flat（顺序平铺列表）和 tree（按依赖关系构建的树状结构，适合前端展示）。",
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
			Name:        "task_control",
			Description: "对任务执行控制操作。支持两种操作：1) retry - 重试指定的失败/跳过子任务；2) terminate - 终止整个任务（所有未完成子任务标记为跳过）。",
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
				return mgr.Control(wfID, ControlRequest{
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
