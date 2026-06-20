# dao — 数据访问层

基于 GORM 的数据访问对象，封装数据库表的 CRUD 操作。

## 功能

- **Bot 定义**：`BotDefinition` — Bot 配置持久化（模型/温度/系统提示词等）
- **Channel 定义**：`ChannelDefinition` — 渠道配置持久化
- **聊天消息**：`ChatMessage` — 对话历史记录
- **用户**：`User` — 用户账户
- **设置**：`Setting` — 键值配置存储
- **用量统计**：`UsageDaily` — 每日 Token 使用量
- **窗口状态**：`WindowState` — 对话窗口快照
- **工作流**：`Workflow` — 工作流定义与执行状态
- **自动迁移**：`Migrate()` 启动时自动建表/加列

## 表结构

| 表名 | 对应模型 | 说明 |
|------|----------|------|
| `bot_definitions` | `BotDefinition` | Bot 配置 |
| `channel_definitions` | `ChannelDefinition` | 渠道配置 |
| `chat_messages` | `ChatMessage` | 对话历史 |
| `users` | `User` | 用户账户 |
| `settings` | `Setting` | 键值配置 |
| `usage_daily` | `UsageDaily` | 用量统计 |
| `window_states` | `WindowState` | 窗口快照 |
| `workflows` | `Workflow` | 工作流 |
