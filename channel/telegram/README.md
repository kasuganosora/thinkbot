# channel/telegram — Telegram 平台适配器

通过 long polling 持续获取 Telegram Bot 的更新消息，归一化为统一的 `core.Message` 注入 Ingress。支持回复、编辑消息、发送"正在输入"状态、消息反应感知（awareness-only）、引用上下文捕获，以及工作空间文件/图片投递工具。

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
ch.SetFileSource(fn)                                          // 注入工作空间文件源（须在 Start 前调用，见文件投递工具）
ch.ChannelTools(ctx)                                          // 返回平台专属工具（见下）
```

## 特性

- **消息识别**：自动识别 @提及、`/`命令（offset=0）、回复 Bot、以及 `text_mention`（无 username 的用户提及）的消息
- **长消息拆分**：超过 4096 字符（`telegramMaxMessageLength`）的消息按换行/rune 自动拆分多条发送
- **Markdown 支持**：通过 `ParseMode` 指定 `MarkdownV2` 或 `HTML`
- **user_choice**：Start 时注册 `PollCreator`，发送 inline keyboard；`getUpdates` 默认含 `callback_query`，点击经 `ResolveFrom` 回填（不注入 Ingress）
- **引用回复可见**：入站消息携带 `reply_to_message_id` / `reply_to_text` / `reply_to_from`，上游 messageBuilder 据此渲染 `[引用 <作者> 的消息]` 块，模型能看到被引内容
- **消息反应（awareness-only）**：`message_reaction` 更新（需 bot 为群管理员）归一化为 `[Telegram 反应]` 注入，只处理新增反应（new − old），带 `ack_only: true`、故意不设 `reply_target`，不触发回复
- **私聊永不静音（telegram 侧表现）**：Telegram 私聊 `ChatType=private` 映射到 `core.ChatPrivate`，上游 reply-control 在 1:1 私聊反转为 fail-open——软门（节奏/engagement）不再抑制、模型 `send:false` 时仍尽量提取可发内容、缺控制块时回退清洗后纯文本（心跳/cron 源除外）；硬门（纯 Renote、被动未提及、反应通知、未回应熔断）仍 fail-closed

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
| `telegram_send_document` | 发送工作空间文件（multipart 上传 `sendDocument`，任意类型 ≤45MB） |
| `telegram_send_photo` | 发送工作空间图片（multipart 上传 `sendPhoto`，照片渲染带预览，PNG/JPEG/GIF/WEBP ≤9MB） |

### 文件投递工具（send_document / send_photo）

两者共用一套骨架，把 bot 工作空间文件实际送达用户（此前工作空间能写、Channel 只能发文本）：

- **文件源注入**：`SetFileSource(FileSourceFunc)` 在 Start 前由 BotService 注入（`WorkspaceManagerForBot → ws.ReadFile`）；telegram 包不依赖 sandbox，docker/local 后端由 workspace 抽象统一，`validatePath` 防路径穿越。未注入或读取失败时工具拒绝执行。
- **参数**：`filePath`（工作空间相对路径）；`chatId` 缺省取当前会话（`MessageMeta.ChatID`），两者都无则拒绝执行，避免发错对象；`caption` 可选。
- **文件名**：`filepath.Base`（先归一化 `\` → `/`，兼容 Windows 风格路径与尾部斜杠），空/`.` 时兜底 `file` / `photo`。
- **multipart 转义**：`api.go` 统一走 `AddFileWithMIME`——文件名含双引号/换行时 Content-Disposition 被正确转义（`escapeMultipartQuotes`），防止头部被截断导致上传丢失。注意：标准库 `AddFile` 同样会转义（`CreateFormFile` 内部 `%q`，已由 `TestMultipartAddFileEscapesQuotes` 锁真值），用 WithMIME 仅为显式设置 Content-Type，不要据此"统一"调用点。
- **上限**：document 45MB（TG bot 50MB 限内留余量）；photo 9MB（TG sendPhoto 10MB 限内），超限报错并提示改用 `telegram_send_document`。

`send_photo` 额外加固：

- **caption 按 UTF-16 code units 截断至 1024**（`truncateToUTF16`）：Telegram 上限按 UTF-16 计，emoji 等代理对 1 rune 占 2 unit，按 rune 截断仍可能超限被 400；截断绝不切断代理对。`send_document` 的 caption 仍按 rune 截断。
- **图片守卫**：上传前按魔数校验（PNG/JPEG/GIF/WEBP），非图片直接拒绝，在被 TG 400 前暴露失败。
- **可观测性**：发送成功打 `Infow`（bot_id/chat_id/file_path/file_name/file_size/caption_utf16_len/message_id）。
- **exfil-deny 锁**：`telegram_send_document` / `telegram_send_photo` 在 toolperm 中注册为 `RiskExfil`——默认拒绝（即便平台无任何权限规则也**不**自动开放），必须管理员显式 allow；同时复用 broadcast 的「系统/子代理会话禁止对外发言」硬约束。风险级别已由 `risk_test` / `service_test` 锁定，防未来重构悄悄降级。

## 架构

```
Telegram getUpdates (long polling) → types.go (Update/Message/CallbackQuery/MessageReaction 解析)
    → channel.go (归一化 + 提及识别) → Ingress
    → choice.go (inline keyboard / callback_query → interaction)
    ← api.go (sendMessage/editMessage/answerCallbackQuery/sendDocument/sendPhotoUpload)
```

- **api.go** — Telegram Bot API HTTP 封装（含 `APIBaseURL` 自定义、限流 throttle、multipart 上传）
- **channel.go** — Long polling 循环、消息归一化、提及检测、拆分发送、反应事件入站、`FileSourceFunc` 注入点
- **types.go** — Telegram API 数据结构（`Update`、`Message`、`CallbackQuery`、`ChatMemberUpdated`、`MessageReactionUpdated`）
- **choice.go** — user_choice inline keyboard 与 callback_query 回填
- **tools.go** — 平台专属工具定义（`ChannelTools`）、caption UTF-16 截断与图片魔数守卫
