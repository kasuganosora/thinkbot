# util/retry — 通用重试工具

提供可配置的重试执行器，支持指数/线性/固定退避、Panic 恢复、HTTP 智能重试（Retry-After 头解析）、LLM/流式场景预设。

## 快速开始

```go
import "github.com/kasuganosora/thinkbot/util/retry"

// 最简：默认配置（3 次重试，固定 200ms）
err := retry.DoSimple(ctx, "fetch_user", func(ctx context.Context) error {
    return fetchUser(ctx, uid)
})

// 自定义配置
result := retry.Do(ctx, "call_llm", retry.Config{
    MaxRetries: 5,
    Backoff: &retry.Backoff{
        Strategy: retry.StrategyExponential,
        Initial:  2 * time.Second,
        Factor:   2.0,
        Max:      30 * time.Second,
        Jitter:   true,
    },
    ShouldRetry:   retry.HTTPShouldRetry,
    GetRetryDelay: retry.HTTPGetRetryDelay,
    OnRetry: func(attempt int, err error, wait time.Duration) {
        metrics.RetryCounter.Inc()
    },
}, func(ctx context.Context) error {
    return callLLM(ctx, prompt)
})

if result.Err != nil {
    log.Error("failed after %d attempts", result.Attempts)
}
```

## Config 配置项

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `MaxRetries` | `int` | 3 | 最大重试次数（-1 = 无限，0 = 不重试，N = 最多重试 N 次，总执行 N+1 次） |
| `Backoff` | `*Backoff` | nil | 退避配置，nil 时使用 `FixedInterval` |
| `FixedInterval` | `time.Duration` | 200ms | 无 Backoff 时的固定间隔 |
| `RecoverPanic` | `*bool` | true | 是否自动 recover panic 并视为可重试错误 |
| `ShouldRetry` | `func(int, error) bool` | nil | 判断错误是否可重试（nil = 所有错误都重试） |
| `OnRetry` | `func(int, error, Duration)` | nil | 每次重试前回调 |
| `GetRetryDelay` | `func(error) Duration` | nil | 从错误中提取服务端建议延迟（如 Retry-After），取与退避计算的较大值 |
| `OnPanic` | `func(int, any, []byte)` | nil | Panic 捕获回调（attempt, panic 值, 堆栈） |

## Backoff 退避策略

```go
backoff := retry.Backoff{
    Strategy: retry.StrategyExponential,
    Initial:  1 * time.Second,
    Max:      30 * time.Second,
    Factor:   2.0,
    Jitter:   true,
}
```

| 策略 | 公式 | 适用场景 |
|------|------|---------|
| `StrategyFixed` | `wait = Initial` | 快速失败重试，低延迟场景 |
| `StrategyLinear` | `wait = Initial × attempt` | 逐步增加压力，温和退避 |
| `StrategyExponential` | `wait = Initial × Factor^(attempt-1)` | 网络/LLM API（推荐） |

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `Initial` | 200ms | 初始间隔 |
| `Max` | 30s | 等待上限（封顶值） |
| `Factor` | 2.0 | 指数因子 |
| `Jitter` | false | 随机抖动 ±50%（避免惊群） |

## Result 返回值

```go
type Result struct {
    Err           error         // 最终错误（成功时为 nil）
    Attempts      int           // 总尝试次数（含首次，最小 1）
    Panics        int           // 发生的 panic 次数
    TotalElapsed  time.Duration // 总耗时
}
```

## HTTP 智能重试

### HTTPStatusError

携带 HTTP 状态码、响应头和 Body 的错误类型，供 `ShouldRetry` 和 `GetRetryDelay` 使用。

### HTTPShouldRetry

| 状态码 | 重试 | 说明 |
|--------|------|------|
| 429 | ✓ | 限流 |
| 500/502/503/504 | ✓ | 服务器错误 |
| 529 | ✓ | 过载 |
| 408 | ✓ | 请求超时 |
| 4xx（其他） | ✗ | 客户端错误，不重试 |
| 非 HTTP 错误 | ✓ | 网络超时等，重试 |
| context 取消 | ✗ | 立即停止 |

### HTTPGetRetryDelay

从响应头解析服务端建议的等待时间：

| Header | 格式 | 优先级 |
|--------|------|--------|
| `Retry-After-MS` | 毫秒数 | 最高 |
| `Retry-After` | 秒数 | 正常 |
| `Retry-After` | HTTP-date | 正常 |

返回值与退避计算的值取较大值。

### DoHTTPRequest

```go
resp, err := retry.DoHTTPRequest(ctx, client, req, cfg)
```

一行完成 HTTP 请求 + 自动重试。内部克隆请求避免 Body 复用问题。

## 预设配置

| 函数 | 重试次数 | 退避 | 说明 |
|------|---------|------|------|
| `DefaultConfig()` | 3 | 固定 200ms | 通用默认 |
| `DefaultLLMRetryConfig()` | 3 | 指数 2s→30s + Jitter | LLM API |
| `LLMRetryConfig(n)` | n | 指数 2s→30s + Jitter | 自定义次数 LLM |
| `AggressiveRetryConfig()` | 5 | 指数 2s→60s + Jitter | 高负载 LLM |
| `StreamingRetryConfig(n)` | n | 指数 1s→20s + Jitter | 流式连接 |

## Panic 恢复

- 默认开启（`RecoverPanic = true`）
- 捕获 panic 后转为 `panicError` 继续重试流程
- 可通过 `OnPanic` 回调获取堆栈
- 关闭后（`RecoverPanic = false`）直接 re-panic

## Context 取消行为

| 时机 | 行为 |
|------|------|
| `fn` 执行期间 ctx 取消 | fn 返回后立即停止，返回 `ctx.Err()` + last error |
| 退避等待期间 ctx 取消 | 立即唤醒，返回 `ctx.Err()` |

## 文件结构

| 文件 | 职责 |
|------|------|
| `retry.go` | `Config` / `Result` / `Do` / `DoSimple` / panic 恢复 / 执行循环 |
| `backoff.go` | `Backoff` 类型 / 三种退避策略 / `Calc` 计算 |
| `http_retry.go` | `HTTPStatusError` / `HTTPShouldRetry` / `HTTPGetRetryDelay` / 预设配置 / `DoHTTPRequest` |
