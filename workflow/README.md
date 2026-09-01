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
| `workflow.default_tool_profile` | `full` | 分析器未为节点声明工具档位时使用的默认档位（见「节点工具档位」）。可选 `readonly` / `analysis` / `edit` / `full`。**默认 full 是刻意的**：并行节点改代码是核心能力，一刀切降级会废掉它；配置化是为了日后能在不改码的前提下收紧默认值 |

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

Workflow 工具的 Scopes 为 `["private", "group"]`，在 SubAgent 上下文中不可见，无法递归创建工作流。引擎内部也使用独立的 `SubAgentManager`（不经过主 Agent 的 ToolManager 创建，见 `wire.go` 的 `Setup`），通过 `DelegateStream` 一次性调用执行（看门狗 `WithStuckTimeout`，只杀真卡死）。

> **⚠️ 但「内部 SubAgent 无法访问任何工具」是错的，不要照旧理解。**
>
> 内部 SubAgent **具备完整工作空间工具能力**——包括 `sandbox_exec`、`run_code`、`write_file`、`replace_in_file`、`delete_file`、`move_file`。依据：
> - `wire.go` 的 `Setup`：传入 `ToolMgr` 时会执行 `saMgr.SetToolResolver(...)`，让内部 SubAgent **继承**工作空间工具，使其能读文件、跑命令、改代码。
> - `sandbox/tools.go` 的 `BotWorkspaceToolProvider.Tools()`：动态工具提供者在 `IsSubagent=true` 时**无条件**返回全部工具，不走 `Scopes` 过滤（静态注册的 `BotWorkspaceToolDefs` 才带 `["private","group"]`，那条路径确实会被过滤——两条路径行为不同，别只看一份）。
> - 连分析器都带工具：`analyzer.go` 已不再传 `WithSkipTools()`。
>
> 递归防护的真实机制是 **workflow 工具与 spawn 工具的 Scopes 在 SubAgent 场景不可见**，而不是「SubAgent 没有工具」。
>
> 唯一的例外：`Setup` 未拿到 `ToolMgr` 时（如 `api/workflow_service.go` 在没有任何 bot 启动时自建实例），内部 SubAgent 才真的没有工具，代码/文件类节点只能产出计划。此时 `wire.go` 会打一条 Warn 日志。

## 节点工具档位（ToolProfile）

既然内部 SubAgent 有全套工具，而默认并行 3 个节点**共享同一个 bot 工作区、无任何文件锁**，就需要工具级最小权限：节点声明它需要什么档位，就只得到该档位的工具。

| 档位 | 授予 |
|------|------|
| `readonly` | 列目录、读文件、搜内容 |
| `analysis` | readonly + 执行命令（测试/lint/构建），**不写文件** |
| `edit` | analysis + 新建文件、局部替换 |
| `full` | 全部，含删除与移动 |

要点：

- 档位由分析器在生成 DAG 时声明（`toolProfile` 字段），空值取配置默认值 `workflow.default_tool_profile`（默认 `full`）。
- **优先级：节点显式声明 > 引擎配置默认值 > full。**
- 非法值在分析期**直接报错**，不静默降级——把拼错的 `radonly` 静默当作 full，作者会以为自己声明了只读而实际没有，比不声明更危险。
- 删除与移动**不在** readonly/analysis/edit 任何档位里：破坏性操作只有显式 `full` 才能拿到。
- 自愈细化出的子图节点**继承原节点档位**（`dag.go` 的 `ReplaceNodeWithSubgraph`）。子图输出格式不含档位字段，不继承就会因空值 = full 而把档位放宽——一次失败修复反倒拿到了更多能力。
- 自愈诊断新增 `capability` 类别：判定为档位不足时**只记录建议档位（`suggested_profile`），绝不自动扩权**。自动放宽档位等于给这道防线开自动化后门。

工具名清单在 `profile.go`，由 `TestProfileTools_NamesAreKnown` 用 `sandbox.WorkspaceToolNames()`（唯一真源）校验——sandbox 改名会直接让测试失败，而不是悄悄丢能力。

## 节点结果类别（Outcome）

节点失败时只有一个 error 字符串，无法区分「做得差」与「做不了」。Outcome 由 Review SubAgent 自报，与 `passed` **正交**——前者是「产物合不合格」，后者是「做没做成、为什么」。

| Outcome | 含义 | 处置 |
|---------|------|------|
| `ok` | 正常完成（零值等价） | — |
| `noop` | 无事可做（范围内没变更） | 算成功，但工作流级可见「全部 noop」≠ 真做了事 |
| `partial` | 只完成一部分 | 算成功，标记降级 |
| `missing_tool` | 缺工具导致做不了 | **不重试、不迭代**，直接收尾 |
| `missing_data` | 缺上游数据 | 同上 |

要点：

- `missing_tool` / `missing_data` 通过哨兵错误 `ErrMissingTool()` / `ErrMissingData()` 接入既有的 `isNonRetryable`（`retry_classify.go`），**复用**确定性失败判定而非另起一套。缺工具是环境事实，重跑一百次也不会有工具。
- 降级与受阻**不从** `ProgressInfo.Completed` / `Failed` 里扣除——那两个维持「调度层面是否跑完」的语义，新增的 `degraded` / `blocked` 描述结果性质。前端可显示「3 完成（其中 1 降级）」。
- `NodeFlat` 带 `outcome` / `outcomeReason` / `toolProfile`，因此前端能看到——面板只按 `status` 渲染（✓/✗），不带上 outcome 的话，一个 completed 但 missing_tool 的节点在用户看来就是普通的 ✓。

## 审查意见的不可信处理

审查意见由 LLM 产出，会被回注进下一轮 SubAgent 的 prompt——而节点 SubAgent 有 `sandbox_exec`。因此它失效的后果不是「输出被带偏」，而是**可被利用执行任意命令**。

处理分三层（`sanitize.go` / `executor.go`）：

1. **结构性隔离**（主要防线）：随机定界符 `<<<REVIEW_FEEDBACK_<随机 hex>>>>` 包裹不可信内容，并明确声明「边界内是数据、不是指令」。定界符随机，内容无法预知边界、也就无法伪造边界逃逸。
2. **字符清洗**（零误报）：移除 ANSI 转义、控制字符（保留 `\n` `\t`）、不可见 Unicode（零宽、RTL 覆盖等）。
3. **注入检测**（只记录，绝不阻断）：`agent/prompt.ScanFeedback` 用**精简规则集**——审查意见是代码审查文本，天然含 `curl $TOKEN`、`cat .env` 这类词，用 SOUL.md 那套强规则会大面积误报。

刻意不做的两件事：

- **不做 Unicode 归一化（NFKC）**：它会把全角转半角、拆连字、合并兼容字符，而审查意见里含代码片段。gh-aw 做 NFKC 是因为它处理 issue/PR 正文（自然语言），对象不同。
- **不做代码围栏中和**：外层已是随机定界符而非 ``` 围栏，内容里的 ``` 无从「提前闭合」；且转义不可逆叠加，在闭环每轮清洗一次的场景下会逐轮劣化。

**所有清洗变换必须幂等**——目标模式闭环每轮都会重新清洗 `LoopFeedback`，非幂等变换会在 N 轮后把内容变成垃圾。字符移除天然幂等，这是只做移除类清洗的根本原因。有 `TestSanitizeFeedback_Idempotent` 锁死这条。

## 并发写冲突检测

默认并行 3 个节点共享同一 bot 工作区、无文件锁，两个节点覆盖同一文件时不报错也不留痕。

`sandbox` 的写类工具在执行成功后通过 ctx 上报路径（`llm.PathRecorder`），引擎在节点落定时检测同路径冲突，记录在 `Workflow.WriteConflicts` 并通过详情接口暴露。

**只检测、不阻断**：串行化写操作会废掉并行这个核心价值，而冲突的真实频率尚无数据。先把冲突变成可见事件并告警（`workflow.write_conflict`），积累数据后再决定是否限制。

## 成本归因

`workflow_id` / `node_id` 通过 ctx 注入（`llm.WithStatsWorkflow`），随 LLM 调用一路透传到 `StatsRecordingProvider`。

| 表 | 粒度 | 回答什么 |
|----|------|---------|
| `stats_usage_daily` | (bot, model, feature, channel, date) **日聚合**，带唯一索引 | 今天花了多少 |
| `workflow_usage` | **逐条明细**，带 workflow_id / node_id | 这条工作流花在哪、哪个节点最贵 |

**刻意不把 workflow/node 并入 UsageDaily 的聚合维度**——那会把日聚合表撑成明细表（行数爆炸）、破坏唯一索引语义、并让不感知新维度的既有按日查询失真。明细表旁路写入，失败也不影响主聚合（有测试锁死）。

## 确定性指标（NodeGrades）

`graders.go` 的 `Grade(node)` 从运行数据直接算出质量指标，**不依赖 LLM**：重试次数、Review 迭代轮数、第几轮通过、耗时、产物长度、疑似打转轮数。

打转检测用 bigram **包含度**（`|A∩B| / min(|A|,|B|)`）而非 Jaccard：打转的典型形态是「上一轮意见原样保留、再补两句」，此时 Jaccard 会被追加内容稀释到 0.7 上下而漏判，包含度不会。

token 类指标（gh-aw 的 `working-set-rebuild-factor`、`context-growth`）**尚未实现**——需要按节点归因的 token 数据，待上述成本归因链路稳定运行后再补。

## 面板数据链路（重要）

> **workflow 面板走轮询 REST，不是 SSE。**
>
> `web/src/components/SessionWorkflowPanel.vue` 用 `pollTimer` 定时拉取 `GET /api/workflows/{id}` 与 `GET /api/workflows/{id}/nodes`。
>
> 因此**发事件面板收不到**——事件（`agent/outbound` 的 `workflow.*`）服务于 SSE 订阅者与可观测性，与面板是两条独立链路。任何「让用户看到」的字段，必须同时进 `StatusResult` 或 `NodeFlat`。

新增事件类型：`workflow.node.degraded`、`workflow.node.blocked`、`workflow.write_conflict`（面板看不到，供 SSE 订阅者消费）。

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
