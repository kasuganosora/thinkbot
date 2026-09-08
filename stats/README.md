# stats — LLM 用量统计

记录和查询 Bot 的 LLM Token 使用量、缓存命中、工具调用等运行指标。通过异步批量写入 + 按日聚合，实现低开销的全链路用量追踪；另含两条**旁路明细**链路：工作流节点用量明细（`workflow_usage`）与 engagement 快判结果（`judge_records`）。

## 架构概览

```
LLM 调用完成
    │
    ▼
UsageMetric                          ← llm.UsageMetric（Bot/Model/Feature/Channel/Usage/ToolCalls/Steps）
    │
    ▼
Recorder.RecordUsage()               ← 非阻塞写入 channel（满则丢弃+告警）
    │
    ▼
后台 goroutine                       ← 5s 定时 或 100 条批量触发
    │
    ▼
flushBatch()                         ← 按 (bot_id, model, feature, channel, date) 聚合
    │            └→ flushWorkflowUsage()（旁路：WorkflowID 非空时逐条写明细，失败不影响主链）
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
// 1. AutoMigrate stats_usage_daily / workflow_usage / judge_records 三张表
// 2. 启动 Recorder 与 JudgeRecorder 两个后台写入 goroutine
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
    Channel: "telegram", // 可选，非 pipeline 路径（dream/memory 等）为空
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
| `Stop()` | 关闭 stopCh 并等待 goroutine 退出（退出前 drain channel 剩余指标并 flush） |
| `RecordUsage(ctx, metric)` | 非阻塞记录（channel 满时丢弃 + Warn 日志） |
| `SyncFlush()` | 同步 drain 并 flush（测试用） |

### 写入触发条件

| 条件 | 行为 |
|------|------|
| channel 消息数累积到 100 | 立即 flush |
| 每 5 秒定时器触发 | flush 全部缓冲 |
| `Stop()` 调用 | drain 剩余 + flush + 退出 goroutine |

### 聚合策略

同一批次（batch）中的指标按 **(bot_id, model, feature, channel, date)** 五元组聚合后逐行 upsert：

```sql
INSERT INTO stats_usage_daily (...) VALUES (...)
ON CONFLICT(bot_id, model, feature, channel, date) DO UPDATE SET
    total_requests = total_requests + excluded.total_requests,
    input_tokens   = input_tokens   + excluded.input_tokens,
    ...
```

`date` 取 **指标发生时刻** `UsageMetric.At` 的当天零点（`truncateToDate`，UTC），
确保同一天的数据汇总到同一行，避免延迟 flush / 跨日时错日；`At` 未填充时回退到 flush 时刻。

---

## 数据模型

### stats_usage_daily 表

按日聚合的 LLM 使用统计表（`dao.UsageDaily`），维度组合 `(bot_id, model, feature, channel, date)` 唯一。

| 字段 | 类型 | 说明 |
|------|------|------|
| `bot_id` | string | Bot 标识 |
| `model` | string | 模型标识（如 `glm-5.2`） |
| `feature` | string | 功能维度。代码中实际出现的取值：`reply`、`vision`、`subagent`、`engagement`、`memory_formation`、`memory_compression`、`memory_consolidation`、`memory_dedup`、`dream_extract`、`dream_cluster`、`bot_profiler`、`user_profiler`、`heartbeat`、`dreaming`、`quota_blocked`、`budget_warning`、`cron`（cron 任务默认，可经 `job.Feature` 自定义） |
| `channel` | string | 来源渠道（如 `telegram`/`web`/`misskey`），非 pipeline 路径为空串 |
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

### workflow_usage 表（旁路明细）

工作流节点维度的**逐条**调用明细（`dao.WorkflowUsage`），由 `Recorder.flushWorkflowUsage` 在日聚合之后旁路写入：

- 只落 `WorkflowID` 非空的指标，非工作流路径（reply / dream / memory 等）零开销
- `workflow_id` / `node_id` **不进** UsageDaily 聚合维度——否则日聚合表会被撑成明细表、五列唯一索引语义被破坏
- 失败仅记日志，不影响日聚合主链路
- 两表经 `bot_id` + `created_at` 关联，Token 口径一致便于对账

| 字段 | 说明 |
|------|------|
| `workflow_id` / `node_id` | 归因到哪条工作流的哪个节点 |
| `bot_id` / `model` / `feature` | 同 UsageDaily 维度 |
| `input_tokens` / `output_tokens` / `total_tokens` | Token 明细（口径与 UsageDaily 一致） |
| `cache_read_tokens` / `cache_write_tokens` | 缓存 Token 明细 |
| `tool_calls` / `steps` | 编排指标 |

### judge_records 表（旁路明细）

engagement LLM 快判（Tier 2 judge）的**逐条**结果明细（`dao.JudgeRecord`），使参与决策的质量可观测。与 UsageDaily 刻意分开：后者是日聚合记 token，本表记判定语义。

| 字段 | 说明 |
|------|------|
| `bot_id` / `channel` / `model` | 维度；`feature` 固定 `engagement`（预留） |
| `engage` | LLM 认为是否值得参与 |
| `score` | 0-100 评分；0 表示未用评分模式（传统 YES/NO） |
| `reason` | LLM 理由（落库前截断到 480 字符） |
| `tier` | 决策层（`tier_rule` / `tier_llm`） |
| `latency_ms` | 判定耗时 |

### UsageMetric（输入）

由各 Stage 在 LLM 调用完成后构建，传递给 `Recorder.RecordUsage()`：

```go
type llm.UsageMetric struct {
    BotID      string    // 哪个 Bot
    At         time.Time // 调用发生时刻（归日用；零值回退 flush 时刻）
    Model      string    // 哪个模型
    Feature    string    // 哪个功能场景
    Channel    string    // 来源渠道（可为空）
    Usage      llm.Usage // Token 用量（含缓存明细）
    ToolCalls  int       // 工具调用次数
    Steps      int       // 编排步数
    WorkflowID string    // 工作流归因（非工作流路径为空；不进日聚合维度）
    NodeID     string    // 工作流节点归因（同上）
}
```

---

## 查询 API

所有查询函数直接接受 `*gorm.DB`，不依赖 Recorder 实例，可在 API Handler 中独立使用。

### 查询函数

| 函数 | 维度 | 用途 |
|------|------|------|
| `GetBotModelStats(db, botID, from, to)` | Bot × Model | 某 Bot 各模型的用量汇总（按 total_tokens 降序） |
| `GetModelFeatureStats(db, botID, model, from, to)` | Model × Feature | 某 Bot + 模型在各功能中的分布（按 total_requests 降序） |
| `GetDailyStats(db, botID, from, to)` | Date | 某 Bot 按天的用量趋势（date 降序） |
| `GetAllBotsModelStats(db, from, to)` | Bot × Model | 管理面板：全部 Bot 的用量（bot_id 升序、组内 total_tokens 降序） |
| `GetDailyStatsGlobal(db, botID, from, to)` | Date | 全局按天趋势（`botID` 为空则不限 Bot，date 升序） |
| `GetDailyByBotStats(db, from, to)` | Date × Bot | 按日×Bot 的 token 量（堆叠图表用） |
| `GetUsageRecords(db, botID, from, to, page, pageSize)` | 明细 | 分页查询用量流水，返回 `([]UsageRecord, total, error)` |

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
    CacheReadTokens   int
    CacheWriteTokens  int
    NonCacheTokens    int
    TotalTokens       int
}
```

#### DailyByBotEntry — 按日 × Bot

```go
type DailyByBotEntry struct {
    Date   time.Time
    BotID  string
    Tokens int   // 列 total_tokens
}
```

#### UsageRecord — 流水明细

```go
type UsageRecord struct {
    ID              uint      // 取自 rowid
    Date            time.Time // JSON 字段名为 time
    BotID           string
    Model           string
    Feature         string
    CacheReadTokens int
    InputTokens     int
    OutputTokens    int
    TotalRequests   int
}
```

### 日期范围过滤

所有查询接受 `from *time.Time` / `to *time.Time` 可选参数（`applyDateRange`）：

- `from` / `to` 为 nil 时不限制
- 两侧均截断到 UTC 零点（`truncateToDate`）后按 `date >= from` / `date <= to` 比较
- `to` 与 `from` 同为闭区间边界（按日聚合行比较，无需 +1 天）

---

## fx 模块

```go
var Module = fx.Module("stats",
    fx.Provide(NewRecorderModule),      // 提供 *Recorder + llm.UsageRecorder
    fx.Provide(NewJudgeRecorderModule), // 提供 *JudgeRecorder
    fx.Invoke(RegisterLifecycle),       // 注册生命周期钩子
)
```

| 生命周期 | 行为 |
|---------|------|
| `OnStart` | `AutoMigrate(UsageDaily, JudgeRecord, WorkflowUsage)` + `Recorder.Start()` + `JudgeRecorder.Start()` |
| `OnStop` | `Recorder.Stop()` + `JudgeRecorder.Stop()`（各自 drain + flush 剩余数据） |

`NewRecorderModule` 同时返回 `*Recorder` 和 `llm.UsageRecorder`，后者供各 Stage 通过 fx 可选注入。

`JudgeSink` 不在 fx Module 内提供：由 `api/module.go` 的 `newBotService` 在组装时手动 `stats.NewJudgeSink(p.JudgeRecorder)` 包装（`JudgeRecorder` 为 nil 时不挂 sink，判定结果不落库）。另外 `dao.Migrate()` 的统一迁移列表也包含这三张表，AutoMigrate 幂等，两处注册无害。

---

## JudgeRecorder / JudgeSink — 判定结果旁路落库

`JudgeRecorder` 复用 Recorder 的模式（channel 缓冲 1024 + 后台 goroutine，5s / 100 条批量，`CreateInBatches` 逐条插入，不做聚合），把 engagement 的 LLM 快判结果异步写入 `judge_records`。

| 方法 | 说明 |
|------|------|
| `NewJudgeRecorder(db, logger)` | 创建实例（db 为 nil 时静默丢弃，纯内存/测试模式） |
| `Start()` / `Stop()` | 启停后台 goroutine（Stop 时 drain + flush） |
| `Record(ctx, rec)` | 非阻塞记录（channel 满时丢弃 + Warn） |
| `SyncFlush()` | 同步 drain 并 flush（测试用） |

`JudgeSink` 实现 `engagement.JudgeRecordSink` 接口（消费方定义接口），把 `engagement.JudgeRecord` 适配为 `dao.JudgeRecord` 落库——落库是旁路观测，绝不影响「是否参与」这个主决策。依赖方向为 stats → engagement 单向。

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
| `recorder.go` | `Recorder` 类型、异步 channel 写入、批量聚合、SQLite UPSERT、工作流明细旁路写入（`flushWorkflowUsage`） |
| `repository.go` | 查询函数（`GetBotModelStats` / `GetModelFeatureStats` / `GetDailyStats` / `GetAllBotsModelStats` / `GetDailyStatsGlobal` / `GetDailyByBotStats` / `GetUsageRecords`）、结果类型 |
| `judge_record.go` | `JudgeRecorder` — 判定结果异步批量落库（channel + 后台 goroutine） |
| `judge_sink.go` | `JudgeSink` — 实现 `engagement.JudgeRecordSink`，reason 截断（480 字符） |
| `module.go` | fx Module 定义、`NewRecorderModule` / `NewJudgeRecorderModule`、生命周期钩子 |
