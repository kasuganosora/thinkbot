package subagent

import (
	"fmt"
	"time"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// SubAgent 工具定义
//
// 子代理工具入口，只暴露一个统一的 spawn 工具：
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

## 子 Agent 的能力边界

- **可以使用全部工作空间工具**：子 Agent 运行在与你相同的 per-bot 工作空间（同一沙箱），
  因此 exec、读/写/列目录等文件操作工具对它**完全可用**——它可以真正创建文件、运行命令、
  产出可落地的产物，而不只是返回文本建议。需要文件操作时直接委托，不必自己代劳。
- **不能 spawn 子 Agent**：为防止无限嵌套（套娃），子 Agent 无法再调用 spawn。
  这是它唯一的工具限制；其余你有权使用的工具它都能用。

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
- **独立子任务必须合并进同一次 spawn**：如果你能把任务拆成 N 个互相独立、互不依赖的子任务，必须把它们全部放进**同一次** spawn 的 tasks 数组（最多 5 个）。同一次调用中的多个任务会**自动并行**执行；**禁止**为了"分批"而多次调用 spawn 工具——多次调用会被主 Agent 串行执行，反而更慢。
- **仅当后一步依赖前一步的结果时**，才分多次调用 spawn。
- 在 system_prompt 中清晰描述子 Agent 的角色和职责

正确示例（一次并行审查三个模块）：
` + "```" + `
spawn({
  tasks: ["审查模块A的安全风险", "审查模块B的性能瓶颈", "审查模块C的可维护性"],
  system_prompt: "你是一个代码审查专家"
})
` + "```" + `
错误示例（拆成三次调用 → 串行、更慢，不要这样做）：
` + "```" + `
spawn({ tasks: ["审查模块A"] })   // 主 Agent 等它返回
spawn({ tasks: ["审查模块B"] })   // 再等
spawn({ tasks: ["审查模块C"] })   // 再等
` + "```" + `
`,
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
					traceid.L(ctx).Warnw("spawn: tasks truncated",
						"requested", len(tasksArr), "max", maxTasksPerSpawn)
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

			// 诊断日志：记录本次 spawn 实际派发的任务数，便于观察模型是否把独立子任务
			// 合并进单次调用（并行）还是分多次调用（串行）。
			if l := traceid.L(ctx); l != nil {
				l.Infow("spawn: delegate many", "tasks", len(tasks), "system_prompt_set", systemPrompt != "")
			}

			// 心跳保活：spawn 是同步阻塞调用（DelegateMany 返回整个子 Agent 的最终结果），
			// 重任务（读大量文件 + 多轮模型推理）很容易超过前端 3 分钟「卡死看门狗」阈值，
			// 触发误报「执行超时：连接可能已中断」。周期性发送 heartbeat 进度以重置前端计时器。
			stopHeartbeat := make(chan struct{})
			if ctx.SendProgress != nil {
				go func() {
					ticker := time.NewTicker(30 * time.Second)
					defer ticker.Stop()
					for {
						select {
						case <-stopHeartbeat:
							return
						case <-ticker.C:
							ctx.SendProgress(map[string]any{
								"stream": "heartbeat",
								"chunk":  "子 Agent 仍在执行中（读取文件 / 模型推理）…\n",
							})
						}
					}
				}()
			}

			// 流式进度：把 DelegateMany 内每个子 Agent 的「启动/完成」实时推到 UI，
			// 让并行的多个 subagent 可见（否则 spawn 同步阻塞期间 UI 只有心跳，看不出并行）。
			progressHandler := func(phase string, index, total int, task string, elapsed time.Duration, res *TaskResult) {
				if l := traceid.L(ctx); l != nil {
					l.Infow("spawn: subagent progress", "phase", phase, "index", index, "total", total, "task", task, "elapsed", elapsed.String())
				}
				if ctx.SendProgress == nil {
					return
				}
				var chunk string
				switch phase {
				case "start":
					chunk = fmt.Sprintf("🔄 子 Agent %d/%d 启动：%s", index, total, task)
				case "done":
					status := "✅"
					if res != nil && !res.Success {
						status = "❌"
					}
					chunk = fmt.Sprintf("%s 子 Agent %d/%d 完成（耗时 %s）：%s",
						status, index, total, elapsed.Round(time.Second), task)
				}
				ctx.SendProgress(map[string]any{
					"stream": "subagent",
					"chunk":  chunk,
				})
			}

			results := mgr.DelegateMany(WithDelegateProgress(ctx, progressHandler), systemPrompt, tasks)

			close(stopHeartbeat)

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
