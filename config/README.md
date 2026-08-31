# config — 配置管理

基于键值存储的动态配置系统，支持数据库持久化、`.env` 文件和环境变量。

## 功能

- **键值存储**：`Store` 提供运行时可读写的配置接口（Get/Set/OnChange）
- **多来源加载**：`.env` 文件、数据库（`config_settings` 表）、系统环境变量
- **配置监听**：`OnChange` 注册变更回调，值真正发生变化时同步通知
- **类型安全**：`GetString`/`GetInt`/`GetInt64`/`GetFloat64`/`GetBool`/`GetDuration`/`GetStringSlice`
- **元数据注册**：`RegisterMeta`/`RegisterMany` 声明分类与描述，供前端设置界面渲染
- **Typed Builder**：`Builder` 从 Store 构建结构化配置对象（LLM/Bot/Channel/Engagement/Workflow/Soul/Dreaming/ToolPolicy 等）

### 读取优先级（从高到低）

1. 运行时覆盖（`SetTemporary`）
2. `.env` 文件
3. 数据库缓存（`config_settings` 表，`Set`/`Reload` 后生效）
4. 操作系统环境变量（键名转换：`api.addr` → `API_ADDR`）
5. 调用方提供的默认值

## 关键类型

| 类型 | 说明 |
|------|------|
| `Store` | 配置存储（并发安全） |
| `Setting` | `dao.Setting` 的类型别名（Key/Value/Category/Description/UpdatedAt） |
| `Builder` | 从 Store 构建 typed 配置对象 |
| `MetaSpec` | 配置项元数据（Key/Category/Description） |

## 使用示例

```go
store := config.NewStore(db)
_ = store.Migrate()          // 建表（幂等）
_ = store.Reload(ctx)        // 从数据库加载缓存

_ = store.Set(ctx, "api.addr", ":8080")   // 持久化
addr := store.GetString("api.addr", ":8080")

// 监听变更（返回取消注册函数）
cancel := store.OnChange(func(key, oldVal, newVal string) {
    logger.Infow("config changed", "key", key, "old", oldVal, "new", newVal)
})
defer cancel()
```

fx 模块 `config.Module` 在 `OnStart` 时自动执行 `Migrate` + `Reload`；
`.env` 路径默认 `.env`，可通过环境变量 `CONFIG_FILE` 覆盖。

## Typed Builder

`Builder` 提供 typed 配置读取方法，自动填充默认值：

```go
builder := config.NewBuilder(store, logger)

// LLM 模型：从 provider.* 配置中按模型 ID 解析
model, ok := builder.GetLLMModel("gpt-4o")      // → ModelDef
assign := builder.GetBotLLMAssignment("mybot")  // → BotLLMAssignment{Main,Light,Vision}

// Bot / 基础设施
settings := builder.GetBotSettings()            // → BotSettings
dbPath := builder.GetDBPath()                   // 默认 "thinkbot.db"
level := builder.GetLogLevel()                  // 默认 "info"
wsDir := builder.GetWorkspaceDir()              // 默认 "data/workspaces"
loc := builder.GetTimezoneLocation()            // system.timezone → $TZ → time.Local

// 梦境巩固配置（per-bot）
dreamCfg := builder.GetDreamingConfig("mybot")
_ = builder.SetDreamingConfig(ctx, "mybot", dreamCfg)

// 其他
channels := builder.GetChannelConfigs()         // → []ChannelConfig
engCfg := builder.GetEngagementConfig()         // → EngagementConfig
wfCfg := builder.GetWorkflowConfig()            // → WorkflowConfig
soulCfg := builder.GetSoulConfig()              // → SoulConfig
policy := builder.GetToolPolicy("mybot")        // → ToolPolicyConfig
```

各配置组均有对应的 `DefaultXxxConfig()` 与 `XxxMetaSpecs()`；
`AllMetaSpecs()` 汇总全部元数据，`DefaultMap()` 返回默认值映射。

### 常用配置键与默认值

| 键 | 默认值 | 说明 |
|------|--------|------|
| `api.addr` | `:8080` | HTTP 监听地址 |
| `api.cors_origins` | 空 | 允许的 CORS 来源（逗号分隔），为空时仅允许 localhost |
| `api.cookie_secure` | `false` | Cookie 是否仅走 HTTPS |
| `api.chat_context_limit` | `20` | LLM 上下文加载的最大历史消息数 |
| `bot.temperature` | `0.7` | 采样温度 |
| `bot.max_tokens` | `4096` | 最大输出 token 数 |
| `bot.workers` | `4` | Bot 并发 worker 数 |
| `db.path` | `thinkbot.db` | SQLite 文件路径 |
| `log.level` | `info` | 日志级别 |
| `system.timezone` | 服务器本地时区 | IANA 时区标识符 |
| `workspace.dir` | `data/workspaces` | Bot 工作空间根目录 |
| `sandbox.backend` | `auto` | `auto`/`docker`/`local` |
| `sandbox.stuck_timeout` | `300` | 卡死看门狗阈值（秒） |
| `sandbox.timeout` | `0` | 单命令硬上限（秒），0=卡死阈值×3 |
| `soul.reload_interval` | `5s` | SOUL.md 热重载轮询间隔，0=禁用 |

### 配置键命名约定

| 前缀 | 示例 | 说明 |
|------|------|------|
| `provider.<name>` | `provider.openai` | Provider 定义（JSON，含 models 列表），LLM 模型由此解析 |
| `bot.<id>.<role>` | `bot.mybot.main` | Bot 的 LLM 角色分配（main/light/vision） |
| `bot.<id>.dreaming.<sub>` | `bot.mybot.dreaming.enabled` | 梦境巩固配置（per-bot） |
| `bot.<id>.timezone` | `bot.mybot.timezone` | Bot 独立时区 |
| `bot.<id>.token_quota` | `bot.mybot.token_quota` | Bot 级月 Token 额度（可细化到 channel/chat） |
| `bot.<id>.engagement.adaptive.<sub>` | `bot.mybot.engagement.adaptive.enabled` | 自适应参与度（Bot/Channel/会话三级） |
| `channel.<name>.<prop>` | `channel.mk.token` | Channel 配置 |
| `tools.<id>.policy` | `tools.mybot.policy` | 工具权限策略（JSON） |

键名规范：仅允许小写字母、数字、`.`、`_`、`-`（见 `ValidateKey`）。
