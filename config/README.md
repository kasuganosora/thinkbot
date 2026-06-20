# config — 配置管理

基于键值存储的动态配置系统，支持环境变量和 `.env` 文件加载。

## 功能

- **键值存储**：`Store` 提供运行时可读写的配置接口（Get/Set/Listen）
- **环境变量加载**：从 `.env` 文件和系统环境变量加载配置
- **配置监听**：支持注册变更回调，配置更新时自动通知
- **类型安全**：提供 `GetString`/`GetInt`/`GetBool` 等类型化访问方法
- **密钥管理**：敏感配置（API Key 等）的加密存储

## 关键类型

| 类型 | 说明 |
|------|------|
| `Store` | 配置存储（线程安全） |
| `Config` | 配置项（Key/Value/Description） |

## 使用示例

```go
store := config.NewStore(db)
store.Set("api.addr", ":8080")
addr := store.GetString("api.addr", ":3000")

// 监听变更
store.Listen("bot.system_prompt", func(key, value string) {
    logger.Info("config changed", key, value)
})
```
