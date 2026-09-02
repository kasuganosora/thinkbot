# channel/telegram — Telegram 平台适配器

通过 long polling 持续获取 Telegram Bot 的更新消息，归一化为统一的 `core.Message` 注入 Ingress。支持回复、编辑消息、发送"正在输入"状态。

## 核心类型

| 类型 | 说明 |
|------|------|
| `Config` | Telegram 渠道配置（见下） |
| `TelegramChannel` | Telegram 平台适配器，实现 `bot.Channel` / `bot.Sender` 接口 |

### Config

```go
type Config struct {
    Token          string   // Bot 令牌（如 "123456:ABC-DEF..."）
    PollTimeout    int      // long polling 超时秒数，0 = 默认 30
    AllowedUpdates []string // 仅接收的更新类型，空 = 接收所有
    APIBaseURL     string   // API 基础地址，空 = 默认 https://api.telegram.org
    ParseMode      string   // 发送格式："HTML" / "MarkdownV2" / ""（纯文本）
}
```

`APIBaseURL` 用于反向代理或无法直连 `api.telegram.org` 的场景。

## 主要方法

```go
ch := telegram.NewChannel("tg-main", "bot1", telegram.Config{
    Token: "your-bot-token",
})

ch.Start(ctx, ingress)                                        // 启动 long polling
ch.Stop(ctx)                                                  // 停止
ch.Reply(ctx, chatID, "回复内容", replyToMessageID)            // 回复某条消息
ch.ReplyWithMode(ctx, chatID, "**粗体**", "MarkdownV2", replyToMessageID)
ch.EditMessage(ctx, chatID, messageID, "编辑后的内容")         // 编辑已发送消息
ch.SendTyping(ctx, chatID)                                     // 发送"正在输入"动作
ch.Send(ctx, action)                                          // 按 core.Action 发送
ch.RecentChats()                                          // 近期活跃会话列表（最多 20 个，实现 core.RecentChatLister）
ch.Name() / ch.Type() / ch.BotID()                            // 元信息，Type() 返回 "telegram"
ch.ChannelTools(ctx)                                          // 返回平台专属工具（见下）
```

## 特性

- **消息识别**：自动识别 @提及、`/`命令（offset=0）、回复 Bot、以及 `text_mention`（无 username 的用户提及）的消息
- **长消息拆分**：超过 4096 字符（`telegramMaxMessageLength`）的消息按换行/rune 自动拆分多条发送
- **Markdown 支持**：通过 `ParseMode` 指定 `MarkdownV2` 或 `HTML`
- **user_choice**：Start 时注册 `PollCreator`，发送 inline keyboard；`getUpdates` 默认含 `callback_query`，点击经 `ResolveFrom` 回填（不注入 Ingress）

## 平台专属工具

`ChannelTools(ctx)` 返回以下工具，供 Agent 在对话中直接调用 Telegram 管理操作：

| 工具名 | 说明 |
|--------|------|
| `telegram_ban_member` | 封禁群成员 |
| `telegram_unban_member` | 解封群成员 |
| `telegram_delete_message` | 删除消息 |
| `telegram_get_chat_info` | 获取群组/频道信息 |
| `telegram_get_chat_member_count` | 获取群成员数量 |
| `telegram_get_chat_administrators` | 获取群管理员列表 |
| `telegram_pin_message` | 置顶消息 |

## 架构

```
Telegram getUpdates (long polling) → types.go (Update/Message/CallbackQuery 解析)
    → channel.go (归一化 + 提及识别) → Ingress
    → choice.go (inline keyboard / callback_query → interaction)
    ← api.go (sendMessage/editMessage/answerCallbackQuery)
```

- **api.go** — Telegram Bot API HTTP 封装（含 `APIBaseURL` 自定义）
- **channel.go** — Long polling 循环、消息归一化、提及检测、拆分发送
- **types.go** — Telegram API 数据结构（`Update`、`Message`、`CallbackQuery`、`ChatMemberUpdated`）
- **choice.go** — user_choice inline keyboard 与 callback_query 回填
- **tools.go** — 平台专属工具定义（`ChannelTools`）
