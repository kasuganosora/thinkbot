# stats — LLM 用量统计

记录和查询 Bot 的 LLM Token 使用量、缓存命中、工具调用等运行指标。通过异步批量写入 + 按日聚合，实现低开销的全链路用量追踪。

## 架构概览

```
LLM 调用完成
    │
    ▼
UsageMetric                          ← llm.UsageMetric（Bot/Model/Feature/Usage/ToolCalls/Steps）
    │
    ▼
Recorder.RecordUsage()               ← 非阻塞写入 channel（满则丢弃+告警）
    │
    ▼
后台 goroutine                       ← 5s 定时 或 100 条批量触发
    │
    ▼
flushBatch()                         ← 按 (bot_id, model, feature, date) 聚合
    │
    ▼
SQLite UPSERT → stats_usage_daily    ← ON CONFLICT 累加
```

## 快速开始

### 通过 fx 模块集成

```go
import "github.com/kasuganosora/thinkbot/stats"

// 在 fx App 中注册
app := fx.New(
    // ... db, log 等其他模块
    stats.Module,
)

// Module 自动完成：
// 1. AutoMigrate stats_usage_daily 表
// 2. 启动后台写入 goroutine
// 3. 注册 Recorder 为 llm.UsageRecorder（供各 Stage 注入）
// 4. 应用停止时 flush 剩余指标
```

### 手动使用

```go
recorder := stats.NewRecorder(db, logger)
recorder.Start()
defer recorder.Stop() // 停止时自动 flush

// 记录一次 LLM 调用（实现 llm.UsageRecorder 接口）
recorder.RecordUsage(ctx, llm.UsageMetric{
    BotID:   "bot-1",
    Model:   "glm-5.2",
    Feature: "reply",
    Usage: llm.Usage{
        InputTokens:  150,
        OutputTokens: 80,
        TotalTokens:  230,
    },
    ToolCalls: 2,
    Steps:     3,
})
```

---

## Recorder — 异步批量记录器

实现 `llm.UsageRecorder` 接口，通过 channel + 后台 goroutine 实现非阻塞写入。

### 核心方法

| 方法 | 说明 |
|------|------|
| `NewRecorder(db, logger)` | 创建实例（channel 缓冲 1024，5s flush，batch 100） |
| `Start()` | 启动后台写入 goroutine |
| `Stop()` | 停止 goroutine，drain channel 中剩余指标后 flush |
| `RecordUsage(ctx, metric)` | 非阻塞记录（channel 满时丢弃 + Warn 日志） |
| `SyncFlush()` | 同步 drain 并 flush（测试用） |

### 写入触发条件

| 条件 | 行为 |
|------|------|
| channel 消息数累积到 100 | 立即 flush |
| 每 5 秒定时器触发 | flush 全部缓冲 |
| `Stop()` 调用 | drain 剩余 + flush + 退出 goroutine |

### 聚合策略

同一批次（batch）中的指标按 **(bot_id, model, feature, date)** 四元组聚合后逐行 upsert：

```sql
INSERT INTO stats_usage_daily (...) VALUES (...)
ON CONFLICT(bot_id, model, feature, date) DO UPDATE SET
    total_requests = total_requests + excluded.total_requests,
    input_tokens   = input_tokens   + excluded.input_tokens,
    ...
```

日期截断到 UTC 零点（`truncateToDate`），确保同一天的数据汇总到同一行。

---

## 数据模型

### stats_usage_daily 表

按日聚合的 LLM 使用统计表，维度组合 `(bot_id, model, feature, date)` 唯一。

| 字段 | 类型 | 说明 |
|------|------|------|
| `bot_id` | string | Bot 标识 |
| `model` | string | 模型标识（如 `glm-5.2`） |
| `feature` | string | 功能维度（如 `reply`/`chat`/`vision`/`memory_compress`） |
| `date` | date | 聚合日期（UTC 零点截断） |
| `total_requests` | int | 总请求数 |
| `cache_hit_requests` | int | 缓存命中请求数（当次调用有 CacheRead > 0） |
| `cache_miss_requests` | int | 缓存未命中请求数 |
| `cache_read_tokens` | int | 缓存读取 Token 数 |
| `cache_write_tokens` | int | 缓存写入 Token 数 |
| `non_cache_tokens` | int | 未缓存 Token 数 |
| `input_tokens` | int | 输入 Token 总数 |
| `output_tokens` | int | 输出 Token 总数 |
| `total_tokens` | int | 总 Token 数 |
| `tool_calls` | int | 工具调用累计次数 |
| `steps` | int | 编排步数累计 |

### UsageMetric（输入）

由各 Stage 在 LLM 调用完成后构建，传递给 `Recorder.RecordUsage()`：

```go
type llm.UsageMetric struct {
    BotID     string         // 哪个 Bot
    Model     string         // 哪个模型
    Feature   string         // 哪个功能场景
    Usage     llm.Usage      // Token 用量（含缓存明细）
    ToolCalls int            // 工具调用次数
    Steps     int            // 编排步数
}
```

---

## 查询 API

所有查询函数直接接受 `*gorm.DB`，不依赖 Recorder 实例，可在 API Handler 中独立使用。

### 查询函数

| 函数 | 维度 | 用途 |
|------|------|------|
| `GetBotModelStats(db, botID, from, to)` | Bot × Model | 某 Bot 各模型的用量汇总 |
| `GetModelFeatureStats(db, botID, model, from, to)` | Model × Feature | 某 Bot + 模型在各功能中的分布 |
| `GetDailyStats(db, botID, from, to)` | Date | 某 Bot 按天的用量趋势 |
| `GetAllBotsModelStats(db, from, to)` | Bot × Model | 管理面板：全部 Bot 的用量 |

### 使用示例

```go
// 查询 bot-1 最近 7 天各模型用量
from := time.Now().AddDate(0, 0, -7)
to := time.Now()
stats, err := stats.GetBotModelStats(db, "bot-1", &from, &to)
// → [{Model: "glm-5.2", TotalTokens: 52300, InputTokens: 38000, ...}, ...]

// 查询 bot-1 的 glm-5.2 模型在各功能中的分布
featureStats, err := stats.GetModelFeatureStats(db, "bot-1", "glm-5.2", &from, &to)
// → [{Feature: "reply", TotalRequests: 120, ...}, {Feature: "vision", ...}]

// 查询 bot-1 按天的用量趋势
daily, err := stats.GetDailyStats(db, "bot-1", &from, &to)
// → [{Date: "2026-06-27", TotalTokens: 8200, ...}, ...]

// 管理面板：所有 Bot 的模型用量
allStats, err := stats.GetAllBotsModelStats(db, &from, &to)
```

### 查询结果类型

#### BotModelStat — Bot × Model 汇总

```go
type BotModelStat struct {
    BotID             string
    Model             string
    TotalRequests     int
    CacheHitRequests  int   // 缓存命中请求数
    CacheMissRequests int   // 缓存未命中请求数
    CacheReadTokens   int   // 缓存读取 Token
    CacheWriteTokens  int   // 缓存写入 Token
    NonCacheTokens    int   // 未缓存 Token
    InputTokens       int
    OutputTokens      int
    TotalTokens       int
    ToolCalls         int
}
```

#### ModelFeatureStat — Model × Feature 汇总

```go
type ModelFeatureStat struct {
    Model             string
    Feature           string
    TotalRequests     int
    CacheHitRequests  int
    CacheMissRequests int
    CacheReadTokens   int
    TotalTokens       int
}
```

#### DailyStat — 按日汇总

```go
type DailyStat struct {
    Date              time.Time
    TotalRequests     int
    CacheHitRequests  int
    CacheMissRequests int
    TotalTokens       int
}
```

### 日期范围过滤

所有查询接受 `from *time.Time` / `to *time.Time` 可选参数：

- `from` / `to` 为 nil 时不限制
- 日期截断到 UTC 零点
- `to` 包含当天（自动 +1 天再 `<=` 比较）

---

## fx 模块

```go
var Module = fx.Module("stats",
    fx.Provide(NewRecorderModule),  // 提供 *Recorder + llm.UsageRecorder
    fx.Invoke(RegisterLifecycle),   // 注册生命周期钩子
)
```

| 生命周期 | 行为 |
|---------|------|
| `OnStart` | `AutoMigrate(&dao.UsageDaily{})` + `Recorder.Start()` |
| `OnStop` | `Recorder.Stop()`（drain + flush 剩余指标） |

`NewRecorderModule` 同时返回 `*Recorder` 和 `llm.UsageRecorder`，后者供各 Stage 通过 fx 可选注入。

---

## 缓存命中判定

当一次 LLM 调用的 `Usage.InputTokenDetails.CacheReadTokens > 0` 或 `CachedInputTokens > 0` 时，该请求计为 **cache hit**，否则计为 **cache miss**。这允许统计 Prompt Cache 的效果：

```
缓存命中率 = CacheHitRequests / TotalRequests
Token 节省 = (InputTokens - CacheReadTokens) 的比例变化
```

---

## 文件结构

| 文件 | 职责 |
|------|------|
| `recorder.go` | `Recorder` 类型、异步 channel 写入、批量聚合、SQLite UPSERT |
| `repository.go` | 查询函数（`GetBotModelStats` / `GetModelFeatureStats` / `GetDailyStats` / `GetAllBotsModelStats`）、结果类型 |
| `module.go` | fx Module 定义、`NewRecorderModule`、生命周期钩子 |
