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
2. **Compile** — 编译工作流图：校验 DAG 完整性（无环、无悬空引用）+ 计算拓扑排序 + 构建邻接表和入度缓存
3. **Scheduler** — 按拓扑序调度，同层无依赖节点并行执行（semaphore 限流）；下游节点自动注入上游产物上下文
4. **Executor** — 每个节点由独立 SubAgent 执行；`review=true` 的节点执行后启动 Review 自循环
5. **提交即阻塞** — `task` 调用在服务端等待工作流进入终态才返回最终状态与进度，LLM 无需轮询

## 架构

| 组件 | 文件 | 职责 |
|------|------|------|
| `Manager` | `manager.go` | 统一入口：Submit / GetStatus / ListNodes / Control / Recover；另有 StartSweeper（过期清理）/ StartQuotaWatch（429 配额暂停与到点续跑看门狗） |
| `Analyzer` | `analyzer.go` | LLM 需求分析 + DAG 生成 |
| `Compile` | `dag.go` | DAG 编译：校验 + 拓扑排序 + 邻接表/入度缓存 + 上游结果聚合 |
| `Scheduler` | `scheduler.go` | DAG 拓扑调度、并行限流、重试/Review 循环、级联跳过、上游结果注入 |
| `Executor` | `executor.go` | 节点执行（SubAgent `DelegateStream`）、Review 审查、带反馈迭代 |
| `DAG` | `dag.go` | 纯领域算法：校验、环检测、就绪节点计算、级联跳过、树构建 |
| `Repository` | `repository.go` | 内存优先 + DB 双写持久化 |
| `Models` | `models.go` | GORM 模型（JSON 全量序列化策略） |
| `Wire` | `wire.go` | 组合根：`Setup()` 统一装配 |
| `Tools` | `tools.go` | 暴露给主 Agent 的 LLM 工具 |
| `Types` | `types.go` | 领域模型、枚举、视图结构 |
| `Intent` | `intent.go` | `DetectGoalModeIntent` 识别「反复打磨直到达标」类收敛性意图，命中则强制 goalMode |
| `ReviewError` | `review_error.go` | `isReviewInfraError` 区分基础设施错误与业务判定 |
| `StatusWait` | `status_wait.go` | `waitForTerminal` 服务端阻塞等待（task 提交即阻塞的底层） |
| `QuotaBreak` | `quota_break.go` | 429 配额熔断：暂停调度、标记 `interrupted` 等待到点续跑 |

## LLM 工具

引擎向主 Agent 注册 3 个工具：

| 工具 | 说明 |
|------|------|
| `task` | 提交需求并**阻塞**到工作流进入终态（completed/failed/terminated）才返回最终状态与进度，LLM 无需也不存在单独的轮询调用。支持 `goalMode` 目标模式（闭环迭代直到审查通过）与 `maxParallel` 并发度覆盖；阻塞等待上限 `taskBlockingMaxTimeout=18m`，超时返回 `timedOut: true`（工作流仍在后台运行，可用 `task_detail` 查看进度，**不要**重复提交 `task`） |
| `task_detail` | 查询子任务列表（`flat` 平铺 / `tree` 树状）；开启目标模式时额外返回 `goalMode`/`goalIteration`/`goalMaxIterations` |
| `task_control` | 控制操作：对 **运行中** 工作流重试指定失败节点 / 终止（`analyzing` 阶段禁止终止，会被拒绝）；对 **已终态** 工作流通过 `ActionRetry` 从指定节点重新拉起调度（`restartFromNode`） |

> 工具命名与主流 LLM 预训练中的 agentic 工具名（如 Claude 的 Task、LangChain 的 TaskTool）对齐，降低 LLM 适配成本。旧的 `task_status(wait: true)` 轮询工具已移除。

## 提交即阻塞（task 的服务端等待）

`task` 是阻塞式工具：提交后在引擎侧 `waitForTerminal` 内持续轮询（复用原 `task_status(wait:true)` 的服务端等待逻辑），直到工作流进入 `completed` / `failed` / `terminated` 才返回，避免 LLM 用 `sleep` 反复轮询：

- 阻塞等待上限 `taskBlockingMaxTimeout = 18m`（必须小于 api/handler_chat.go 后台落库的 20min bgCtx 上限，否则最终回复无法落库）。
- 超时返回 `TimedOut: true` 且携带最新快照，而非报错；工作流仍在后台运行——**不要**再调 `task`（那会提交新任务），用 `task_detail` 查看进度或直接告知用户任务仍在进行。
- 内部轮询间隔 `statusWaitPollInterval = 3s`，并响应 `ctx` 取消；等待期间通过 `ctx.SendProgress` 推送进度（payload 带 `workflowId`，前端工作流面板靠它挂载）。

判断口诀：**想知道结果 → 一次 `task` 调用 → 返回即终态再继续**；`task_detail` 仅用于「事后查看各子任务明细」。

## 并行 DAG（默认并行）

Analyzer 的系统提示词明确要求：**默认让子任务并行**，仅当存在真实数据依赖（如「B 依赖 A 的输出」）才建 `dependencies`。因此 Scheduler 按编译后的拓扑序调度，所有无依赖关系的节点并发执行（受 `MaxParallel` 信号量限流），而非串行。好的需求拆分应尽量减少依赖边，以最大化并行度。

## 图编译 (Compile)

Analyzer 生成 DAG 节点后、Scheduler 运行前，`Workflow.Compile()` 统一执行：

- **DAG 校验** — 无环检测、ID 唯一性、依赖无悬空引用
- **拓扑排序** — Kahn 算法计算节点执行顺序缓存
- **邻接索引** — 构建反向邻接表（nodeID → 下游节点列表）、入度缓存、根节点列表

编译后 `ReadyNodes` / `CascadeSkip` / `BuildTree` 复用预计算索引，将 O(n×deps) 查询降为 O(n)。

```go
wf := NewWorkflow("wf", "req", nodes)
if err := wf.Compile(); err != nil {
    // DAG 校验失败 → 标记为 WorkflowFailed
}
// wf.Compiled() == true, 后续 ReadyNodes 走快速路径
```

崩溃恢复路径中也会自动重新编译从 DB 反序列化的 Workflow。

## 上游结果注入

Scheduler 在执行节点前，自动聚合已完成依赖节点的产物，注入为 SubAgent 输入前缀：

```
[上游任务汇总]
n1(提取Q1数据): 营收增长12%...
n2(市场分析): 份额+3%...

[你的任务]
综合以上数据写投资报告...
```

- 未完成的依赖自动排除（运行时自然为空）
- 单结果 >4000 字符截断，总上下文 >8000 字符省略
- 对 LLM 零感知 — Analyzer prompt 不需要改动，依赖关系语义自然传递

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

## 从终态重试（restartFromNode）

`task_control` 的 `ActionRetry` 对**已终态**（`completed`/`failed`/`terminated`）的工作流同样可用：

- 当工作流处于终态时，`Control` 会调用 `restartFromNode(wf, nodeID)` 重新拉起调度。
- 目标节点的状态被重置为 `pending`，其因终态而被级联 `skipped` 的下游节点一并恢复为 `pending`，并重建运行实例；其他已完成节点状态保留。
- 这使得「修复后从某一节点续跑」成为可能，无需从头重跑整个 DAG。

运行中（`running`/`interrupted`）工作流的重试则交由活跃 Scheduler 就地处理，不触发 `restartFromNode`。

## Review 错误分类（基础设施 vs 业务）

Review 阶段会区分两类错误，决定重试还是失败迭代：

- **基础设施错误（可重试）**：网络/服务抖动类，如 `context deadline exceeded`、超时、`connection refused`、502/503/504、限流等。由 `isReviewInfraError` 判定，`context.Canceled` 不算基础设施错误。这类错误触发 `reviewWithInfraRetry` 就地重试（最多 `reviewInfraMaxAttempts=3`，间隔 `reviewInfraRetryBaseDelay=2s`）。
- **业务判定（失败迭代）**：模型正常给出了「不通过」结论（只是任务没达标）。这类按节点 `max_iterations` 走 Review 自循环带反馈重执行；目标模式下回退到 `Feedback` 目标节点形成「工作→审查→修复→审查」闭环，直到 `goal_max_iterations` 上限。

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
| `workflow.analyzer_stuck_timeout` | `180` | 分析器（流式 LLM）卡死看门狗阈值（秒）。连续无 token 超过该时长判卡死并终止；硬上限 = 该值 × 3 |
| `workflow.analyzer_max_duration_ms` | `600000` | 分析阶段整轮总时长上限（毫秒，即 10 分钟）。兜底防止分析器无限重试把「分析中」拖成黑洞；超时则分析阶段整体失败并报错 |
| `workflow.goal_max_iterations` | `5` | 目标模式（闭环循环）全局最大迭代轮数；达到上限仍不通过则工作流失败 |

> 分析器 LLM 的 `max_tokens` **不再由独立配置键控制**（`workflow.analyzer_max_tokens` 已移除），改为跟随所用模型的 `MaxTokens` 能力（如模型未显式给出则代码兜底，见 `wire.go` 的 `analyzerMaxTokens`）。

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

Workflow 工具的 Scopes 为 `["private", "group"]`，在 SubAgent 上下文中不可见，无法递归创建工作流。此外，引擎内部使用独立的 `SubAgentManager`，通过 `DelegateStream` 一次性调用执行（看门狗 `WithStuckTimeout`，只杀真卡死），不经过主 Agent 的 ToolManager，无法访问任何工具。

## 持久化

采用 JSON 全量序列化策略：整个 `Workflow` 对象序列化为 `WorkflowModel.Data` 字段。读操作优先从内存缓存获取（O(1)），写操作同时更新内存和 DB。表名 `workflow_workflows`。

> **两个 Manager 实例 + 引擎复用**：`api/botservice.go` 在每 bot 启动时构建带工作区工具（`ToolMgr`）的引擎并发布；`api/workflow_service.go` 优先复用 BotService 已装配的引擎（不缓存，避免持有关闭的引擎），仅在没有任何 bot 启动时退化为自建实例（此时工作流 SubAgent 拿不到工作区工具，代码/文件类节点只能产出计划）。因此 `Repository.Get(id)` 即便命中内存缓存，仍会用 `updated_at` 与 DB 比对新鲜度；DB 不可用时退回内存缓存快照，避免读到已被其他实例改写的陈旧数据。写操作 `Save` 存入的是工作流的深拷贝快照；缓存上限 `maxCacheSize=500`，终态工作流会被优先淘汰。

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

## 配额熔断与到点续跑（quota_break）

上游 LLM 配额耗尽（HTTP 429 / body code 1308 等）时不再逐节点撞墙重试，而是三级联动：HTTP 流式层与节点层识别配额错误后立即放弃重试；工作流层**首个**配额失败即熔断整条工作流（`tripQuotaBreak`）：

- 配额失败的节点退回 `pending`（不记 `failed`、不级联跳过），已有成果保留；
- 工作流置 `interrupted` 并记录 `QuotaResumeAt`（服务端告知的重置时刻；未知时兜底 `defaultQuotaWait=15m`，解析异常封顶 `hardQuotaWaitCap=12h`）；
- `Manager.StartQuotaWatch` 看门狗到点自动 `ResumeQuotaInterrupted` 续跑；`SweepStale`/`Recover` 对仍在等待窗口内的工作流跳过，交给看门狗接管；
- 累计熔断超过 `maxQuotaBreaks=5` 次按失败收尾。

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
| `workflow.running` | 工作流开始执行调度 | `node_count`, `max_parallel` |
| `workflow.completed` | 工作流全部成功 | `node_count` |
| `workflow.failed` | 工作流失败 | `error` |
| `workflow.terminated` | 工作流被终止 | — |
| `workflow.node.started` | 节点开始执行 | `node_id`, `node_name`, `task` |
| `workflow.node.completed` | 节点完成 | `node_id`, `retry_count`, `iteration_count`, `result_preview` |
| `workflow.node.failed` | 节点失败 | `node_id`, `retry_count`, `error` |
| `workflow.node.reviewing` | 节点进入 Review | `node_id`, `iteration` |
| `workflow.node.retrying` | 节点执行重试 | `node_id`, `attempt`, `max_retries`, `error` |
| `workflow.node.skipped` | 节点被级联跳过 | `caused_by`, `skipped_ids` |

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
