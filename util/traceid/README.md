# util/traceid — OpenTelemetry Trace ID 工具

提供 Trace ID 的生成、注入和提取工具，用于全链路追踪。

## 核心函数

```go
// 生成新 Trace ID
traceID := traceid.New() // → "4bf92f3577b34da6a3ce929d0e0e4736"

// 从 HTTP Header 提取 Trace ID（兼容 W3C Trace Context）
traceID := traceid.FromHeader(header)

// 注入 Trace ID 到 context
ctx = traceid.WithTraceID(ctx, traceID)

// 从 context 提取 Trace ID
traceID := traceid.GetTraceID(ctx)
```

## 设计

- 兼容 W3C Trace Context 格式（`traceparent` header）
- 与 OpenTelemetry SDK 集成，自动桥接到 OTel Span
- 提供 HTTP 中间件，自动注入 Trace ID 到请求 context
