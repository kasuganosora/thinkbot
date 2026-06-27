# util — 通用工具库

各子包提供独立的通用工具能力，尽量减少跨包依赖。

## 子包一览

| 子包 | 说明 | 关键能力 |
|------|------|---------|
| [errs](errs/) | 结构化错误处理 | HTTP 状态码映射、堆栈捕获、链式 With/WithCode、日志分级集成 |
| [http](http/) | HTTP 客户端 | 自动重试（Retry-After）、SSE 流式解析、WebSocket、Multipart 上传、代理、Dump 调试 |
| [idgen](idgen/) | 唯一 ID 生成器 | crypto/rand 安全随机、带前缀 ID、96 位随机空间 |
| [log](log/) | 结构化日志 | Zap + GORM 桥接、多输出源、Lumberjack 文件轮转、Context 字段注入 |
| [retry](retry/) | 重试执行器 | 指数/线性/固定退避、Panic 恢复、HTTP 智能重试、LLM/流式预设 |
| [strutil](strutil/) | 字符串工具 | Unicode 安全截断、LLM JSON 提取、Map 键提取 |
| [traceid](traceid/) | 链路追踪 ID | 128-bit Trace ID、OTel 桥接、context 传播、HTTP 中间件、GORM 自动注入 |
| [watchdog](watchdog/) | 看门狗定时器 | 无活动超时检测、parent context 传播、TimedOut 区分、动态超时 |

## 子包依赖关系

```
traceid ──→ log
errs     ──→ log
retry    ──→ errs, traceid
watchdog ──→ traceid
http     ──→（独立，可选集成 retry）
idgen    ──→（无依赖）
strutil  ──→（无依赖）
```

`log` 是最底层基础包，`traceid` 和 `errs` 依赖它。`retry` 和 `watchdog` 依赖 `traceid` + `errs`。`idgen` 和 `strutil` 零依赖。
