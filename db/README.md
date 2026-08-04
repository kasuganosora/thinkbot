# db — 数据库初始化

SQLite 数据库连接的打开与 GORM 初始化。

## 功能

- 通过 GORM + `gorm.io/driver/sqlite` 打开 SQLite 连接
- DSN 自动附加 PRAGMA 参数，防止并发锁死：
  - `_journal_mode=WAL` — 启用 Write-Ahead Logging，允许并发读写
  - `_busy_timeout=5000` — 遇到锁时最多等待 5 秒，而非立即返回 `SQLITE_BUSY`
- 可选注入自定义 GORM logger

## 导出函数

| 函数 | 说明 |
|------|------|
| `OpenSQLite(path string) (*gorm.DB, error)` | 使用默认 GORM 配置打开数据库 |
| `OpenSQLiteWithLogger(path string, logger gormlogger.Interface) (*gorm.DB, error)` | 打开数据库并指定 GORM logger |

## 使用示例

```go
gdb, err := db.OpenSQLite("./thinkbot.db")
if err != nil {
    log.Fatal(err)
}
// gdb 传入 dao.Migrate 建表后，即可在各处使用
if err := dao.Migrate(gdb); err != nil {
    log.Fatal(err)
}
```
