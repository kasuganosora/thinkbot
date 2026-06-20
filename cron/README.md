# cron — 定时任务调度

## 概述

参照同类 Agent 框架的 Cron 机制，实现定时触发 bot 执行预设提示词的功能。

核心特性：
- **4 种调度格式**：标准 cron 表达式、间隔循环、相对延迟、ISO 时间戳
- **时区感知**：使用 bot 配置的时区（`BotConfig.Timezone`）解析所有时间
- **可观测性**：每次执行生成独立 trace_id，全链路结构化日志
- **Token 统计**：cron 产生的 token 消耗自动记录到 stats 模块
- **JSON 持久化**：原子写入，重启不丢失
- **并发控制**：限制同时执行的 Job 数量
- **生命周期管理**：创建/暂停/恢复/手动触发/删除
- **一次性任务**：执行后自动标记 Done，支持宽限窗口

## 调度格式

| 格式 | 示例 | 说明 |
|------|------|------|
| Cron 表达式 | `0 9 * * 1-5` | 标准 5 段（分 时 日 月 周），工作日 9:00 |
| 间隔循环 | `every 30m` | 每 30 分钟，永不停止 |
| 相对延迟 | `2h` / `1d` | 延迟后执行一次 |
| ISO 时间戳 | `2026-03-20T14:00` | 在指定时刻执行一次 |

Cron 表达式支持 `*` `/` `-` `,` 语法（不支持秒、年、L/W/#）。

## 时区处理

调度器初始化时接收 `*time.Location`，通常从 `BotConfig.Location()` 获取：

```go
loc := bot.Config.Location() // "Asia/Shanghai" → time.LoadLocation → *time.Location
scheduler := cron.NewScheduler(store, executor, cron.SchedulerConfig{
    Location: loc,
    BotID:    bot.ID,
    // ...
})
```

- Cron 表达式和 ISO 时间戳在 bot 时区内解析
- `NextRunAt` 以 UTC 时间戳存储（跨时区安全）
- 时区为空时使用系统本地时区
- 无效时区名称安全回退到 UTC

## 可观测性

### TraceID

每次 Job 执行自动生成 128-bit trace_id，注入到 `context.Context` 中。
执行器可以通过 `traceid.FromContext(ctx)` 获取，实现全链路追踪。

```
INFO  cron: scheduler starting    {tick_interval, max_concurrent, tz, bot_id}
INFO  cron: job executing          {trace_id, job_id, job_name, schedule, run_count, tz}
INFO  cron: job completed          {trace_id, job_id, duration, output_len}
ERROR cron: job failed             {trace_id, job_id, duration, err}
INFO  cron: job marked done        {job_id, reason}
INFO  cron: scheduler stopped
```

### Token 统计

通过 `WithUsageRecorder()` 注入 `llm.UsageRecorder`（即 `stats.Recorder`）：

```go
scheduler.WithUsageRecorder(statsRecorder)
```

执行器在 `ExecuteResult` 中返回 `llm.Usage`，调度器自动以 `feature` 维度记录到 stats 表。
默认 feature 为 `"cron"`，可通过 Job 的 `Feature` 字段自定义。

在 stats 模块的 `stats_usage_daily` 表中可查询 cron 产生的 token 消耗：

```sql
SELECT feature, SUM(total_tokens), SUM(input_tokens), SUM(output_tokens)
FROM stats_usage_daily
WHERE feature LIKE 'cron%'
GROUP BY feature;
```

## 架构

```
┌──────────┐     ┌──────────────┐     ┌───────────┐     ┌──────────┐
│ Manager  │────▶│    Store     │◀────│ Scheduler │────▶│ Executor │
│ (CRUD)   │     │ (JSON File)  │     │ (Tick)    │     │ (Bot)    │
└──────────┘     └──────────────┘     └───────────┘     └──────────┘
                                          │
                                          ▼
                                   ┌──────────────┐
                                   │   Recorder   │
                                   │ (stats 模块) │
                                   └──────────────┘
```

- **Store**: JSON 文件存储，原子写入（tmp → rename），读写锁保护
- **Scheduler**: 每 60s 扫描活跃 Job，到期则异步执行，自动注入 trace_id
- **Manager**: Job 的 CRUD 接口（创建/查询/更新/删除/暂停/恢复/触发）
- **Executor**: 执行抽象接口，返回 `ExecuteResult`（含 token 用量）

## 使用示例

```go
// 1. 创建存储和执行器
store := cron.NewStore(filepath.Join(dataDir, "cron.json"))

executor := cron.ExecutorFunc(func(ctx context.Context, job *cron.Job) (*cron.ExecuteResult, error) {
    // 向 bot 发送合成消息，获取 token 用量
    result := bot.TriggerCronJob(ctx, job.Prompt, job.Model)
    return &cron.ExecuteResult{
        Output:    result.Summary,
        Usage:     result.Usage,     // llm.Usage
        ToolCalls: result.ToolCalls,
        Steps:     result.Steps,
    }, nil
})

// 2. 创建调度器（使用 bot 时区）
loc := bot.Config.Location()
scheduler := cron.NewScheduler(store, executor, cron.SchedulerConfig{
    Location:  loc,
    BotID:     bot.ID,
}).
    WithUsageRecorder(statsRecorder) // 启用 token 统计

// 3. 启动
ctx := context.Background()
scheduler.Start(ctx)
defer scheduler.Stop()

// 4. 管理任务
mgr := cron.NewManager(store, loc)

// 创建每天 9:00（bot 时区）的任务
job, err := mgr.CreateJob(cron.CreateJobRequest{
    Name:     "每日早报",
    Prompt:   "总结今天的新闻",
    Schedule: "0 9 * * *",
    Feature:  "cron_morning", // 自定义统计标签
    Tags:     []string{"daily"},
})
```

## Agent 工具

cron 模块提供**单一压缩工具** `cron`，通过 `action` 参数分发到不同操作。
这种设计（参考 Hermes Agent）显著减少 LLM 上下文中的工具 schema token 开销。

| action | 说明 | 必填参数 |
|--------|------|----------|
| `create` | 创建定时任务 | name, prompt, schedule |
| `list` | 列出所有任务（可按状态过滤） | — |
| `get` | 查看任务完整详情 | job_id（支持名称） |
| `update` | 更新任务属性 | job_id |
| `remove` | 删除任务 | job_id（支持名称） |
| `pause` | 暂停任务 | job_id（支持名称） |
| `resume` | 恢复任务 | job_id（支持名称） |
| `trigger` | 立即触发执行 | job_id（支持名称） |

注册方式：

```go
import (
    agenttools "github.com/kasuganosora/thinkbot/agent/tools"
    "github.com/kasuganosora/thinkbot/cron"
)

mgr := cron.NewManager(store, loc)
cron.RegisterTools(toolMgr, mgr) // 注册单个 cron 工具
```

### 安全特性

- **Prompt 安全扫描**：创建/更新时自动扫描 prompt 中的注入和 exfiltration 模式（参考 `prompt_scan.go` 设计）
- **按名称查找**：job_id 参数也接受任务名称（大小写不敏感），多匹配时报错
- **反嵌套**：Scopes 限制子 agent 不可用，防止 cron 任务递归创建

## 文件结构

| 文件 | 职责 |
|------|------|
| `cron_parser.go` | 标准 5 段 cron 表达式解析器 |
| `job.go` | Job 模型 + 调度字符串解析（4 种格式） |
| `store.go` | JSON 文件持久化（原子写入） |
| `scheduler.go` | 调度循环 + 执行器 + Manager CRUD + 日志 + 统计 |
| `tools.go` | Agent 工具定义（单一压缩工具 + prompt 安全扫描） |
| `cron_test.go` | 调度器/解析器/存储单元测试（22 个） |
| `tools_test.go` | Agent 工具单元测试（27 个） |
