# dao — 数据访问层

基于 GORM 的数据模型定义与自动迁移。本包只定义表结构（模型 + `TableName()`），
具体 CRUD 由各业务包直接通过 `*gorm.DB` 完成。

## 模型概览

- **Bot 定义**：`BotDefinition` — Bot 配置持久化（模型/温度/系统提示词/步数上限/内存限制等）
- **Channel 定义**：`ChannelDefinition` — 渠道配置持久化（`Config` 存类型特有 JSON）
- **聊天会话**：`ChatSession` — 每个 Bot 下的独立对话线程
- **聊天消息**：`ChatMessage` — 对话历史记录，支持游标分页与流式中间态
- **用户**：`User` — 用户账户（角色 admin/member，状态 active/disabled）
- **设置**：`Setting` — 键值配置存储（含分类与描述元数据）
- **用量统计**：`UsageDaily` — 按日聚合的 LLM 用量（维度：bot/model/feature/channel/date）
- **窗口状态**：`WindowStateModel` — 上下文窗口状态快照（按 scope upsert）
- **记忆条目**：`EntryModel` — 统一记忆条目表
- **分层记忆**：`TieredMemoryModel` — L0~L3 分层记忆持久化
- **工作流**：`WorkflowModel` — 工作流 JSON 全量序列化存储
- **授权码**：`BindCode` — 一次性跨平台绑定授权码（5 分钟过期）
- **身份映射**：`IdentityMapping` — 平台用户 ID 到内部用户的映射
- **用户消息事件**：`UserMessageEvent` — 入站用户消息事件流（dreaming 回灌数据源，id 兼作水位线）
- **Bot 工具权限**：`BotToolPermission` — bot 维度 tool/platform/user_ids 三维权限规则（首条匹配生效）
- **浏览器 Cookie**：`BotBrowserCookie` — bot 浏览器 Cookie 持久化（(bot_id, domain, name, path) 唯一；`BrowserCookieView` 提供掩码视图）
- **自动迁移**：`Migrate(*gorm.DB) error` — 启动时自动建表，并幂等补齐存量表缺失列

## 表结构

| 表名 | 对应模型 | 说明 |
|------|----------|------|
| `bot_definitions` | `BotDefinition` | Bot 配置 |
| `channel_definitions` | `ChannelDefinition` | 渠道配置 |
| `chat_sessions` | `ChatSession` | 会话（对话线程） |
| `chat_messages` | `ChatMessage` | 对话历史 |
| `users` | `User` | 用户账户 |
| `config_settings` | `Setting` | 键值配置 |
| `stats_usage_daily` | `UsageDaily` | 用量统计 |
| `window_states` | `WindowStateModel` | 上下文窗口状态 |
| `memory_entries` | `EntryModel` | 记忆条目 |
| `tiered_memories` | `TieredMemoryModel` | 分层记忆 |
| `workflow_workflows` | `WorkflowModel` | 工作流 |
| `bind_codes` | `BindCode` | 授权码（一次性，5 分钟有效） |
| `identity_mappings` | `IdentityMapping` | 平台身份映射 |
| `user_message_events` | `UserMessageEvent` | 入站用户消息事件流 |
| `bot_tool_permissions` | `BotToolPermission` | Bot 工具权限规则 |
| `bot_browser_cookies` | `BotBrowserCookie` | Bot 浏览器 Cookie |

## 状态常量

```go
dao.BotStatusStopped / dao.BotStatusRunning          // Bot 运行状态
dao.SessionStatusActive / dao.SessionStatusArchived  // 会话状态
dao.ChatRoleUser / dao.ChatRoleAssistant             // 消息角色
```

## 迁移说明

`Migrate()` 先执行 GORM `AutoMigrate`，再调用内部 `ensureColumns`：
SQLite 上 `AutoMigrate` 不会为存量表 ALTER 加列，因此新增列（如
`bot_definitions.max_steps`、`hard_max_steps`、`memory_limit_mb`、
`chat_messages.session_id`、`bot_tool_permissions.auto`）需手动幂等补齐，避免写入时报 “no such column”。
