# memory — 多层记忆系统

实现四层记忆架构（L0 工作记忆 / L1 长期记忆 / L2 场景记忆 / L3 画像），支持自动巩固、快照刷新、上下文展开、笔记过滤、工具输出过滤、跨平台记忆镜像、历史对话回灌和可插拔 Provider。包内组件分两类：生产接线使用的核心（`TieredStore`、`memory` 工具、Snapshot、Window、dreaming、backfill），以及独立可选的扩展点（Expander / ContextPacker / SemanticCompactor / LLMCompressor / FormationPipeline / Provider 栈，见「记忆整理组件」一节）。

## 功能

- **分层存储**：L0 工作记忆（近期对话窗口）→ L1 长期记忆（事实/偏好）→ L2 场景记忆（事件快照）→ L3 画像（用户性格特征 + Bot 自我认知）。默认容量 L0 200 / L1 500 / L2 50 / L3 20 条，L0 TTL 14 天（`DefaultTierConfigs`）。L2 为可选扩展点：仓库未提供 Aggregator 实现，生产中恒为空，实际管线为 L0→L1→L3
- **自动巩固**：后台定期将 L0 记忆通过 LLM 巩固为 L1（去重/合并/更新）
- **快照刷新**：可配置刷新策略（实时/冻结/定期），默认实时模式让 bot 始终看到最新记忆
- **上下文展开**：从 L1/L2 检索相关记忆注入 LLM 上下文
- **上下文隔离**：系统标注包裹记忆上下文，防止记忆内容被误认为用户输入
- **笔记过滤**：自动识别值得记住的信息（Think Filter）
- **工具输出过滤**：记忆写入前剥离工具调用输出（搜索结果/文件内容/API JSON 等），避免冗长低价值内容挤占记忆预算（见 Strip 系列一节）
- **窗口管理**：基于 token 计数的滑动窗口，自动压缩旧消息。窗口耗尽（`Available() <= 0`）时 `AssembleContext` 直接早退、不再注入记忆（避免无预算时仍灌记忆）；压缩判定用 `ShouldCompress`（80% 阈值提前压缩，而非等到 100% 才截断）。默认值对齐 GLM-5.2/5.3：上下文 1M、`OutputReserve` 128000（生产路径优先用主模型 MaxTokens / contextLength）、`MemoryBudgetRatio` 0.15、`MaxMemoryTokens` 4096；运行值集中由 config 模块的 `memorywindow.*` 键提供，可在前端「系统配置」调整
- **可插拔 Provider**：`MemoryProvider` 接口 + `ProviderManager` 编排器 + `ProviderRegistry` 工厂注册表（延迟实例化、singleflight 实例缓存、健康检查），支持外部记忆后端
- **后台同步**：单 worker 串行写入，不阻塞对话循环；带 debounce 和 drain 超时
- **预取缓存**：每轮结束后异步预取下一轮记忆（`ProviderManager.PrefetchAll` / `QueuePrefetchAll`），命中时零延迟
- **写入威胁扫描**：记忆写入时自动检测注入/渗出模式（复用 `prompt_scan.go`）
- **批量原子操作**：单次 `batch` 调用执行 add+replace+remove，按最终字符预算验证
- **跨平台记忆镜像**：`ToolConfig.BotID` 非空时，channel 作用域记忆在 add/replace/remove 时自动同步镜像到 BotScope（镜像 ID 带 `xch:` 前缀，正文带 `[<channel>]` 来源标注），使任意频道会话可召回其他平台的活动
- **历史对话回灌**：`BackfillFromChatHistory` 从 `user_message_events` 事件流（append-only，权威数据源）一次性 bootstrap 补灌 L0，水位线 `bot.<id>.memory.backfill.event_watermark` 独立持久化，清空记忆表后重启也不会回潮
- **Bot 自我画像**：`BotProfileProfiler` 从 BotScope 的 L1+L2 蒸馏 Bot 量化人格（energy_level / patience / preferred_topics / verbosity / personality）写入 L3，由 dreaming 结束时触发
- **梦境巩固**：三相位后台记忆整理管线（Light → REM → Deep），证据驱动评分门控，从短期信号提取长期知识
- **画像语义验证**：提取的用户画像通过 embedding cosine 相似度（或 Jaccard 降级）验证与源记忆的一致性
- **可观测性**：后台任务 panic 恢复 + 日志记录，`traceid` 贯穿请求级到后台任务级

## 关键类型

| 类型 | 说明 |
|------|------|
| `TieredManager` | 分层记忆管理器（核心入口，含 L0→L1 自动巩固与 L2→L3 画像提取） |
| `TieredStore` | 分层存储后端（tier×scope 分桶，write-through 持久化到 `tiered_memories` 表） |
| `Entry` / `TieredEntry` | 记忆条目 / 带层级标识的条目 |
| `Scope` / `Query` | 记忆作用域（Channel/User/Bot/Global）/ 检索请求 |
| `Store` / `Retriever` / `Repository` / `Replacer` | 写入 / 检索 / 读写复合 / 原子替换接口（CQRS 读写分离） |
| `ContextManager` / `ContextBuilder` | 记忆上下文组装（`AssembleContext`）/ 格式化为 LLM 可消费文本 |
| `LLMConsolidator` / `RuleConsolidator` | LLM 驱动 / 规则降级的记忆巩固器 |
| `Compressor` / `LLMCompressor` / `CompressedBlock` | 窗口超限压缩接口与实现 |
| `Aggregator` | L1→L2 场景聚合接口（仅定义，本仓库无实现） |
| `Expander` / `IDRetriever` / `ExtractRefIDs` | 按 `[ref:ID]` 回溯加载压缩摘要引用的原文 |
| `ThinkFilterStore` | Store 装饰器：写入前用 `StripThinking` 系列函数清理 `<think>`/内部标签 |
| `Window` / `WindowMetrics` / `WindowState` | 对话窗口管理器（`RecordUsage` 覆盖写最新一轮 prompt input，非累计；`Reset` 同步清零累计指标）/ 运行指标 / 状态快照 |
| `LLMProfiler` / `Profiler` / `ProfileItem` | 用户画像提取器（TF-IDF 聚类 + 语义验证）/ 接口 / 条目 |
| `BotProfileProfiler` / `BotProfileResult` | Bot 自我画像提取器（BotScope L1+L2 → L3 量化人格）/ 结果 |
| `Snapshot` / `SnapshotConfig` / `RefreshMode` | 快照管理器（实时/冻结/定期刷新） |
| `MemoryProvider` / `ProviderManager` | 可插拔记忆后端接口 / Provider 编排器（`SyncAll` / `PrefetchAll` / `FlushPending`） |
| `ProviderRegistry` / `ProviderFactory` / `ProviderEntry` | Provider 工厂注册表（延迟创建 + singleflight 实例缓存 + 健康检查） |
| `SyncExecutor` / `BackgroundSyncManager` | 后台同步执行器（单 worker 串行，panic 恢复）/ 后台写入协调器（debounce 默认 5s） |
| `PrefetchManager` | 预取缓存管理器 |
| `DreamManager` / `DreamConfig` | 梦境巩固管线（Light → REM → Deep） |
| `DreamReport` / `DreamCandidate` / `DreamPhase` | 梦境运行报告 / 候选记忆 / 相位 |
| `ScoreBreakdown` | 6 信号评分明细 |
| `FormationPipeline` / `FormationConfig` / `FactItem` / `FactDecision` | 对话后即时记忆提取管线（Extract → Gather → Decide → Apply） |
| `ContextPacker` / `PackEntry` | 精细上下文打包器（字符预算 + anti-lost-in-the-middle 重排序） |
| `SemanticCompactor` / `CompactionReport` / `ClusterMerge` | 语义记忆压缩器（LLM 聚类合并 L1 相似条目，旧条目归档不删除） |
| `MemoryStage` / `MemoryWriteStage` | Pipeline 记忆读取/写入 Stage（检索注入 `memory.context` / ActionNote 转存记忆） |
| `MemoryRepository` / `RepositoryMetrics` | 内存记忆仓储（`Repository`+`Replacer` 实现，容量上限 + metrics） |
| `MultiStore` / `TieredStoreAdapter` | 写入广播到多后端（L0 重复写入 30min 去重）/ 将 `TieredStore` 适配为 `Store` |
| `ToolOutputFilterStore` | Store 装饰器：写入前剥离工具调用输出 |
| `NoteWriterAdapter` | 将 Note 写入流适配到 `Store`（ActionNote → L0） |
| `BackfillMessage` / `UserMessageSource` | 回灌消息 / 入站用户消息事件流抽象 |
| `EntryResult` | `memory` 工具返回的单条记忆格式化结果 |

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

Dreaming 系统的 `discoverScopes()` 会自动发现所有活跃 scope（包括 `user:*`、`channel:*`、`bot:*`），但其中仅 `bot:*` scope 会进入画像提取（`extractBotProfiles` 注入的 `BotProfileProfiler`，蒸馏 Bot 的 L3 自我认知，并经 `SetOnBotProfileUpdated` 回调通知调用方）。用户画像由 `TieredManager.ExtractProfile`（配 `LLMProfiler`）按 scope 独立提取，与 dreaming 调度无关。因此：

- `user:A` scope 的记忆 → 提取用户 A 的专属画像
- `user:B` scope 的记忆 → 提取用户 B 的专属画像
- `channel:group1` scope 的记忆 → 提取群组上下文摘要（不是任何个人的画像）

## 核心接口

```go
// 存储与检索（CQRS 读写分离）
type Store interface {         // 写入侧
    Append(ctx, entry) error
    Delete(ctx, scope, entryID) error
    Clear(ctx, scope) error
}
type Retriever interface {     // 检索侧
    Retrieve(ctx, query Query) ([]Entry, error)
    Recent(ctx, scope, limit) ([]Entry, error)
    Count(ctx, scope) (int, error)
}
type Repository interface { Store; Retriever }   // 读写复合
type Replacer interface {     // 原子替换（Delete+Append 合并为单锁内操作）
    Replace(ctx, scope, deleteID, newEntry) error
}
```

`MemoryRepository` 同时实现 `Repository` + `Replacer`，因此跨平台镜像在走 `MemoryRepository` 时使用 `Replace` 就地更新，其他后端退化为追加。`TieredStoreAdapter` 将 `TieredStore` 适配为 `Store`，`MultiStore` 把写入广播到多个后端（L0 层重复写入在 30 分钟 TTL 内去重）。

## Agent 工具

单一 `memory` 工具，通过 `action` 参数分发到不同操作。
显著减少 LLM 上下文中的工具 schema token 开销。

| action | 说明 | 特性 |
|--------|------|------|
| `add` | 添加记忆 | 威胁扫描、think/工具输出过滤、空 mention 前缀归一化、跨平台镜像 |
| `replace` | 替换记忆 | **子串匹配**（非 ID），同步更新镜像 |
| `remove` | 删除记忆 | 优先按 `id`/`memory_id` 精确删除（可跨 scope 解析归属）；`old_text` 子串匹配在本 scope 未命中时会提示其他 scope 的 `id`/`scope` 供重试；均同步删除镜像 |
| `search` | 搜索记忆 | 关键词/分类/limit |
| `recent` | 获取最近记忆 | 按时间倒序 |
| `count` | 查询记忆数量 | |
| `batch` | **批量原子操作** | add+replace+remove 一次完成，逐条威胁扫描 |

默认字符预算：`MaxMemoryChars` 2200（channel 记忆）/ `MaxUserChars` 1375（user 记忆），条目分隔符 `\n§\n`。

```go
repo := memory.NewMemoryRepository()
cfg := memory.DefaultToolConfig(repo)
cfg.BotID = botID // 开启跨平台镜像（channel 记忆同步到 BotScope）
memory.RegisterTools(toolMgr, cfg)
```

## 跨平台记忆镜像

生产装配中 `ToolConfig.BotID` 非空时启用：channel 作用域的记忆在 add/replace/remove/batch 时同步镜像一份到 `BotScope`，任意频道的会话召回时都能看到其他平台的活动。

- 镜像条目 ID = `xch:` + 原始 channel 条目 ID（确定性 ID，增改删稳定定位，不重复累积）
- 镜像正文以 `[<channel>]` 前缀标注来源频道，避免跨频道串台时丢失上下文
- 仓储实现 `Replacer` 接口时走 `Replace` 就地更新，否则退化为追加
- 移除原始记忆（子串或 memory_id）时同步删除对应镜像

## 历史对话回灌（backfill）

L0 工作记忆原本只由实时聊天产生的 ActionNote 写入，历史消息从未进入记忆系统。回灌在 Bot 启动时补齐这个 backlog，数据源是 **`user_message_events` 事件流**（运行期由 NoteCapture 实时写入，历史部分由 `SeedUserMessageEvents` 从 `chat_messages` 一次性幂等补齐），而非原始渠道存储表：

```go
// 事件流为空时先从 chat_messages 补齐（幂等，仅首次）
memory.SeedUserMessageEvents(ctx, db, botID, logger)

// L0 为空且无水位线时，把事件流消息灌入 L0
src := memory.NewDBUserMessageSource(db)
n, maxID, err := memory.BackfillFromChatHistory(ctx, memStore, src, botID, 0, logger)
// 持久化 maxID 到 bot.<botID>.memory.backfill.event_watermark，阻断未来回潮
```

要点：

- **一次性 bootstrap**：水位线（`config.BotMemoryBackfillEventWatermarkKey` → `bot.<botID>.memory.backfill.event_watermark`）独立于 `tiered_memories` 持久化，回灌完成后清空记忆表重启也不会回潮；要强制重灌只需删除水位线键
- **scope**：优先按消息的 Channel 写 `ChannelScope`，否则按 UserID 写 `UserScope`；`Source="chat_history"`，Metadata 记录原始事件 id / message_id 便于追溯
- **CreatedAt 设为当前时间**，否则会被 dreaming 的 `LookbackDays` / `ActiveThresholdHours` 门槛判为过期
- **总开关**：`memory.backfill.enabled`（默认 true），per-bot 可用 `bot.<id>.memory.backfill.enabled` 覆盖
- **为何不做持续增量**：运行期 NoteCapture 已把新用户消息实时写入 L0 与事件流，每轮按水位线重跑会把同一条消息重复追加，因此生产调用点以 `sinceID=0` 调用一次、以水位线作「已完成」标志

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

快照在渲染时对每条记忆做威胁扫描，命中注入/渗出模式的条目会被剔除并注入告警标注。

## 记忆整理组件

除梦境管线外，包内还有几组整理/组装组件（多为可选扩展点，生产路径按需接线）：

### FormationPipeline — 对话后即时提取

参考 Memoh 的 OnAfterChat Formation Pipeline：对话结束 → `ProcessTurn()` → LLM 从本轮 user+assistant 提取事实候选（`MaxFactsPerTurn` 5，`MinContentLen` 20）→ 搜索现有 L1 去重（`DedupScopeLimit` 20）→ LLM 决策 ADD/UPDATE/SKIP → 应用。与 Consolidator 的区别：Consolidator 是 L0 积累到阈值后的批量巩固，Formation 是每轮对话后的即时提取。

### ContextPacker — 精细上下文打包

四阶段打包：贪心填充 → 压缩腾位 → 重分配 → **anti-lost-in-the-middle 重排序**（LLM 注意力呈 U 型曲线，把最高分条目放到首尾）。默认预算：总字符 1800、单条 40~360、目标 8 条、过采样率 3。`OverfetchRatio` 供调用方设定检索 limit（检索 `TargetItems × OverfetchRatio` 条候选再交给 `Pack` 筛选）。

### SemanticCompactor — 语义记忆压缩

LLM 将相似 L1 记忆聚类合并为信息密集的摘要，旧记忆**归档而非删除**，摘要保留来源 EntryIDs 供回溯。与 `Compressor`（窗口超限时的临时压缩，产出 `CompressedBlock`）不同，其结果直接替换 L1 条目。生产路径的 SQLite 存储压缩由 `agent/storage.SQLiteCompactor` 承担（复用本包 `PreprocessContent` 做前置压缩）。

### precompress — LLM 摘要前的确定性前置压缩

`PreprocessContent` 在把记忆整批交给 LLM 前，先用零延迟规则瘦一轮（JSON 压平、重复行去重、超大数组抽样保留计数），并保护 must-keep 脆弱 token（UUID / 文件路径 / URL / IP / 错误码 / 金额）；压缩后反而变大则回滚。只减冗余字节，不做有损语义取舍。

### Expander — 引用回溯展开

压缩摘要中以 `[ref:ID]` 引用原始记忆。`Expander` 按这些 ID 从 Retriever 加载原文注入当前上下文，供 LLM 需要细节时展开；`ExtractRefIDs(text)` 从摘要文本中解析引用 ID 列表。独立扩展点，生产管线未接线。

### Strip 系列过滤器

`think_filter.go` / `tool_filter.go` 提供写入前与上下文构建前的清理函数族：`StripThinkTags` / `StripThinking`（`<think>` 标签）、`StripInternalTags` / `StripInternalState`（内部状态标签）、`StripReasoningArray`（reasoning 数组）、`StripToolMessages` / `StripToolOutput`（工具调用输出）、`StripContextMarkers`（系统标注）。对应的 Store 装饰器为 `ThinkFilterStore` 和 `ToolOutputFilterStore`，可叠加使用。

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

权重常量定义于 `dreaming.go`，合计 = 1.0：

| 信号 | 权重 | 含义 |
|------|------|------|
| Relevance | 0.10 | 检索召回质量 |
| Frequency | 0.30 | Light 阶段累积命中次数 |
| Diversity | 0.05 | 触发召回的不同查询数 |
| Recency | 0.25 | 时间衰减新鲜度（14 天半衰期） |
| Consolidation | 0.20 | 跨多次梦境重现强度 |
| Richness | 0.10 | 内容具体性和原子性 |

### 3 门控阈值

候选需通过 `DeepPhaseConfig` 中配置的门控才能晋升。默认值（见 `DefaultDreamConfig`）：

- `MinScore`：综合评分阈值，默认 **0.45**。
- `MinRecallCount`：召回次数门控，默认 **0**（关闭）。
- `MinUniqueQueries`：唯一查询数门控，默认 **0**（关闭）。

> 注：召回/查询类门控默认关闭。这些信号仅在候选晋升后召回时才会累积，作为硬门控会造成死锁，因此保留配置项供将来按需启用。另有 `MinREMHits`（默认 0）、`MaxPromotions`（默认 10）、`RecencyHalfLifeDays`（默认 14）、`MaxAgeDays`（默认 30）、`ActiveThresholdHours`（默认 24）、Light `LookbackDays`（默认 2）、REM `LookbackDays`（默认 7）等可调项。已晋升候选不再参与后续 REM 聚类与 Deep 评分（避免重复固化）。

### 集成示例

```go
cfg := memory.DefaultDreamConfig()
cfg.Enabled = true
cfg.Schedule = "0 3 * * *" // 凌晨 3 点

dm := memory.NewDreamManager(cfg, tieredManager, llmProvider, tp, logger)

// 注入 Bot 自我画像提取器（可选）
dm.SetBotProfiler(memory.NewBotProfileProfiler(
    memory.BotProfileProfilerConfig{}, tp, logger))
dm.SetOnBotProfileUpdated(func(botID string, r *memory.BotProfileResult) {
    // 画像更新后的联动（如刷新 system prompt）
})

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
| Light | LLM 提取候选事实 | 直接取原始片段（长度 ≥10），同样排除 assistant 发言与易失指标（`volatileMetricRe`） |
| REM | LLM 主题聚类 | 按 category 分组；已聚类候选（`Theme` 非空）复用既有主题，不每晚重跑（避免主题抖动、REMHits 不稳定） |
| Deep | — | 无降级（评分纯计算） |
| Formation | LLM 决策 ADD/UPDATE/SKIP | 决策失败时全部按 ADD 写入 |

（FormationPipeline 不在 dreaming 调度内，此处一并列出供对照。）

### Per-Bot 配置

梦境巩固通过 config 系统按 Bot 独立配置：

| 键 | 默认值 | 说明 |
|----|--------|------|
| `bot.<botID>.dreaming.enabled` | `false` | 是否启用梦境巩固 |
| `bot.<botID>.dreaming.schedule` | `0 3 * * *` | cron 表达式（每天凌晨 3 点） |
| `memorywindow.max_context_tokens` | `1000000` | 模型上下文窗口（token） |
| `memorywindow.reserved_tokens` | `2000` | 系统 prompt / 工具定义预留 |
| `memorywindow.output_reserve` | `128000` | LLM 输出预留 |
| `memorywindow.budget_ratio` | `0.15` | 记忆可用窗口比例 |
| `memorywindow.max_memory_tokens` | `4096` | 记忆注入硬上限 |
| `memorywindow.compress_threshold` | `0.8` | 压缩触发阈值比例 |

以上 `memorywindow.*` 为全局键（`config/keys.go`），生产装配时 `MaxContextTokens` 优先取主模型 `ContextLength`、`OutputReserve` 优先取主模型 `MaxTokens`，未配置时回退全局默认值。

配置示例（`.env` 文件）：
```ini
bot.mybot.dreaming.enabled=true
bot.mybot.dreaming.schedule=0 4 * * *
```

启用后，Bot 启动时自动创建 `DreamManager` + `cron.Scheduler`（经 `agent/bot.DreamExecutor` 桥接），按计划定时执行梦境巩固，并用 `DefaultDreamConfig` 补齐生产接线未设置的相位默认值（`ActiveThresholdHours` 24、Light `LookbackDays` 2、REM `LookbackDays` 7 / `MinPatternStrength` 0.75、`MaxPromotions` 10、`JaccardThreshold` 0.9）。Bot 关闭时自动停止调度器。

### Bot 自我画像蒸馏

dreaming 注入 `BotProfileProfiler` 后（`SetBotProfiler`），每次运行末尾对活跃的 `bot:*` scope 提取自我画像并写入 L3；`SetOnBotProfileUpdated` 回调可用于画像更新后的联动（如刷新 system prompt）。用户画像不走这条链路（见上文 Profiler 一节）。

## 子包

- `agent/storage` — 记忆持久化仓储（SQLite，含 `SQLiteCompactor` 语义压缩）

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

`SyncExecutor` 的 worker 在 panic 时自动恢复，并通过注入的 logger 记录 panic 值和完整调用栈。默认任务队列 `BufferSize` 16，`BackgroundSyncConfig.SyncDebounce` 5s（同 scope 最小同步间隔）：

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

验证逻辑：计算画像 vs 所有源记忆的**最大**相似度（max 而非 avg，因为画像可能只对应部分源记忆），通过 `MinValidationScore` 阈值（默认 0.15，`LLMProfilerConfig`）的画像才保留。未达 `MinValidationScore × 2/3` 的条目直接丢弃，介于两档之间的标记为需要复核。

### TF-IDF 聚类

画像提取前先对 L1 记忆做 TF-IDF + k-means 聚类（k = √N，上限 8、下限 2），按聚类分组提取画像，避免主题混叠。confidence 校准规则（LLM prompt 约定）：反复确认的特质 > 0.8，单次观察 < 0.5。
