# pipeline — 消息处理流水线

可组合、可排序的 Stage 管道框架，支持中间件、谓词过滤和可观测性。

## 功能

- **Stage 注册与排序**：每个 Stage 声明 Order 值，Pipeline 按 Order 升序执行
- **中间件**：在 Stage 执行前后插入横切逻辑（日志/指标/错误恢复）
- **谓词过滤**：基于条件决定是否执行某个 Stage
- **可观测性**：OpenTelemetry 集成，自动追踪 Stage 执行耗时和结果
- **控制流**：Stage 可中止 Pipeline（`env.Abort`）、跳过后续 Stage（`SkipError`）
- **线程安全**：`Envelope` 内部使用 RWMutex 保护共享状态
- **循环检测中间件**（`loop_detection.go`）：检测重复工具调用模式，注入软/硬警告
- **Token 预算中间件**（`token_budget.go`）：按 Channel 追踪累计 Token 用量，阈值告警和硬限制
- **运行日志中间件**（`run_journal.go`）：异步缓冲记录 LLM 用量到 DB，通过 context 注入追踪元数据

## 关键类型

| 类型 | 说明 |
|------|------|
| `Pipeline` | 流水线主体，管理 Stage 列表和执行调度 |
| `StageEntry` / `StageInfo` | Stage 注册项 + 元数据 |
| `Middleware` | 中间件函数签名 |
| `Predicate` | Stage 执行条件谓词 |
| `Observable` | 可观测性适配器 |
| `LoopDetectionMiddleware` | 循环检测中间件（滑窗 + digest 重复检测） |
| `TokenBudgetMiddleware` | Token 预算中间件（按 Channel 累计用量 + 阈值控制） |
| `RunJournalRecorder` | 运行日志记录器（异步缓冲 + DB 持久化 + context 元数据注入） |

## 使用示例

```go
p := pipeline.New("main", logger, tp)
p.Use(pipeline.LoggingMiddleware(logger))
p.Register(pipeline.StageEntry{
    Stage: pipeline.StageFunc(func(ctx context.Context, env *core.Envelope) error {
        env.AddAction(core.Action{Type: core.ActionReply, Payload: "Hi"})
        return nil
    }),
    Info: core.StageInfo{Name: "greet", Order: 100, Enabled: true},
})
p.Execute(ctx, envelope)
```

## 内建中间件

### 循环检测

```go
cfg := NewLoopDetectionConfig().
    WithWindow(5).           // 滑动窗口大小
    WithSoftThreshold(0.6).  // 60% 重复率 → 软警告
    WithHardThreshold(0.8)   // 80% 重复率 → 硬警告

mw := LoopDetectionMiddleware(cfg)
llmStage = mw(llmStage)
```

### Token 预算

```go
cfg := NewTokenBudgetConfig().
    WithMaxTokens(100000).   // 每 Channel 累计上限
    WithWarnPercent(0.8).    // 80% 时软警告
    WithHardPercent(0.95)    // 95% 时硬错误（PipelineError）

mw := TokenBudgetMiddleware(cfg)
llmStage = mw(llmStage)
```

### 运行日志

```go
recorder := NewRunJournalRecorder(db, DefaultRunJournalConfig())
defer recorder.Shutdown()

// 作为 UsageRecorder 传给 LLMStage
llmStage := stages.NewLLMStage("llm", provider, stages.LLMConfig{
    UsageRecorder: recorder,
})

// 用中间件注入 trace_id / message_id / channel / user_id
mw := recorder.Middleware()
llmStage = mw(llmStage)
```
