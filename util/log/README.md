# util/log — Zap 结构化日志

基于 `go.uber.org/zap` 的日志初始化工具包，提供多输出源、文件滚动（Lumberjack）、GORM 日志桥接和 context 字段注入。

---

## 目录

- [快速开始](#快速开始)
- [核心概念](#核心概念)
- [输出源](#输出源)
- [多输出源](#多输出源)
- [文件滚动](#文件滚动)
- [日志格式](#日志格式)
- [全局 Logger 使用](#全局-logger-使用)
- [GORM 日志桥接](#gorm-日志桥接)
- [Context 字段注入](#context-字段注入)
- [向后兼容](#向后兼容)
- [Gin 集成](#gin-集成)
- [文件结构](#文件结构)

---

## 快速开始

```go
import "github.com/kasuganosora/thinkbot/util/log"

// 最简：仅 stdout，console 格式，info 级别
err := log.Init()

// 自定义配置
err := log.InitWithConfig(log.Config{
    Level: "debug",
    Outputs: []log.Output{
        log.Stdout(),                        // 控制台（console 格式，彩色级别）
        log.File("./logs", "thinkbot"),      // 文件（JSONL 格式，自动滚动）
    },
})
if err != nil {
    panic(err)
}

// 使用全局 Logger
log.Logger.Infow("server started", "addr", ":8080")
log.Logger.Errorw("database connection failed", "err", err)
```

---

## 核心概念

| 类型 | 说明 |
|------|------|
| `Logger` | 全局 `*zap.SugaredLogger` 实例，初始化后直接使用 |
| `Config` | 全局日志配置（级别 + 输出源列表） |
| `Output` | 单个输出源配置（类型 + 级别 + 格式 + 文件参数） |
| `GormConfig` | GORM 日志配置 |
| `ContextFielder` | 从 context 提取 zap 字段的回调函数（用于 GORM 日志注入 trace_id） |

---

## 输出源

每个输出源独立配置类型、级别过滤和格式：

```go
log.Output{
    Type:   log.OutputFile,   // stdout / stderr / file
    Level:  "warn",           // 该输出源的级别过滤（空=继承全局）
    Format: log.FormatJSON,   // auto / console / json
}
```

### 快捷构造函数

| 函数 | 类型 | 默认格式 |
|------|------|----------|
| `Stdout()` | stdout | console（彩色） |
| `Stderr()` | stderr | console（彩色） |
| `File(dir, name)` | file | json（JSONL） |

快捷构造返回的是 `Output` 结构体值，需要覆盖字段时直接赋值：

```go
fileOut := log.File("/var/log/thinkbot", "app")
fileOut.Level = "info" // 文件只记 info+

outputs := []log.Output{
    log.Stdout(), // 控制台看 debug（继承全局 Level）
    fileOut,
}
```

> 注意：`stdout` / `stderr` 输出源始终使用 console 编码器（彩色级别），
> 对它们设置 `Format: FormatJSON` 不会生效。`Format` 仅对 `file` 输出源有实际区分。

---

## 多输出源

支持同时输出到多个目标，每个目标有独立的级别和格式：

```go
err := log.InitWithConfig(log.Config{
    Level: "debug",
    Outputs: []log.Output{
        // 控制台：debug 级别，console 格式（彩色）
        log.Stdout(),

        // 主日志文件：info 级别，JSONL 格式，100MB 滚动
        log.File("./logs", "thinkbot"),

        // 错误日志文件：仅 error 级别，JSONL 格式
        {
            Type:   log.OutputFile,
            Level:  "error",
            Format: log.FormatJSON,
            FileDir: "./logs", FileName: "error",
            MaxSize: 50, MaxBackups: 30, MaxAge: 90, Compress: true,
        },
    },
})
```

底层使用 `zapcore.NewTee` 将多个 Core 组合为一个 Logger。

---

## 文件滚动

文件输出源通过 [Lumberjack](https://github.com/natefinch/lumberjack) 实现自动滚动：

`File(dir, name)` 已预填一组默认值：

```go
out := log.File("./logs", "thinkbot")
// out.FileExt    == ".log"
// out.MaxSize    == 100    // 单文件最大 100MB
// out.MaxBackups == 7      // 保留 7 个旧文件
// out.MaxAge     == 30     // 旧文件保留 30 天
// out.Compress   == true   // gzip 压缩旧文件
```

也可以通过 `Output` 结构体完全自定义：

```go
log.Output{
    Type:       log.OutputFile,
    Format:     log.FormatJSON,
    FileDir:    "/var/log/myapp",
    FileName:   "app",
    FileExt:    ".log",
    MaxSize:    200,   // 200MB
    MaxBackups: 20,
    MaxAge:     60,
    Compress:   true,
}
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `FileDir` | `"."` | 日志目录（不存在自动创建） |
| `FileName` | `"thinkbot"` | 文件名（不含扩展名） |
| `FileExt` | `".log"` | 文件扩展名 |
| `MaxSize` | `100` | 单文件最大体积（MB），超出后滚动 |
| `MaxBackups` | `7` | 保留旧文件数量 |
| `MaxAge` | `30` | 旧文件最大保留天数 |
| `Compress` | `false` | 是否 gzip 压缩旧文件（`File()` 构造时预设为 `true`） |

上表默认值在字段为零值（数值 ≤ 0 / 字符串为空）时由构建逻辑兜底，`Compress` 除外（布尔无兜底）。
滚动文件时间戳使用 `LocalTime`（非 UTC）。

---

## 日志格式

| 格式 | 说明 | 典型场景 |
|------|------|----------|
| `console` | 人类可读，彩色级别标记 | 终端 / 开发环境 |
| `json` | JSONL（每行一个 JSON 对象） | 文件 / 生产环境 / 日志收集 |
| `auto` | 自动：stdout/stderr → console，file → json | 默认 |

**Encoder 配置**（所有格式共用）：

| 字段 | Key | 编码方式 |
|------|-----|----------|
| 时间 | `ts` | ISO8601（`2025-06-27T22:50:00.123+0800`） |
| 级别 | `level` | 大写（`INFO` / `ERROR`），console 模式带颜色 |
| 调用方 | `caller` | 短路径（`pkg/file.go:42`） |
| 消息 | `msg` | — |
| 堆栈 | `stacktrace` | 保留了 key，但未启用 `zap.AddStacktrace`，需显式传字段才会输出 |
| 耗时 | — | 秒级浮点数 |

Logger 通过 `zap.New(core, zap.AddCaller())` 构建，并调用 `zap.ReplaceGlobals` 覆盖全局实例。

---

## 全局 Logger 使用

初始化后，`log.Logger` 是一个 `*zap.SugaredLogger`，同时 `zap.L()` 和 `zap.S()` 也指向同一个实例：

```go
// SugaredLogger（推荐，支持键值对）
log.Logger.Infow("user login", "uid", 123, "ip", "1.2.3.4")
log.Logger.Errorw("db error", "err", err, "query", sql)
log.Logger.Debugw("cache miss", "key", key)

// 格式化消息
log.Logger.Infof("listening on :%d", port)
log.Logger.Errorf("failed to start: %v", err)

// Desugar 后获得高性能 Logger
log.Logger.Desugar().Info("high-throughput event")
```

---

## GORM 日志桥接

将 GORM 的日志转发到 zap，统一输出格式，支持慢查询检测和 SQL 记录：

```go
import "gorm.io/gorm"

db, err := gorm.Open(sqlite.Open("test.db"), &gorm.Config{
    Logger: log.NewGormLogger(log.DefaultGormConfig()),
})
```

### GORM 日志配置

```go
gormLog := log.NewGormLogger(log.GormConfig{
    Level:                     log.GormInfo,           // silent / error / warn / info
    SlowThreshold:             500 * time.Millisecond, // 慢查询阈值
    IgnoreRecordNotFoundError: true,                   // 忽略 ErrRecordNotFound
})
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `Level` | `warn` | silent / error / warn / info（`DefaultGormConfig()` 为 `warn`；未识别的值也回落到 `warn`） |
| `SlowThreshold` | `200ms` | 超过此值的查询以 Warn 记录（≤ 0 时兜底为 200ms） |
| `IgnoreRecordNotFoundError` | `false` | 是否忽略 `ErrRecordNotFound`（`DefaultGormConfig()` 设为 `true`） |
| `ParameterizedQueries` | `false` | 预留字段，当前实现未使用 |

`DefaultGormConfig()` 返回 `{Level: warn, SlowThreshold: 200ms, IgnoreRecordNotFoundError: true}`。

### 日志输出

| 场景 | 级别 | 包含字段 |
|------|------|----------|
| 查询错误 | Error | sql, rows, elapsed, err, caller, trace_id |
| 慢查询 | Warn | sql, rows, elapsed, threshold, caller, trace_id |
| 正常查询（`Level: info`） | Debug | sql, rows, elapsed, caller, trace_id |

`context.Canceled` / `context.DeadlineExceeded`（客户端断开、超时取消）属于预期中断，不会记为 Error。

**真实 caller 捕获**：GORM logger 禁用了 zap 内置的 caller（只会指向 `gorm.go`），改为通过 `runtime.Callers` 遍历调用栈，跳过 `gorm.io` 和 `util/log` 帧，返回真正的业务调用方文件和行号。

### 独立 Logger（不依赖全局）

```go
customZap := zap.New(...)
gormLog := log.NewGormLoggerWithZap(customZap, log.DefaultGormConfig())
```

---

## Context 字段注入

GORM logger 支持从 `context.Context` 中提取字段（如 trace_id）注入到每条日志：

```go
// 在 util/traceid 包中注册（避免循环依赖）
log.RegisterGormContextFielder(func(ctx context.Context) []zap.Field {
    if tid := traceid.FromContext(ctx); tid != "" {
        return []zap.Field{zap.String("trace_id", tid)}
    }
    return nil
})

// 之后所有带 context 的 GORM 操作自动携带 trace_id
db.WithContext(ctx).Find(&users)
// → {"level":"debug","msg":"gorm query","trace_id":"req-abc-123","sql":"SELECT ..."}
```

通过 `ContextFielder` 回调模式解耦，`util/log` 不需要反向导入 `util/traceid`。

---

## 向后兼容

旧版配置方式仍然支持，`Outputs` 为空时自动从 Legacy 字段推导：

```go
// 旧版写法（仍然有效）
err := log.InitWithConfig(log.Config{
    Level:      "info",
    FileDir:    "./logs",
    FileName:   "thinkbot",
    FileExt:    ".log",
    MaxSize:    100,
    MaxBackups: 7,
    MaxAge:     30,
    Compress:   true,
    AlsoStdout: true,  // 文件 + stdout
})
// 等价于 Outputs: [File("./logs", "thinkbot"), Stdout()]
```

`FileDir` 为空时退化为仅 stdout 输出。

---

## Gin 集成

本包不提供 Gin 适配器。Gin 的请求日志与 panic 恢复重定向到 zap 的实现位于 `api` 包
（`zapWriter` / `zapRecovery`），它们直接复用 `log.Logger`。

---

## 文件结构

```
util/log/
├── logger.go   # 全局 Logger + Config + Output + 多 Core 组装 + Lumberjack 文件滚动
└── gorm.go     # GORM logger.Interface 实现 + 慢查询检测 + caller 捕获 + context 字段注入
```
