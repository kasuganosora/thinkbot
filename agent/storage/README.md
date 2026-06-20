# storage — 记忆持久化仓储

基于 SQLite（GORM）的记忆和窗口状态持久化实现，实现 `memory.MemoryRepository` 和 `WindowStateStore` 接口。

## 功能

- **记忆存储**：CRUD 操作，支持按 Scope、层级、时间范围查询
- **窗口快照**：对话窗口状态的保存与恢复
- **自动迁移**：启动时自动创建/更新表结构
- **索引优化**：对 Scope、CreatedAt 等高频查询字段建立索引

## 关键类型

| 类型 | 说明 |
|------|------|
| `SQLiteRepository` | SQLite 记忆仓储，实现 `memory.MemoryRepository` |
| `WindowStateStore` | 窗口状态存储 |
| `WindowSnapshot` | 窗口快照（ScopeKey/UsedTokens/Messages） |

## 使用示例

```go
repo := storage.NewSQLiteRepository(db)
_ = repo.Append(ctx, memory.Entry{
    Scope:   memory.ChannelScope("chat-1"),
    Content: "用户喜欢 Go 语言",
})
entries, _ := repo.Recent(ctx, memory.ChannelScope("chat-1"), 10)
```
