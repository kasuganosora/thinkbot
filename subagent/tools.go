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
	Content: `# SubAgent Delegation

You can use the ` + "`spawn`" + ` tool to delegate work to sub-agents that run with their own isolated context.

## When to use it

- **The task is complex and needs a lot of intermediate reasoning**: delegating keeps those intermediate steps from polluting your own conversation context.
- **A different role or perspective is needed**: give the sub-agent a dedicated system prompt (e.g. "你是安全审计专家").
- **Several independent sub-tasks**: spawn multiple sub-agents at once and run them in parallel.
- **Context isolation is required**: a sub-agent's conversation history is fully isolated from yours.

## Sub-agent capability boundary

- **It can use every workspace tool**: sub-agents run in the same per-bot workspace (the same sandbox) as you, so exec and the file tools (read / write / list directory) are **fully available** to them. A sub-agent can genuinely create files, run commands, and produce real artifacts — not just return textual suggestions. Delegate file work directly instead of doing it yourself.
- **It CANNOT spawn sub-agents**: to prevent unbounded nesting, a sub-agent may not call spawn. This is its only tool restriction; every other tool you may use is available to it.

## Usage

<example>
spawn({
  tasks: ["分析这段代码的安全风险", "同时检查性能瓶颈"],
  system_prompt: "你是一个代码审查专家"
})
</example>

- tasks: the list of tasks to run; each task executes in its own sub-agent.
- system_prompt: the sub-agent's role definition (optional).

## Rules

- Answer simple questions yourself. Do not over-delegate.
- **Independent sub-tasks MUST be merged into a single spawn call**: if you can split the work into N mutually independent sub-tasks, you MUST put all of them in the tasks array of **one** spawn call (max 5). Tasks in the same call run **in parallel automatically**. NEVER call spawn multiple times just to "batch" the work — separate calls are executed sequentially by the main agent and are therefore slower.
- Only split into multiple spawn calls **when a later step depends on an earlier step's result**.
- Describe the sub-agent's role and responsibilities clearly in system_prompt.
- IMPORTANT: the sub-agent's output is surfaced to the end user, so instruct it in system_prompt to respond in Chinese (中文).

<example>
Correct — review three modules in parallel with one call:
spawn({
  tasks: ["审查模块A的安全风险", "审查模块B的性能瓶颈", "审查模块C的可维护性"],
  system_prompt: "你是一个代码审查专家"
})
</example>

<example>
NEVER do this — three separate calls run sequentially and are slower:
spawn({ tasks: ["审查模块A"] })   // the main agent waits for it to return
spawn({ tasks: ["审查模块B"] })   // waits again
spawn({ tasks: ["审查模块C"] })   // waits again
</example>
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
			Description: "Create one or more sub-agents to execute tasks. Each sub-agent has its own isolated conversation context and a role you define via system_prompt. Multiple tasks run in parallel and their results are returned synchronously. Use it for complex work that needs context isolation or parallel processing.",
			// 延迟加载：spawn 是「按需委托」的 Heavy 工具，非每轮必需。初始仅暴露
			// 名称+描述，完整 schema 由模型经 tool_search 或直接引用触发加载（同时其
			// 提示词段 spawnToolPromptSection 始终注入，教模型如何调用）。这与 offload
			// 指针提示「委托 spawn 读落盘文件」协同：深挖代码的代价隔离到子 agent 上下文。
			DeferredLoad: true,
			Keywords:     []string{"子代理", "子agent", "委托", "delegate", "sub-agent", "spawn", "并行任务", "子任务"},
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tasks": map[string]any{
						"type":        "array",
						"description": "The list of tasks to delegate. Each task runs in its own sub-agent, in parallel. At most " + fmt.Sprintf("%d", maxTasksPerSpawn) + " tasks. IMPORTANT: put all mutually independent sub-tasks in this one array instead of making several spawn calls.",
						"items":       map[string]any{"type": "string"},
					},
					"system_prompt": map[string]any{
						"type":        "string",
						"description": "System prompt for the sub-agent, defining its role, domain expertise, and behavioral rules — for example \"你是一个专业的代码审查专家\". Write it in Chinese (中文), since the sub-agent's output is surfaced to Chinese end users. If left empty, the sub-agent falls back to a generic assistant role.",
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
						success := res == nil || res.Success
						fields := []any{
							"phase", phase,
							"index", index,
							"total", total,
							"task", task,
							"elapsed", elapsed.String(),
							"success", success,
						}
						if res != nil && !res.Success && res.Error != "" {
							fields = append(fields, "error", res.Error)
						}
						// 失败用 Warn 让其从日志中凸显，便于排障。
						if success {
							l.Infow("spawn: subagent progress", fields...)
						} else {
							l.Warnw("spawn: subagent progress", fields...)
						}
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
