# storage — 记忆持久化仓储

agent 模块的持久化层实现（SQLite / GORM），是 DDD 端口-适配器模式中的**适配器**：
领域层（`agent/memory`）定义接口，本包提供实现，不反向依赖。

所有表模型集中定义在 `dao` 包（`EntryModel`、`WindowStateModel`、`TieredMemoryModel`），
通过 `dao.Migrate` 建表——**本包不做迁移**，传入的 `*gorm.DB` 必须已迁移。

## 文件结构

```
storage/
├── repository.go        # SQLiteRepository / WindowStateStore / 自动压缩与淘汰
├── sqlite_compactor.go  # SQLiteCompactor：LLM 语义压缩器
├── tiered_recall.go     # TieredL1Retriever / MergedRetriever：分层 L1 记忆召回
└── doc.go               # 包文档
```

## 功能

- **记忆存储**：实现 `memory.Repository`（`Store` 写入侧 + `Retriever` 查询侧）
- **多维检索**：按 Scope、Category、MinImportance、Text（LIKE 模糊）过滤，时间倒序返回
- **容量淘汰**：每个 Scope 超过 `MaxEntriesPerScope` 时异步淘汰最旧条目（`Append`/`Replace` 后均触发）
- **语义压缩**：注入 `Window` + `Compactor` 后，scope 字符数超过 `Window.MemoryBudget()*3` 派生的预算（`CompressThreshold`，默认 0.85）时异步触发 `MemoryCompactor` 压缩后入库（取代截断），来源条目标记 archived。同一 scope 有 5min 冷却与并发重入保护，避免无可合并项时每轮写入都打 LLM
- **手动压缩**：`CompactScope` 委派内部压缩器，供 `/compact` 命令按需触发（未配置 Compactor 时为空操作）
- **访问时间回写**：`Retrieve` 后异步批量更新 `LastAccessedAt`
- **窗口快照**：对话窗口状态的保存（upsert）与恢复
- **分层召回**：`TieredL1Retriever` 读取 `tiered_memories` 表 tier=1 的蒸馏知识，
  经 `MergedRetriever` 与原始笔记合并注入 recall stage
- **指标统计**：`Metrics()` 返回 `memory.RepositoryMetrics`
- **索引优化**：`dao.EntryModel` 对 scope、category、source、created_at、last_accessed_at 建有索引

## 关键类型

| 类型 | 说明 |
|------|------|
| `SQLiteRepository` | SQLite 记忆仓储，实现 `memory.Repository`（Store + Retriever）与 `memory.Replacer` |
| `SQLiteRepositoryConfig` | 配置：`MaxEntriesPerScope`（默认 1000）、`DefaultLimit`（默认 10）、`CompressThreshold`（默认 0.85）、`Window`、`Compactor` |
| `MemoryCompactor` | 压缩接口（`CompactScope(ctx, scope)`），`SQLiteCompactor` 为其实现 |
| `SQLiteCompactor` | LLM 语义压缩器：聚类合并相似条目（source=compactor）并归档来源，复用 `memory.ClusterMerge`；同一 scope 经 singleflight 串行化，超 `MaxInputEntries` 分批循环处理 |
| `TieredL1Retriever` | 从 `tiered_memories` 表读取 tier=1 记忆（梦境升华的长期知识），实现 `memory.Retriever` |
| `MergedRetriever` | 合并多个 `memory.Retriever`：按源顺序保留、按内容去重；单源失败跳过不影响其他源 |
| `WindowStateStore` | 窗口状态存储（Save / Load / Delete） |
| `WindowSnapshot` | 窗口快照：ScopeKey、UsedTokens、RoundCount、TotalInput/OutputTokens、Compressions |

## SQLiteRepository 方法

| 方法 | 说明 |
|------|------|
| `Append(ctx, entry)` | 追加记忆；自动补全 ID（`idgen.New("mem")`）与时间戳，并触发异步淘汰与预算压缩 |
| `Delete(ctx, scope, entryID)` | 按 ID 删除指定 scope 下的条目 |
| `Clear(ctx, scope)` | 清空指定 scope 的所有记忆 |
| `Retrieve(ctx, query)` | 按 `memory.Query` 条件检索 |
| `Replace(ctx, scope, deleteID, newEntry)` | 事务内原子替换（实现 `memory.Replacer`，允许复用同一 ID）；替换后同样触发淘汰与压缩 |
| `Recent(ctx, scope, limit)` | 获取指定 scope 的最近 N 条 |
| `Count(ctx, scope)` | 统计 scope 下条目数 |
| `CompactScope(ctx, scope)` | 手动触发该 scope 的语义压缩（委派内部 Compactor） |
| `GetAllActive(ctx, scope)` | 返回该 scope 全部未归档条目（按时间升序，供压缩器读取） |
| `ArchiveByID(ctx, scope, entryID)` | 将条目标记为 archived（幂等，供压缩器归档来源） |
| `Metrics()` | 指标快照（总 scope 数、总条目数、写入/删除/检索计数） |

## 分层召回（tiered_recall.go）

`TieredL1Retriever` 把梦境子系统升华到 `tiered_memories`（tier=1）的长期知识暴露为
`memory.Retriever`；`MergedRetriever` 将其与 `memory_entries` 的原始笔记合并注入召回：

```go
tieredL1 := storage.NewTieredL1Retriever(db)
recall := storage.NewMergedRetriever(tieredL1, memRepo) // L1 源排最前，预算截断时优先保留
```

- 合并策略：各源按传入顺序检索、先加入者优先保留（**高价值源放最前**，确保蒸馏知识
  在 Snapshot 字符预算截断时不被丢弃），按内容去重
- 单源检索失败属非致命，跳过该源（与 recall stage 容错语义一致）
- L1 只在真人对话轮次被召回（recall stage 在 lurk 只读消息上提前返回），不会回环

## 自动压缩行为

- 预算口径与 snapshot 渲染一致：`Window.MemoryBudget()*3`，user 类 scope 保持 2200/1375 比例；
  未注入 `Window` 表示不限制、不压缩
- 同一 scope 两次压缩至少间隔 5 分钟（冷却），且压缩进行中跳过重入
- 注入了 `Window` 但预算算出 0（如 `OutputReserve + ReservedTokens ≥ MaxContextTokens`）
  会打一次告警——此时自动压缩静默失效、记忆只增不减，表现为存储缓慢膨胀而非报错

## 使用示例

```go
// db 需先经过 dao.Migrate
repo := storage.NewSQLiteRepository(db, storage.SQLiteRepositoryConfig{
    MaxEntriesPerScope: 2000,
})

_ = repo.Append(ctx, memory.Entry{
    Scope:   memory.ChannelScope("chat-1"),
    Content: "用户喜欢 Go 语言",
    Source:  "conversation",
})

entries, _ := repo.Recent(ctx, memory.ChannelScope("chat-1"), 10)

// 窗口状态
ws := storage.NewWindowStateStore(db)
_ = ws.Save(ctx, storage.WindowSnapshot{ScopeKey: "chat-1", UsedTokens: 1200})
snap, _ := ws.Load(ctx, "chat-1") // 不存在时返回 nil, nil
```
