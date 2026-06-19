# Workflow — DAG 任务引擎

供主 Agent 使用的复杂任务自动化分解与执行引擎。将用户需求自动拆解为 DAG 子任务图，按拓扑序并行调度，支持失败重试和 Review 质量迭代。

## 核心流程

```
用户需求 ──→ Analyzer(LLM分解) ──→ DAG节点图
                                      │
                         ┌────────────┘
                         ▼
                    Scheduler 调度
                    ┌──────┼──────┐
                    ▼      ▼      ▼
                  Node   Node   Node   (并行执行)
                 (SubAgent)  ...       每个节点 = 独立 SubAgent
                    │
                    ▼
              Review (可选) ──→ 不通过 ──→ 带反馈重新执行
                    │
                    ▼
                Completed
```

1. **Analyzer** — 使用 LLM（JSON 模式）将需求文本分解为 DAG 节点图，自动识别依赖关系和审查需求
2. **Scheduler** — 按拓扑序调度，同层无依赖节点并行执行（semaphore 限流）
3. **Executor** — 每个节点由独立 SubAgent 执行；`review=true` 的节点执行后启动 Review 自循环
4. **全程异步** — 提交后立即返回 `task_id`，LLM 通过工具轮询进度

## 架构

| 组件 | 文件 | 职责 |
|------|------|------|
| `Manager` | `manager.go` | 统一入口：Submit / GetStatus / ListNodes / Control |
| `Analyzer` | `analyzer.go` | LLM 需求分析 + DAG 生成 + 校验 |
| `Scheduler` | `scheduler.go` | DAG 拓扑调度、并行限流、重试/Review 循环、级联跳过 |
| `Executor` | `executor.go` | 节点执行（SubAgent Delegate）、Review 审查、带反馈迭代 |
| `DAG` | `dag.go` | 纯领域算法：校验、环检测、就绪节点计算、级联跳过、树构建 |
| `Repository` | `repository.go` | 内存优先 + DB 双写持久化 |
| `Models` | `models.go` | GORM 模型（JSON 全量序列化策略） |
| `Wire` | `wire.go` | 组合根：`Setup()` 统一装配 |
| `Tools` | `tools.go` | 暴露给主 Agent 的 LLM 工具 |
| `Types` | `types.go` | 领域模型、枚举、视图结构 |

## LLM 工具

引擎向主 Agent 注册 4 个工具：

| 工具 | 说明 |
|------|------|
| `task` | 提交需求，异步创建工作流，立即返回 `task_id` |
| `task_status` | 查询工作流状态（analyzing → running → completed/failed/terminated）和进度统计 |
| `task_detail` | 查询子任务列表（`flat` 平铺 / `tree` 树状） |
| `task_control` | 控制操作：重试指定失败节点 / 终止整个工作流 |

> 工具命名与主流 LLM 预训练中的 agentic 工具名（如 Claude 的 Task、LangChain 的 TaskTool）对齐，降低 LLM 适配成本。

## 节点生命周期

```
pending → ready → running ──→ completed
                    │              ↑
                    ├──(review)──→ reviewing ──→ passed ──→ completed
                    │                  │
                    │                  └── not passed ──→ running (带反馈重执行)
                    │
                    └──→ failed (重试耗尽) ──→ 下游级联 skipped
```

## 配置

通过 `config.Store` 管理，支持运行时动态调整。未配置时使用默认值。

| 配置键 | 默认值 | 说明 |
|--------|--------|------|
| `workflow.max_parallel` | `3` | 最大并行执行节点数 |
| `workflow.max_retries` | `2` | 节点执行失败最大重试次数 |
| `workflow.max_iterations` | `3` | Review 不通过时的最大迭代次数 |
| `workflow.retry_initial_ms` | `500` | 重试初始退避间隔（毫秒） |
| `workflow.retry_max_ms` | `10000` | 重试最大退避间隔（毫秒） |
| `workflow.schedule_interval_ms` | `200` | 调度器轮询间隔（毫秒） |
| `workflow.analyzer_temperature` | `0.3` | 分析器 LLM temperature |
| `workflow.analyzer_max_tokens` | `4096` | 分析器 LLM max_tokens |

## 快速接入

```go
import (
    "github.com/kasuganosora/thinkbot/workflow"
    "github.com/kasuganosora/thinkbot/config"
)

// 1. 装配引擎
wfMgr, saMgr := workflow.Setup(workflow.WireConfig{
    Provider:       bundle.Main,        // LLM Provider
    Model:          bundle.MainDef.Model,
    DB:             gormDB,             // 可为 nil（纯内存模式）
    Logger:         logger,
    TracerProvider: tp,
    Store:          configStore,        // 可为 nil（使用默认值）
})
defer saMgr.CloseAll()

// 2. 注册工具到主 Agent
workflow.RegisterTools(toolMgr, wfMgr)
```

## 反嵌套保证

Workflow 工具的 Scopes 为 `["private", "group"]`，在 SubAgent 上下文中不可见，无法递归创建工作流。此外，引擎内部使用独立的 `SubAgentManager`，通过 `Delegate` 一次性调用执行，不经过主 Agent 的 ToolManager，无法访问任何工具。

## 持久化

采用 JSON 全量序列化策略：整个 `Workflow` 对象序列化为 `WorkflowModel.Data` 字段。读操作优先从内存缓存获取（O(1)），写操作同时更新内存和 DB。表名 `workflow_workflows`。

## 崩溃恢复

进程因发布、OOM 或 Kill 中断后，数据库中会残留 `analyzing` / `running` 状态的工作流。`Manager.Recover()` 在启动时扫描并自动恢复：

```go
// 3. 启动时恢复中断的工作流
result, err := wfMgr.Recover(context.Background())
// result.Resumed:    从调度阶段恢复的工作流数
// result.Reanalyzed: 需要重新分析的工作流数
```

恢复策略：

| 中断时状态 | 节点情况 | 恢复动作 |
|-----------|---------|---------|
| `analyzing` | 无节点 | 重新提交 Analyzer 分析（Phase 1 从头开始） |
| `analyzing` / `running` | 已有节点 | 重置 `running`/`reviewing`/`ready` 节点为 `pending`，直接恢复调度（Phase 2 续跑） |
| `interrupted` | 已有节点 | 同上 |

关键设计：
- **已完成的节点保留**：`completed`/`failed`/`skipped` 节点状态不变，避免重复执行
- **中间状态重置**：被中断的 `running`/`reviewing` 节点清零 retry/iteration 计数，重置为 `pending` 等待重新调度
- **幂等安全**：重复调用 `Recover()` 会跳过已经在运行中的工作流
