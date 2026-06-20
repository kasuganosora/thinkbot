# util/log — Zap 日志初始化

基于 `go.uber.org/zap` 的日志配置工具，提供生产环境和开发环境预设。

## 核心函数

```go
// 初始化全局 logger
logger := log.New(log.Config{
    Level:  "info",
    Format: "json", // "json" 或 "console"
    Output: "stderr",
})

// 获取 SugaredLogger
sugar := logger.Sugar()
sugar.Infow("server started", "addr", addr, "port", 8080)
```

## 配置

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `Level` | `"info"` | 日志级别（debug/info/warn/error） |
| `Format` | `"console"` | 输出格式（`json` / `console`） |
| `Output` | `"stderr"` | 输出路径 |
