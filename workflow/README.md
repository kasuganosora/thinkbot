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

## 实时进度事件（旁路输出集成）

Workflow 引擎通过 `EventBus` 发布实时进度事件，Web 端可通过 SSE 订阅指定 `workflow_id` 的事件流。

### 接入方式

```go
// Setup 时传入 EventBus（来自 Pipeline 主流程）
wfMgr, saMgr := workflow.Setup(workflow.WireConfig{
    // ...
    EventBus: bus,  // outbound.EventBus 实例（可为 nil）
})
```

### 事件类型

| 事件类型 | 触发时机 | Data 字段 |
|---------|---------|-----------|
| `workflow.submitted` | 工作流已提交 | `requirement` |
| `workflow.analyzed` | DAG 分析完成 | `node_count`, `nodes[]` |
| `workflow.completed` | 工作流全部成功 | `node_count` |
| `workflow.failed` | 工作流失败 | `error` |
| `workflow.terminated` | 工作流被终止 | — |
| `workflow.node.started` | 节点开始执行 | `node_id`, `node_name`, `task` |
| `workflow.node.completed` | 节点完成 | `node_id`, `retry_count`, `iteration_count`, `result_preview` |
| `workflow.node.failed` | 节点失败 | `node_id`, `retry_count`, `error` |
| `workflow.node.reviewing` | 节点进入 Review | `node_id`, `iteration` |
| `workflow.node.retrying` | 节点执行重试 | `node_id`, `attempt`, `error` |

> 所有事件的 `trace_id` 字段 = `workflow_id`，Web SSE 端通过 `bus.Subscribe(workflowID)` 筛选。

### Web SSE 订阅示例

```go
// SSE Handler — 支持断线重连
// 前端在 Last-Event-ID 中携带上次收到的 Seq，首次连接传 0
sinceSeq := parseLastEventID(r) // 从请求头解析，默认 0
sub := bus.SubscribeWithReplay(workflowID, sinceSeq)
defer bus.Unsubscribe(sub)
for event := range sub.C() {
    // event.Seq 为全局单调递增序列号
    // event.Type = "workflow.node.completed"
    // event.Data["node_id"] = "n1"
    // 将 Seq 作为 SSE id 字段发送，前端用于下次重连
    writeSSE(event.Seq, event)
}
```

### 断线重连机制

用户关闭页面再打开时，之前的 SSE 连接已断开，期间的实时事件会丢失。
EventBus 内置了 **EventStore（环形缓冲 + TTL）** 解决此问题：

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `StoreCapacity` | 10000 | 环形缓冲最大事件数，超出后最旧事件被覆盖 |
| `StoreTTL` | 30 min | 超过此时间的事件在回放时被跳过 |

**工作流程**：
1. 每次 `Publish` 时，事件自动写入 EventStore 并分配 `Seq` 序列号
2. 前端建立/重连 SSE 时，携带 `Last-Event-ID`（即上次收到的 `Seq`）
3. 后端调用 `SubscribeWithReplay(traceID, sinceSeq)`：
   - 先回放 `Seq > sinceSeq` 的历史事件（写锁保护，与实时推送无间隙、无重复）
   - 再转入实时事件推送
4. 事件 JSON 中包含 `seq` 字段，前端保存最新 `seq` 用于下次重连

```go
// 在 SSE handler 中使用
sub := bus.SubscribeWithReplay(workflowID, lastSeq)
// sub.C() 先输出历史事件，再输出实时事件，全程无间隙
```

> `StoreCapacity` 设为 0 可禁用 EventStore（回退为纯 fire-and-forget 模式）。
