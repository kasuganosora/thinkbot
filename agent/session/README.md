# session — 会话串行化运行器

确保同一会话（Scope）内的消息按顺序串行处理，避免并发竞争导致的状态不一致。

## 功能

- **串行化执行**：同一 Scope 的请求排队执行，不同 Scope 并行处理
- **队列管理**：可配置最大队列深度（`MaxQueueDepth`），超限返回 `ErrSessionBusy`
- **超时控制**：支持请求级和全局级超时
- **优雅关闭**：`Close()` 等待所有排队请求完成后退出
- **运行统计**：记录活跃数、排队深度、总执行次数

## 关键类型

| 类型 | 说明 |
|------|------|
| `Runner` | 会话运行器，管理各 Scope 的执行队列 |
| `RunnerConfig` | 运行器配置（MaxQueueDepth/Timeout） |
| `Manager` | 多 Bot 的 Runner 管理器 |

## 使用示例

```go
runner := session.NewRunner(session.RunnerConfig{
    MaxQueueDepth: 10,
}, logger)

err := runner.Run(ctx, func(ctx context.Context) error {
    // 此闭包内对同一 Scope 的操作是串行的
    return processMessage(ctx, msg)
})
```
