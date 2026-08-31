# util/traceid — 全链路追踪 ID

提供 128-bit 随机 Trace ID 的生成、context 传播、OTel 桥接、日志集成和 HTTP 中间件。

## 常量

| 常量 | 值 | 说明 |
|------|-----|------|
| `HeaderKey` | `"X-Trace-ID"` | HTTP 请求/响应头名称 |
| `LogField` | `"trace_id"` | 日志字段名 |
| `IDLength` | `16`（字节） | Trace ID 长度（128-bit，hex 编码后 32 字符） |

## 快速开始

```go
import "github.com/kasuganosora/thinkbot/util/traceid"

// 生成新 Trace ID
traceID := traceid.New() // → "4bf92f3577b34da6a3ce929d0e0e4736"

// 注入 context
ctx = traceid.WithTraceID(ctx, traceID)

// 从 context 提取
id := traceid.FromContext(ctx)

// 快速获取带 trace_id 的 Logger
traceid.L(ctx).Infow("processing", "user_id", uid)
// 日志输出自动包含 trace_id=4bf92f3577b34da6a3ce929d0e0e4736
```

## Trace ID 生成

```go
id := traceid.New()      // 16 字节随机 → 32 字符 hex
id := traceid.OrNew("")  // 空或无效时生成新 ID，否则原样返回
```

- 使用 `crypto/rand`，密码学安全
- 格式与 OpenTelemetry trace ID 兼容（32 字符 hex）
- `crypto/rand` 极端失败时返回零值 hex（不影响流程）

## Context 传播

| 函数 | 说明 |
|------|------|
| `WithTraceID(ctx, id)` | 注入 Trace ID 到 context |
| `FromContext(ctx)` | 提取 Trace ID（优先本包注入值 → OTel span → 空串） |
| `NewContext(ctx)` | 确保 context 有 Trace ID（已有则原样，否则生成新 ID） |

### OTel 桥接

`FromContext` 会自动检测 context 中是否有 OpenTelemetry span：

```go
// 如果有 OTel span，FromContext 自动返回 span 的 trace ID
ctx, span := tracer.Start(ctx, "myOperation")
defer span.End()

traceID := traceid.FromContext(ctx) // → OTel span 的 trace ID
```

## 日志集成

### L — 快捷日志

```go
// 最常用：直接获取带 trace_id 的 SugaredLogger
traceid.L(ctx).Infow("msg", "key", val)
traceid.L(ctx).Errorw("failed", "err", err)
```

### WithLogger / WithLoggerFrom

```go
// 使用全局 Logger + context
logger := traceid.WithLogger(ctx)

// 使用自定义 Logger + context
logger := traceid.WithLoggerFrom(ctx, customLogger)

// context 中无 Trace ID 时返回原 Logger（不加字段）
```

### GORM 自动注入

`init()` 时自动注册 GORM context fielder，使所有数据库日志自动携带 `trace_id` 字段，无需手动处理。

## HTTP 中间件

```go
// 标准 Handler 中间件
handler := traceid.Middleware(myHandler)

// HandlerFunc 中间件
handlerFunc := traceid.MiddlewareFunc(myHandlerFunc)

// Gin 中间件适配
r.Use(func(c *gin.Context) {
    traceid.MiddlewareFunc(func(w http.ResponseWriter, r *http.Request) {
        c.Next()
    })(c.Writer, c.Request)
})
```

中间件行为：

1. 检查请求头 `X-Trace-ID`，有则复用
2. 否则检查 context 中已有值
3. 都没有则生成新 ID
4. 注入 context + 设置响应头
5. 下游所有 `traceid.L(ctx)` 自动携带

## 工具函数

| 函数 | 说明 |
|------|------|
| `IsValid(id)` | 检查是否为有效 Trace ID（32 字符 hex） |
| `OrNew(id)` | 有效则返回原值，无效则生成新 ID |

## 全链路追踪架构

```
HTTP Request
    │
    ▼
traceid.Middleware ──→ 注入 X-Trace-ID 到 context
    │
    ▼
traceid.L(ctx) ──→ 业务日志自动带 trace_id
    │
    ▼
GORM (init 自动注册) ──→ DB 日志自动带 trace_id
    │
    ▼
retry.Do(ctx) ──→ 重试日志自动带 trace_id
    │
    ▼
HTTP Response ←── 响应头 X-Trace-ID
```

## 文件结构

| 文件 | 职责 |
|------|------|
| `traceid.go` | 常量、ID 生成、Context 注入/提取、OTel 桥接、Logger 集成、HTTP 中间件、验证工具 |
