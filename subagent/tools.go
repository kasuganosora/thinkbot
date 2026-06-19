package subagent

import (
	"fmt"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// SubAgent 工具定义
//
// 参考 Memoh 的设计，只暴露一个统一的 spawn 工具：
//
//	spawn({ tasks: ["任务1", "任务2"], system_prompt: "你是..." })
//
// 主 Agent 通过这个工具将任务委托给拥有独立上下文的 SubAgent。
// 支持并行执行多个任务，结果同步返回。
// ============================================================================

// spawnToolPromptSection 是 SubAgent 工具的提示词段落。
var spawnToolPromptSection = &tools.ToolPromptSection{
	Name:  "subagent_spawn",
	Order: 305,
	Content: `# 子 Agent 委托

你可以使用 ` + "`spawn`" + ` 工具将任务委托给拥有独立上下文的子 Agent 执行。

## 何时使用

- **任务复杂、需要大量中间推理**：委托给子 Agent 可以避免中间步骤污染你的对话上下文
- **需要不同角色/视角**：为子 Agent 设置专门的系统提示词（如"你是安全审计专家"）
- **多个独立子任务**：可以一次性 spawn 多个子 Agent 并行处理
- **需要隔离上下文**：子 Agent 的对话历史与你完全隔离

## 使用方式

` + "```" + `
spawn({
  tasks: ["分析这段代码的安全风险", "同时检查性能瓶颈"],
  system_prompt: "你是一个代码审查专家"
})
` + "```" + `
- tasks: 要执行的任务列表，每个任务在独立的子 Agent 中执行
- system_prompt: 子 Agent 的角色定义（可选）

## 使用原则

- 简单任务直接回答，不要过度委托
- 多个独立任务可以放在一个 spawn 调用中并行处理
- 在 system_prompt 中清晰描述子 Agent 的角色和职责`,
	Enabled: true,
}

const maxTasksPerSpawn = 5

// SpawnToolDef 返回统一的 spawn 工具定义。
// 创建一个或多个 SubAgent 并行执行任务，结果同步返回。
func SpawnToolDef(mgr *SubAgentManager) tools.ToolDef {
	return tools.ToolDef{
		Category: "subagent",
		Scopes:   []string{"private", "group"},
		Tool: llm.Tool{
			Name:        "spawn",
			Description: "创建一个或多个子 Agent 来执行任务。每个子 Agent 拥有独立的对话上下文和指定的角色（通过 system_prompt 定义）。支持并行执行多个任务，结果同步返回。适合需要隔离上下文或并行处理的复杂任务。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tasks": map[string]any{
						"type":        "array",
						"description": "要委托给子 Agent 执行的任务列表。每个任务在独立的子 Agent 中并行执行。最多 " + fmt.Sprintf("%d", maxTasksPerSpawn) + " 个任务。",
						"items":       map[string]any{"type": "string"},
					},
					"system_prompt": map[string]any{
						"type":        "string",
						"description": "子 Agent 的系统提示词，定义其角色、专业领域和行为规范。例如：\"你是一个专业的代码审查专家\"。如果留空，子 Agent 将使用通用助手角色。",
					},
				},
				"required": []string{"tasks"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}

				// 解析 tasks
				tasksRaw, ok := m["tasks"]
				if !ok {
					return nil, fmt.Errorf("tasks is required")
				}
				tasksArr, ok := tasksRaw.([]any)
				if !ok {
					return nil, fmt.Errorf("tasks must be an array")
				}
				if len(tasksArr) == 0 {
					return nil, fmt.Errorf("tasks must not be empty")
				}

				// 截断到最大数量
				if len(tasksArr) > maxTasksPerSpawn {
					tasksArr = tasksArr[:maxTasksPerSpawn]
				}

				tasks := make([]string, 0, len(tasksArr))
				for _, t := range tasksArr {
					s, ok := t.(string)
					if !ok {
						return nil, fmt.Errorf("each task must be a string")
					}
					if s != "" {
						tasks = append(tasks, s)
					}
				}
				if len(tasks) == 0 {
					return nil, fmt.Errorf("tasks must contain at least one non-empty string")
				}

				systemPrompt, _ := m["system_prompt"].(string)

				results := mgr.DelegateMany(ctx, systemPrompt, tasks)

				return map[string]any{
					"success": true,
					"count":   len(results),
					"results": results,
				}, nil
			}),
		},
		PromptSection: spawnToolPromptSection,
	}
}

// RegisterTools 将 spawn 工具注册到 ToolManager。
//
// 使用示例：
//
//	saMgr := subagent.NewSubAgentManager(bundle.Main, bundle.MainDef.Model)
//	subagent.RegisterTools(toolMgr, saMgr)
//	defer saMgr.CloseAll()
func RegisterTools(mgr *tools.ToolManager, saMgr *SubAgentManager) error {
	return mgr.RegisterMany(SpawnToolDef(saMgr))
}
