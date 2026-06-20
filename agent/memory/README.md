# memory — 多层记忆系统

实现四层记忆架构（L0 工作记忆 / L1 长期记忆 / L2 场景记忆 / L3 用户画像），支持自动巩固、快照刷新、上下文展开、笔记过滤、工具持久化和可插拔 Provider。

## 功能

- **分层存储**：L0 工作记忆（近期对话窗口）→ L1 长期记忆（事实/偏好）→ L2 场景记忆（事件快照）→ L3 用户画像（性格特征）
- **自动巩固**：后台定期将 L0 记忆通过 LLM 巩固为 L1（去重/合并/更新）
- **快照刷新**：可配置刷新策略（实时/冻结/定期），默认实时模式让 bot 始终看到最新记忆
- **上下文展开**：从 L1/L2 检索相关记忆注入 LLM 上下文
- **上下文隔离**：系统标注包裹记忆上下文，防止记忆内容被误认为用户输入
- **笔记过滤**：自动识别值得记住的信息（Think Filter）
- **工具持久化**：将工具调用结果存入记忆供后续引用
- **窗口管理**：基于 token 计数的滑动窗口，自动压缩旧消息
- **可插拔 Provider**：`MemoryProvider` 接口 + `ProviderManager` 编排器，支持外部记忆后端
- **后台同步**：单 worker 串行写入，不阻塞对话循环；带 debounce 和 drain 超时
- **预取缓存**：每轮结束后异步预取下一轮记忆，命中时零延迟
- **写入威胁扫描**：记忆写入时自动检测注入/渗出模式（复用 `prompt_scan.go`）
- **批量原子操作**：单次 `batch` 调用执行 add+replace+remove，按最终字符预算验证
- **梦境巩固**：三相位后台记忆整理管线（Light → REM → Deep），证据驱动评分门控，从短期信号提取长期知识
- **画像语义验证**：提取的用户画像通过 embedding cosine 相似度（或 Jaccard 降级）验证与源记忆的一致性
- **可观测性**：后台任务 panic 恢复 + 日志记录，`traceid` 贯穿请求级到后台任务级

## 关键类型

| 类型 | 说明 |
|------|------|
| `TieredManager` | 分层记忆管理器（核心入口） |
| `Entry` / `TieredEntry` | 记忆条目 / 带层级标识的条目 |
| `Scope` | 记忆作用域（Channel/User/Bot/Global） |
| `LLMConsolidator` | LLM 驱动的记忆巩固器 |
| `Expander` | 上下文展开器 |
| `ThinkFilter` | 笔记价值过滤器 |
| `Window` | 对话窗口管理器 |
| `LLMProfiler` | 用户画像提取器（TF-IDF 聚类 + 语义验证） |
| `Snapshot` | 快照管理器（实时/冻结/定期刷新） |
| `MemoryProvider` | 可插拔记忆后端接口 |
| `ProviderManager` | Provider 编排器（统一调度） |
| `SyncExecutor` | 后台同步执行器（单 worker 串行，panic 恢复 + 可观测日志） |
| `BackgroundSyncManager` | 记忆后台写入协调器（封装 SyncExecutor + debounce） |
| `PrefetchManager` | 预取缓存管理器 |
| `DreamManager` | 梦境巩固管线（Light → REM → Deep） |
| `DreamReport` / `DreamCandidate` | 梦境运行报告 / 候选记忆 |
| `ScoreBreakdown` | 6 信号评分明细 |

## Scope 设计：群聊 vs 私聊的记忆隔离

记忆系统通过 `Scope` 区分数据归属。核心原则：**Channel scope 记录会话上下文，User scope 记录用户画像**。

### Scope 类型

| Scope | 含义 | 典型用途 |
|-------|------|---------|
| `ChannelScope` | 会话/群组级 | 群聊上下文（"这个群里聊了什么"） |
| `UserScope` | 用户级（跨会话） | 用户画像（"这个人是谁、偏好什么"） |
| `BotScope` | Bot 级 | Bot 自身知识 |
| `GlobalScope` | 全局 | 全局常识 |

### 写入策略（MemoryWriteStage）

群聊场景下，一条记忆**同时写入两个 scope**：

```
群聊消息 (Channel ≠ UserID):
  → ChannelScope(groupID)   ← 会话上下文，Bot 能回忆"群里发生了什么"
  → UserScope(userID)       ← 用户画像原料，Profiler 据此独立构建每个人画像

私聊/直接互动 (Channel == UserID):
  → 只写一次（两者指向同一人，避免冗余）
```

> **注意**：切勿将群组 ID 作为 UserID 写入。`UserID` 必须始终是**实际发言者的 ID**。

### 读取策略（ContextManager）

默认检索 `[ChannelScope, UserScope]` 两个维度，合并后注入 LLM：
- ChannelScope 提供"当前会话的上下文"
- UserScope 提供"当前用户的跨会话画像"

### Channel 适配指南

各 channel 实现**必须正确设置** `core.Message.Channel` 和 `core.Message.UserID`：

| Channel | 场景 | Channel 值 | UserID 值 |
|---------|------|-----------|-----------|
| Telegram | 群组 | `chatID`（群组 ID） | 发言者 user ID |
| Telegram | 私聊 | `chatID`（= 发言者 ID） | 发言者 user ID |
| Misskey | timeline | `misskey:timeline`（共享社交空间） | 发言者 note.User.ID |
| Misskey | mention/reply | 发言者 note.User.ID（1:1 对话） | 发言者 note.User.ID |

**反模式**：将群聊的 `Channel` 设为发言者个人 ID，导致所有人记忆混在同一个桶里，画像无法区分。

### Profiler 画像提取

Dreaming 系统的 `discoverScopes()` 会自动发现所有活跃 scope（包括 `user:*` 和 `channel:*`），然后对每个 scope 独立运行 Profiler。因此：

- `user:A` scope 的记忆 → 提取用户 A 的专属画像
- `user:B` scope 的记忆 → 提取用户 B 的专属画像
- `channel:group1` scope 的记忆 → 提取群组上下文摘要（不是任何个人的画像）

## Agent 工具

单一 `memory` 工具，通过 `action` 参数分发到不同操作。
显著减少 LLM 上下文中的工具 schema token 开销。

| action | 说明 | 特性 |
|--------|------|------|
| `add` | 添加记忆 | 威胁扫描、think 过滤 |
| `replace` | 替换记忆 | **子串匹配**（非 ID） |
| `remove` | 删除记忆 | 子串匹配或 memory_id |
| `search` | 搜索记忆 | 关键词/分类/limit |
| `recent` | 获取最近记忆 | 按时间倒序 |
| `count` | 查询记忆数量 | |
| `batch` | **批量原子操作** | add+replace+remove 一次完成 |

```go
repo := memory.NewMemoryRepository()
memory.RegisterTools(toolMgr, memory.DefaultToolConfig(repo))
```

## 快照刷新模式

```go
// ModeLive（默认）：工具写入后自动刷新，下一轮系统提示即包含最新记忆
snap := memory.NewSnapshot()

// ModeFrozen：整个会话冻结，保护 prefix cache
snap := memory.NewSnapshot(memory.SnapshotConfig{Mode: memory.ModeFrozen})

// ModePeriodic：每 N 轮刷新一次，平衡 freshness 和开销
snap := memory.NewSnapshot(memory.SnapshotConfig{
    Mode:         memory.ModePeriodic,
    RefreshTurns: 10,
})
```

### 集成示例

```go
snapshot := memory.NewSnapshot() // 默认 ModeLive
snapshot.Init(ctx, repo, []memory.Scope{memory.ChannelScope("ch1"), memory.UserScope("user1")})

// 统一工具写入时自动标记快照为脏 → 下一轮自动刷新
cfg := memory.DefaultToolConfig(repo)
cfg.Snapshot = snapshot
memory.RegisterTools(toolMgr, cfg)

// 每轮对话构建系统提示前更新 section
section := snapshot.SnapshotPromptSection()
snapshot.UpdatePromptSection(ctx, section)
// section.Content 现在包含最新记忆
registry.Register(section)
```

## 梦境巩固系统

三相位后台记忆整理管线，受认知科学睡眠周期启发：

```
L0 工作记忆
    │
    ▼
┌──────────────────┐
│  Light（浅睡眠）   │ 摄取近期 L0 → LLM 提取候选 → Jaccard 去重
└────────┬─────────┘
         │
         ▼
┌──────────────────┐
│  REM（快速眼动）   │ 主题聚类 → 模式识别 → 增强 REM 信号
└────────┬─────────┘
         │
         ▼
┌──────────────────┐
│  Deep（深睡眠）    │ 6 信号加权评分 → 3 门控筛选 → 晋升到 L1
└────────┬─────────┘
         │
         ▼
    L1 长期记忆 + 梦境日记
```

### 6 信号加权评分

| 信号 | 权重 | 含义 |
|------|------|------|
| Relevance | 0.30 | 检索召回质量 |
| Frequency | 0.24 | Light 阶段累积命中次数 |
| Diversity | 0.15 | 触发召回的不同查询数 |
| Recency | 0.15 | 时间衰减新鲜度（14 天半衰期） |
| Consolidation | 0.10 | 跨多次梦境重现强度 |
| Richness | 0.06 | 内容具体性和原子性 |

### 3 门控阈值

候选必须**同时通过**才能晋升：`minScore ≥ 0.8`、`recallCount ≥ 3`、`uniqueQueries ≥ 3`

### 集成示例

```go
cfg := memory.DefaultDreamConfig()
cfg.Enabled = true
cfg.Schedule = "0 3 * * *" // 凌晨 3 点

dm := memory.NewDreamManager(cfg, tieredManager, llmProvider, tp, logger)

// 手动触发
report, err := dm.Run(ctx)

// 或注册到 cron 调度器
// cronScheduler.RegisterFunc("dreaming", cfg.Schedule, func() { dm.Run(ctx) })

// 查看运行报告
fmt.Println("promoted:", report.DeepPromoted)

// 查看梦境日记（人类可读审计日志）
for _, entry := range dm.DreamDiary() {
    fmt.Println(entry)
}
```

### 设计原则

- **严格分离**：仅 Deep 相位写入 L1，噪声永不污染长期记忆
- **证据驱动**：候选必须积累足够信号通过门控
- **可审查**：每次梦境产出可读日志 + 报告
- **可选**：默认禁用，通过配置开启
- **全链路可观测**：每个相位创建 OTel span，日志携带 traceID（后台任务通过 `traceid.NewContext` 继承）

### 规则降级

LLM 不可用时自动降级：

| 阶段 | 正常模式 | 降级模式 |
|------|---------|---------|
| Light | LLM 提取候选事实 | 直接取原始片段（长度 ≥10） |
| REM | LLM 主题聚类 | 按 category 分组 |
| Deep | — | 无降级（评分纯计算） |

### Per-Bot 配置

梦境巩固通过 config 系统按 Bot 独立配置：

| 键 | 默认值 | 说明 |
|----|--------|------|
| `bot.<botID>.dreaming.enabled` | `false` | 是否启用梦境巩固 |
| `bot.<botID>.dreaming.schedule` | `0 3 * * *` | cron 表达式（每天凌晨 3 点） |

配置示例（`.env` 文件）：
```ini
bot.mybot.dreaming.enabled=true
bot.mybot.dreaming.schedule=0 4 * * *
```

启用后，Bot 启动时自动创建 `DreamManager` + `cron.Scheduler`，按计划定时执行梦境巩固。Bot 关闭时自动停止调度器。

## 子包

- `agent/storage` — 记忆持久化仓储（SQLite）

## 可观测性

### TraceID 传播

记忆系统的后台任务（SyncAll、Prefetch、BackgroundSync）通过 `util/traceid` 确保日志可关联：

```go
// 后台 context 创建带 traceID 的 context
ctx := traceid.NewContext(context.Background())

// 日志通过 traceid.WithLoggerFrom 自动携带 traceID
logger := traceid.WithLoggerFrom(ctx, m.logger)
logger.Warnw("provider sync_turn failed", ...)
```

### SyncExecutor panic 恢复

`SyncExecutor` 的 worker 在 panic 时自动恢复，并通过注入的 logger 记录 panic 值和完整调用栈：

```go
executor := NewSyncExecutor(16)
executor.SetLogger(logger) // 注入后 panic 会被记录
```

### FlushPending 无竞态等待

`FlushPending` 使用 sentinel channel + `select`/`timeout` 替代 `time.Sleep` hack，确保后台任务真正完成后再返回，无数据竞态。

## 画像语义验证

`profiler_validation.go` 实现画像提取的质量控制：

### 双重验证策略

| 策略 | 条件 | 方法 |
|------|------|------|
| Embedding | 配置了 `EmbeddingProvider` | cosine 相似度（精确） |
| Jaccard | 无 embedding 依赖 | token 集合相似度（近似降级） |

验证逻辑：计算画像 vs 所有源记忆的**最大**相似度（max 而非 avg，因为画像可能只对应部分源记忆），通过 `MinValidationScore` 阈值的画像才保留。

### TF-IDF 聚类

画像提取前先对 L1 记忆做 TF-IDF + k-means 聚类（k = √N，上限 8），按聚类分组提取画像，避免主题混叠。
