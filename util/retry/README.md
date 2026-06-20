# util/retry — 指数退避重试工具

提供通用的重试逻辑，支持指数退避、最大重试次数、可取消的 context。

## 核心函数

```go
// 基本重试
err := retry.Do(ctx, func(ctx context.Context) error {
    return someOperation()
}, retry.WithMaxAttempts(5))

// 带指数退避
err := retry.Do(ctx, func(ctx context.Context) error {
    return callAPI()
}, retry.WithMaxAttempts(3), retry.WithInitialDelay(time.Second))
```

## 配置选项

| 选项 | 默认值 | 说明 |
|------|--------|------|
| `WithMaxAttempts(n)` | 3 | 最大尝试次数 |
| `WithInitialDelay(d)` | 1s | 初始退避延迟 |
| `WithMaxDelay(d)` | 30s | 最大退避延迟 |
| `WithMultiplier(f)` | 2.0 | 退避倍率 |
| `WithJitter(b)` | true | 是否添加随机抖动 |

## 设计

- 仅重试可重试错误（`retry.IsRetryable(err)` 判断）
- 指数退避 + 随机抖动避免惊群效应
- 完整的 `context.Context` 支持，超时/取消立即停止
