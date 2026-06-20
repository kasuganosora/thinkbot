# db — 数据库初始化

SQLite 数据库连接的初始化与管理。

## 功能

- 初始化 SQLite 连接（通过 GORM）
- 配置连接池参数
- 提供全局 `*gorm.DB` 实例

## 使用示例

```go
db, err := db.New("./thinkbot.db")
if err != nil {
    log.Fatal(err)
}
// db 即可在各 dao 中使用
```
