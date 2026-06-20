# channel — 多平台消息渠道适配器

将不同即时通讯平台的消息接入统一为 `core.Message` 格式，注入 Bot 的 Ingress 管道。

## 子目录

### misskey/

通过 WebSocket streaming 连接 Misskey 实例，监听 mention/reply/timeline 事件。

- WebSocket 长连接（自动重连 + 看门狗）
- 事件归一化（note → `core.Message`）
- HTTP API 客户端（发帖/用户信息/文件上传）
- 消息去重（基于 note ID 的 TTL 缓存）

### telegram/

通过 Bot API long polling 持续获取用户消息。

- Long Polling（可配置超时和更新类型过滤）
- 群聊/私聊识别（@提及 + 回复检测）
- HTML / MarkdownV2 / 纯文本发送
- 长消息自动分片（4096 字符限制）

## Message 字段设计规范

所有 channel 实现在构建 `core.Message` 时，**必须正确区分 `Channel` 和 `UserID`**。
这直接影响 memory 系统的记忆隔离和用户画像准确性。

### 字段定义

| 字段 | 含义 |
|------|------|
| `Channel` | 会话空间标识（群组 ID / 私聊会话 ID / 共享社交空间）。memory 的 `ChannelScope` 以此为 key |
| `UserID` | **实际发言者**的 ID。memory 的 `UserScope` 以此为 key，Profiler 据此为每个人独立构建画像 |

### 规则

1. **`UserID` 必须始终是发言者个人 ID**，不是群组/频道 ID
2. **群聊场景**：`Channel` = 群组 ID，`UserID` = 发言者 ID（两者不同）
3. **私聊场景**：`Channel` = 会话 ID（可能 = 发言者 ID），`UserID` = 发言者 ID
4. **社交时间线**：`Channel` = 共享空间标识（如 `misskey:timeline`），`UserID` = 发言者 ID

### 反模式

```
✗ 把群组 chatID 当作 UserID → 所有人画像混在一起，无法区分
✗ timeline 把 user ID 当作 Channel → 每个人独立空间，丢失社交上下文
✗ UserID 为空 → memory 无法为该用户构建画像
```

### 各 Channel 实现

| Channel | 场景 | Channel 值 | UserID 值 |
|---------|------|-----------|-----------|
| Telegram | 群组 | `chatID` | 发言者 user ID |
| Telegram | 私聊 | `chatID`（= user ID） | 发言者 user ID |
| Misskey | timeline | `misskey:timeline` | `note.User.ID` |
| Misskey | mention / reply | `note.User.ID` | `note.User.ID` |

> **新增 Channel 时请参照此规范。** 详见 `agent/memory/README.md` 的 "Scope 设计" 章节。

## 使用示例

```go
// Misskey
mkCh := misskey.NewChannel("mk-bot", "bot-id", misskey.Config{
    Host:  "https://misskey.io",
    Token: "xxx",
})

// Telegram
tgCh := telegram.NewChannel("tg-bot", "bot-id", telegram.Config{
    Token: "123456:ABC-DEF...",
})
```
