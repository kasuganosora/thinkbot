# util — 通用工具库

各子包提供独立的通用工具能力，无跨包依赖。

## 子包

| 子包 | 说明 |
|------|------|
| [errs](#errs) | 错误处理（错误码 + 包装） |
| [http](#http) | HTTP 客户端（重试/SSE/WebSocket/多部分上传） |
| [idgen](#idgen) | ID 生成器（UUID/雪花 ID/短 ID） |
| [log](#log) | 日志（Zap + GORM 适配 + lumberjack 轮转） |
| [retry](#retry) | 重试策略（指数退避 + 看门狗） |
| [strutil](#strutil) | 字符串工具（截断/哈希/模板） |
| [traceid](#traceid) | 链路追踪 ID 生成与传播 |
| [watchdog](#watchdog) | 看门狗定时器（超时检测） |

---

### errs

错误码体系，支持错误包装和错误码提取。

```go
err := errs.New(1001, "用户不存在")
code := errs.Code(err) // 1001
```

### http

增强版 HTTP 客户端，支持自动重试、SSE 流式解析、WebSocket 连接和多部分文件上传。

### idgen

```go
id := idgen.UUID()   // 随机 UUID
sid := idgen.Short()  // 短 ID
```

### log

结构化日志，基于 Zap。自动初始化，全局可用。

```go
log.Logger.Info("hello", zap.String("key", "value"))
```

### retry

可配置的重试策略，支持指数退避和看门狗超时。

```go
err := retry.Do(ctx, func() error {
    return callAPI()
}, retry.WithMaxAttempts(3), retry.WithBackoff(time.Second))
```

### strutil

字符串处理工具（截断、模板渲染、哈希等）。

### traceid

OpenTelemetry 兼容的 TraceID 生成。

### watchdog

看门狗定时器，用于检测长时间无响应的操作。
