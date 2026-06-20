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
