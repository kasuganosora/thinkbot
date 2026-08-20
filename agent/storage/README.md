# storage — 记忆持久化仓储

agent 模块的持久化层实现（SQLite / GORM），是 DDD 端口-适配器模式中的**适配器**：
领域层（`agent/memory`）定义接口，本包提供实现，不反向依赖。

所有表模型集中定义在 `dao` 包（`EntryModel`、`WindowStateModel`），
通过 `dao.Migrate` 建表——**本包不做迁移**，传入的 `*gorm.DB` 必须已迁移。

## 功能

- **记忆存储**：实现 `memory.Repository`（`Store` 写入侧 + `Retriever` 查询侧）
- **多维检索**：按 Scope、Category、MinImportance、Text（LIKE 模糊）过滤，时间倒序返回
- **容量淘汰**：每个 Scope 超过 `MaxEntriesPerScope` 时异步淘汰最旧条目
- **语义压缩**：注入 `Window` + `Compactor` 后，scope 字符数超过 `Window.MemoryBudget()*3` 派生的预算（`CompressThreshold`，默认 0.85）时异步触发 `MemoryCompactor` 压缩后入库（取代截断），来源条目标记 archived
- **访问时间回写**：`Retrieve` 后异步批量更新 `LastAccessedAt`
- **窗口快照**：对话窗口状态的保存（upsert）与恢复
- **指标统计**：`Metrics()` 返回 `memory.RepositoryMetrics`
- **索引优化**：`dao.EntryModel` 对 scope、category、source、created_at、last_accessed_at 建有索引

## 关键类型

| 类型 | 说明 |
|------|------|
| `SQLiteRepository` | SQLite 记忆仓储，实现 `memory.Repository`（Store + Retriever）与 `memory.Replacer` |
| `SQLiteRepositoryConfig` | 配置：`MaxEntriesPerScope`（默认 1000）、`DefaultLimit`（默认 10）、`CompressThreshold`（默认 0.85）、`Window`、`Compactor` |
| `MemoryCompactor` | 压缩接口（`CompactScope(ctx, scope)`），`SQLiteCompactor` 为其实现 |
| `SQLiteCompactor` | LLM 语义压缩器：聚类合并相似条目（source=compactor）并归档来源，复用 `memory.ClusterMerge` |
| `WindowStateStore` | 窗口状态存储（Save / Load / Delete） |
| `WindowSnapshot` | 窗口快照：ScopeKey、UsedTokens、RoundCount、TotalInput/OutputTokens、Compressions |

## SQLiteRepository 方法

| 方法 | 说明 |
|------|------|
| `Append(ctx, entry)` | 追加记忆；自动补全 ID（`idgen.New("mem")`）与时间戳，并触发异步淘汰 |
| `Delete(ctx, scope, entryID)` | 按 ID 删除指定 scope 下的条目 |
| `Clear(ctx, scope)` | 清空指定 scope 的所有记忆 |
| `Retrieve(ctx, query)` | 按 `memory.Query` 条件检索 |
| `Replace(ctx, scope, deleteID, newEntry)` | 事务内原子替换（实现 `memory.Replacer`，允许复用同一 ID） |
| `Recent(ctx, scope, limit)` | 获取指定 scope 的最近 N 条 |
| `Count(ctx, scope)` | 统计 scope 下条目数 |
| `GetAllActive(ctx, scope)` | 返回该 scope 全部未归档条目（按时间升序，供压缩器读取） |
| `ArchiveByID(ctx, scope, entryID)` | 将条目标记为 archived（幂等，供压缩器归档来源） |
| `Metrics()` | 指标快照（总 scope 数、总条目数、写入/删除/检索计数） |

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
