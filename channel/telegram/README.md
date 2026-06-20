# channel/telegram — Telegram 平台适配器

通过 long polling 持续获取 Telegram Bot 的更新消息，归一化为统一的 `core.Message` 注入 Ingress。支持回复、编辑消息、发送"正在输入"状态。

## 核心类型

| 类型 | 说明 |
|------|------|
| `Config` | Telegram 渠道配置（`Token`、`PollTimeout`、`AllowedUpdates`、`ParseMode`） |
| `TelegramChannel` | Telegram 平台适配器，实现 `channel.Channel` 接口 |

## 主要方法

```go
ch := telegram.NewChannel("tg-main", "bot1", telegram.Config{
    Token: "your-bot-token",
})

ch.Start(ctx, ingress)   // 启动 long polling
ch.Reply(ctx, chatID, "回复内容", replyToMessageID)
ch.ReplyWithMode(ctx, chatID, "**粗体**", "MarkdownV2", replyToMessageID)
ch.EditMessage(ctx, chatID, messageID, "编辑后的内容")
ch.SendTyping(ctx, chatID)  // 发送"正在输入"
```

## 特性

- **消息识别**：自动识别 @提及、`/`命令和回复 Bot 的消息
- **长消息拆分**：超过 4096 字符的消息自动拆分多条发送
- **Markdown 支持**：通过 `ParseMode` 指定 `MarkdownV2` 或 `HTML`

## 架构

```
Telegram getUpdates (long polling) → types.go (Update/Message 解析)
    → channel.go (归一化 + 提及识别) → Ingress
    ← api.go (sendMessage/editMessage/sendChatAction)
```

- **api.go** — Telegram Bot API HTTP 封装
- **channel.go** — Long polling 循环、消息归一化、提及检测
- **types.go** — Telegram API 数据结构（`Update`、`Message`、`ChatMemberUpdated`）
