# pipeline — 消息处理流水线

可组合、可排序的 Stage 管道框架，支持中间件、条件路由和可观测性。

## 功能

- **Stage 注册与排序**：每个 `core.StageInfo` 声明 `Order`，Pipeline 按 Order 升序执行，`Enabled=false` 时跳过
- **中间件**：`Middleware` 包装 Stage，在执行前后插入横切逻辑
- **条件路由**：`Router` 本身是一个 Stage，按 `Predicate` 分发到不同 Stage 子链
- **可观测性**：OpenTelemetry span + metrics（processed / errors / dropped / stage duration）
- **控制流**：`env.Abort()` 中止、`core.SkipError` 跳过当前 Stage、返回 `nil` Envelope 表示丢弃
- **panic 恢复**：`Pipeline.executeStage` 内置 recover，转换为 `core.PipelineError` 并记录栈
- **fx 集成**：`Module` 从 `group:"pipeline_stages"` 收集 Stage

## 关键类型

| 类型 | 说明 |
|------|------|
| `Pipeline` | 流水线主体，`New(stages, tp, mp, logger)` 构造，`Execute` 执行 |
| `Middleware` | `func(next core.Stage) core.Stage`，用 `WithMiddleware` 组合 |
| `Predicate` / `PredicateFunc` | `core.Predicate` 的类型别名（向后兼容） |
| `Route` / `Router` | 条件路由规则与分发 Stage |
| `Builder` / `PipelineMode` / `StageGroup` | 声明式 Stage 装配器（`Add` / `AddIf` / `WithMode` / `Build`）与模式门控词汇（`ModeGroups`） |
| `ObservableStage` | 发射 `stage.enter` / `stage.exit` / `stage.skip` 事件的包装器 |
| `Instruments` | Pipeline 所需 OTel 仪器集合 |
| `LoopDetectionConfig` | 循环检测配置（滑窗 + tool-call digest 重复检测） |
| `TokenBudgetConfig` / `TokenBudgetState` | 按 Channel 累计 Token 用量 + 阈值控制 |
| `QuotaResolver` / `TokenQuotaState` | 月度 Token 配额层级解析与计数 |
| `LazyResponseConfig` | 「不调工具直接下结论」的事后兜底检测 |
| `VerificationGateConfig` | 环境类问题的事前强制工具调用门禁 |

## Pipeline 执行语义

| Stage 返回 | Pipeline 行为 |
|------------|---------------|
| `nil` Envelope | 消息被丢弃，Pipeline 终止并返回 `(nil, nil)` |
| `core.AbortError` | 立即终止，错误返回给调用方 |
| `core.SkipError` | 跳过该 Stage，继续下一个 |
| 其他 `error` | 记录 warn 日志后**继续**执行 |
| `env.Aborted() == true` | 下一轮循环开始时终止 |

辅助方法：`StageNames()`（已启用 Stage 名，按执行顺序）、`Len()`（含未启用的总数）、
`SetSink(core.EventSink)`（注入 append-only 事件轨迹接收器，nil 恢复 NoopSink；
sink 会通过 `core.WithEventSink` 注入 ctx 供下游 Stage / 工具循环追加事件）。

## 使用示例

### 直接构造

```go
p, err := pipeline.New([]core.StageInfo{
    {Stage: loggerStage, Order: 10, Enabled: true},
    {Stage: llmStage, Order: 100, Enabled: true},
}, tp, mp, logger)

result, err := p.Execute(ctx, env)
```

### fx 注入

```go
fx.Options(
    pipeline.Module,
    pipeline.ProvideStage(stages.NewLoggerStage, 10),   // order=10
    pipeline.ProvideStage(stages.NewLLMStage, 100),     // order=100
    pipeline.ProvideStageInfo(newCustomStageInfo),      // 自定义 Order/Enabled
)
```

`Module` 在上层未提供时会补充 OTel NoOp `TracerProvider` / `MeterProvider`。

## 通用中间件

```go
stage = pipeline.WithMiddleware(stage,
    pipeline.RecoveryMiddleware(),          // panic → PipelineError
    pipeline.TimeoutMiddleware(30*time.Second),
    pipeline.LoggingMiddleware(logger),
)
```

`WithMiddleware` 从后往前包装，第一个 Middleware 位于最外层、最先执行。

## 条件路由

```go
router := pipeline.NewRouter("dispatch",
    pipeline.Route{
        Name:      "command",
        Predicate: &pipeline.TextHasPrefix{Prefix: "/"},
        Stages:    []core.Stage{commandStage},
    },
    pipeline.Route{Name: "default", Stages: []core.Stage{llmStage}, Fallback: true},
)
```

内置谓词：`TextContains`、`TextHasPrefix`、`TextRegex`、`SourceEquals`、`ChannelEquals`、
`MetadataExists`、`MetadataEquals`、`ValueExists`；
组合谓词：`And` / `Or` / `Not`；
便捷构造：`MatchAll`、`MatchNone`、`MatchTextContains`、`MatchSource`、`MatchChannel`。

## 内建中间件

### 循环检测（`loop_detection.go`）

对每次 LLM 产出的工具调用列表做稳定 hash（tool_name + args 排序后 sha256），
按 Channel 维护滑动窗口统计重复次数。

```go
cfg := pipeline.NewLoopDetectionConfig(). // 默认 warn=3 / hard=5 / window=20
    WithWarnThreshold(3).                 // 重复 3 次 → 软警告
    WithHardLimit(5).                     // 重复 5 次 → 硬警告（每 channel 仅一次）
    WithWindowSize(20).
    WithExemptTools("task_status")        // 轮询类工具不计入检测

llmStage = pipeline.LoopDetectionMiddleware(cfg)(llmStage)
```

从 Envelope KV 的 `llm.result`（`*llm.GenerateResult`）读取工具调用；
配置为零值时中间件退化为透传。

### Token 预算（`token_budget.go`）

按 Channel 跨消息累计 Token 用量。

```go
cfg := pipeline.NewTokenBudgetConfig(). // 默认 100k / 80% / 100%
    WithMaxTokens(100_000).
    WithWarnPercent(0.8).               // 80% → 软警告（每 channel 仅一次）
    WithHardPercent(1.0).
    WithStatsRecorder(recorder)         // 可选，记录 budget_warning 事件

llmStage = pipeline.TokenBudgetMiddleware(cfg)(llmStage)

// 跨 Pipeline 共享状态（支持空闲 TTL 自动清零 + 手动重置）
state := pipeline.NewTokenBudgetState(time.Hour)
llmStage = pipeline.TokenBudgetMiddlewareWithState(cfg, state)(llmStage)
```

行为要点：
- 达到硬限制时**重置该 Channel 的计数窗口并继续处理本条消息**（不永久中止，避免活跃会话被卡死）
- 达到硬限制的 90% 时注入硬警告，要求 LLM 立即收尾
- `TokenBudgetState` 提供 `Snapshot(channel)` / `ResetChannel` / `ResetAll`

### Token 月度配额（`token_quota.go`）

按 chat → channel → bot → system 四级继承解析限额，超额时返回 `core.PipelineError` 拦截。

```go
resolver := pipeline.NewQuotaResolver(store)  // store 实现 GetInt64
state := pipeline.NewTokenQuotaState().WithStatsRecorder(recorder)

// 共享 State（推荐，配合 llm.QuotaRecordingProvider 全链路记账）
llmStage = pipeline.TokenQuotaMiddlewareWithState(resolver, state, tp, logger)(llmStage)

// 或独立中间件（内部自建 State）
llmStage = pipeline.TokenQuotaMiddleware(resolver, tp, logger)(llmStage)
```

**Dimension 格式**：`bot:<botID>:chat:<channelType>:<chatID>`、`bot:<botID>:channel:<channelType>`、
`bot:<botID>`、`system`，确保不同 Bot 之间计数器隔离。
`channelType` 取自 `Metadata["channel_type"]`，缺省回退到 `Message.Source`；
`chatID` 取自 `Metadata["chat_id"]`。

**全链路记账**：中间件在检查通过后用 `llm.WithQuotaDimension(ctx, dim)` 注入 context，
使嵌套 LLM 调用（SubAgent、Workflow、Memory 等）通过 `QuotaRecordingProvider` 自动记账。

**State API**：`Usage(dim)`、`AddUsage(dim, tokens)`、`Snapshot()`、`Reset(dim)`、
`RestoreFromStats(ctx, db, botIDs...)`（重启后从 `stats_usage_daily` 恢复本月 bot 级用量）。
计数器按 UTC 月份自动跨月清零。

### 防偷懒：事前门禁 + 事后兜底

两者配合形成防御纵深，抑制「不调工具就编造环境状态」的行为。

```go
// 事前：命中环境类问题时在 Envelope 上标记 verify.required
llmStage = pipeline.VerificationGateMiddleware(pipeline.NewVerificationGateConfig())(llmStage)

// 事后：无 tool_calls 且文本命中偷懒模式时注入硬警告并同轮 loop-back 重算
llmStage = pipeline.LazyResponseMiddleware(pipeline.NewLazyResponseConfig())(llmStage)
```

- `VerificationGateMiddleware` 只负责打标记，真正的 `tool_choice=required`
  由 `agent/stages/llmroute.go` 读取该标记后落地。
- `LazyResponseMiddleware` 每 Channel 首次命中才触发；模型当轮调用了工具则复位标记。

## 可观测性

```go
// 单个包装
wrapped := pipeline.NewObservableStage(myStage)

// 批量包装（已是 ObservableStage 的不重复包装）
stages = pipeline.WrapWithObservability(stages)
```

`ObservableStage` 通过 `outbound.EmitterFromContext(ctx)` 获取 EventEmitter，
发射 `stage.enter` / `stage.exit`（含耗时和错误）/ `stage.skip` 事件；
context 中无 emitter 时静默退化为直接调用内部 Stage。

Pipeline 自身上报的 metrics：`pipeline.messages.processed`、`pipeline.messages.errors`、
`pipeline.messages.dropped`、`pipeline.stage.duration_seconds`（`stage` 标签，
整条流水线耗时记为 `_pipeline_total`）。
