# config — 配置管理

基于键值存储的动态配置系统，支持环境变量和 `.env` 文件加载。

## 功能

- **键值存储**：`Store` 提供运行时可读写的配置接口（Get/Set/Listen）
- **环境变量加载**：从 `.env` 文件和系统环境变量加载配置
- **配置监听**：支持注册变更回调，配置更新时自动通知
- **类型安全**：提供 `GetString`/`GetInt`/`GetBool` 等类型化访问方法
- **密钥管理**：敏感配置（API Key 等）的加密存储
- **Typed Builder**：`Builder` 从 Store 构建结构化配置对象（LLM/Bot/Channel/Engagement/Workflow/Dreaming 等）

## 关键类型

| 类型 | 说明 |
|------|------|
| `Store` | 配置存储（线程安全） |
| `Config` | 配置项（Key/Value/Description） |
| `Builder` | 从 Store 构建 typed 配置对象（LLM/Bot/Dreaming 等） |
| `MetaSpec` | 配置项元数据（用于前端设置界面注册） |

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

## Typed Builder

`Builder` 提供 typed 配置读取方法，自动填充默认值：

```go
builder := config.NewBuilder(store, logger)

// LLM 配置
model, ok := builder.GetLLMModel("main") // → ModelDef
assign := builder.GetBotLLMAssignment("mybot") // → BotLLMAssignment

// Bot 设置
settings := builder.GetBotSettings() // → BotSettings

// 梦境巩固配置（per-bot）
dreamCfg := builder.GetDreamingConfig("mybot") // → DreamingConfig
builder.SetDreamingConfig(ctx, "mybot", dreamCfg) // 持久化

// Channel 配置
channels := builder.GetChannelConfigs() // → []ChannelConfig

// Engagement / Workflow 配置
engCfg := builder.GetEngagementConfig() // → EngagementConfig
wfCfg := builder.GetWorkflowConfig() // → WorkflowConfig
```

### 配置键命名约定

| 前缀 | 示例 | 说明 |
|------|------|------|
| `llm.<id>` | `llm.main` | LLM 模型定义（JSON） |
| `bot.<id>.<role>` | `bot.mybot.main` | Bot 的 LLM 角色分配 |
| `bot.<id>.dreaming.<sub>` | `bot.mybot.dreaming.enabled` | 梦境巩固配置（per-bot） |
| `bot.<id>.timezone` | `bot.mybot.timezone` | Bot 独立时区 |
| `channel.<name>.<prop>` | `channel.mk.token` | Channel 配置 |
| `tools.<id>.policy` | `tools.mybot.policy` | 工具权限策略（JSON） |
