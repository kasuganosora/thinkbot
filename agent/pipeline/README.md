# pipeline — 消息处理流水线

可组合、可排序的 Stage 管道框架，支持中间件、谓词过滤和可观测性。

## 功能

- **Stage 注册与排序**：每个 Stage 声明 Order 值，Pipeline 按 Order 升序执行
- **中间件**：在 Stage 执行前后插入横切逻辑（日志/指标/错误恢复）
- **谓词过滤**：基于条件决定是否执行某个 Stage
- **可观测性**：OpenTelemetry 集成，自动追踪 Stage 执行耗时和结果
- **控制流**：Stage 可中止 Pipeline（`env.Abort`）、跳过后续 Stage（`SkipError`）
- **线程安全**：`Envelope` 内部使用 RWMutex 保护共享状态

## 关键类型

| 类型 | 说明 |
|------|------|
| `Pipeline` | 流水线主体，管理 Stage 列表和执行调度 |
| `StageEntry` / `StageInfo` | Stage 注册项 + 元数据 |
| `Middleware` | 中间件函数签名 |
| `Predicate` | Stage 执行条件谓词 |
| `Observable` | 可观测性适配器 |

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
