# channel/mastodon — Mastodon（长毛象）平台适配器

通过 WebSocket streaming 监听通知（@提及 / 回复 / 收藏）与可选的公共时间线，归一化为统一的 `core.Message` 注入 Ingress；出站走 REST API。支持断线指数退避重连、消息去重与断连窗口 backfill。

设计与 `channel/misskey` 对等，但 Mastodon 与 Misskey 在协议上有若干**不可抹平的差异**，全部集中记录在本文件「与 Misskey 的差异」一节。

## 核心类型

| 类型 | 说明 |
|------|------|
| `Config` | Mastodon 渠道配置（见下） |
| `MastodonChannel` | 平台适配器，实现 `bot.Channel` / `bot.Sender` 接口 |

### Config

```go
type Config struct {
    Server               string        // 实例 URL（如 "https://mastodon.social"）
    Token                string        // OAuth access token
    WatchdogTimeout      time.Duration // WebSocket 看门狗超时，0 = 默认 120s
    PingInterval         time.Duration // 自动 Ping 间隔，0 = 默认 30s
    ReconnectDelay       time.Duration // 断线后重连间隔，0 = 默认 5s
    Streams              []string      // 订阅的公共时间线流
    ResolveReplyContext  bool          // 入站回复是否拉取父帖构造上下文（默认 true）
}
```

`Streams` 合法值（其余会被过滤）：`user`（关注时间线）、`public`（联邦）、`public:local`（本站）、`public:remote`（远端）、`direct`（私信）。
留空则仅接收通知流里的 @提及 / 回复 / 收藏。

> **通知流 `user:notification` 强制订阅、不接受配置** —— 它是 @提及与回复的**唯一**入口，
> 关掉等同于让 bot 失聪。

### 导出常量（可见性）

| 常量 | 值 | 对应 Misskey |
|------|-----|------|
| `VisibilityPublic` | `"public"` | `public` |
| `VisibilityUnlisted` | `"unlisted"` | `home` |
| `VisibilityPrivate` | `"private"` | `followers` |
| `VisibilityDirect` | `"direct"` | `specified` |

`mapVisibility` 同时接受两侧取值，上游沿用 Misskey 语义（`"home"`）也能正确落位。

## 主要方法

```go
ch := mastodon.NewChannel("mastodon-main", "bot1", mastodon.Config{
    Server: "https://mastodon.example.com",
    Token:  "your-access-token",
    Streams: []string{"user", "public:local"},
})

ch.Start(ctx, ingress)                                       // 启动 WS 监听
ch.Stop(ctx)                                                 // 停止
ch.Reply(ctx, statusID, "回复内容")                            // 回复某条帖子
ch.ReplyWithVisibility(ctx, statusID, "私密回复", mastodon.VisibilityDirect)
ch.Favourite(ctx, statusID)                                  // 收藏（点赞）
ch.Unfavourite(ctx, statusID)                                // 取消收藏
ch.Boost(ctx, statusID) / ch.Unboost(ctx, statusID)           // 转发 / 撤销转发
ch.Send(ctx, action)                                         // 按 core.Action 发送
ch.PostTimeline(ctx, "主动发帖", mastodon.VisibilityUnlisted, "") // 发布顶层新帖（心跳自主发声）
ch.CreatePollStatus(ctx, "今天吃什么？", "", []string{"A","B"}, false, 600, "q1") // user_choice 原生投票帖
ch.Name() / ch.Type() / ch.BotID()                           // 元信息，Type() 返回 "mastodon"
ch.ChannelTools(ctx)                                         // 返回平台专属工具（见下）
```

- WebSocket 地址：`wss://{host}/api/v1/streaming?access_token={token}&stream=user:notification&stream=...`
- 单条帖子上限取自实例配置（`GET /api/v1/instance` 的 `configuration.statuses.max_characters`），
  探测失败时回退 500；超出自动截断
- 重连采用 5s → 5min 指数退避；重连后按通知锚点 backfill 断连窗口（见下）
- 消息去重：基于 status ID 的 TTL 缓存（2min），每 30s 清理一次
- timeline 事件会加上 `[Timeline]` 前缀，并过滤空帖与自身帖子
- 通知流仅 `mention`（正常入站）与 `favourite`（awareness-only）处理；follow / reblog / poll 等忽略

## 与 Misskey 的差异（重要）

这些是平台协议层面的差异，不是实现取舍，改动前请先看这里。

| 维度 | Misskey | Mastodon | 处理 |
|------|---------|----------|------|
| 认证 | 请求体/query 的 `i=<token>` | `Authorization: Bearer <token>` | api.go 统一 `authRequest` |
| 事件格式 | `{type, body}` | `{event, payload}`，payload 是**JSON 字符串** | `decodePayload` 兼容「字符串套 JSON」与「裸对象」 |
| 正文 | 纯文本 | **HTML**（`<p>…</p>`） | `htmlToText` 转纯文本（先补换行→删标签→反转义） |
| 提及标识 | `username` + `host` 两字段 | 合并好的 `acct`（`alice@example.social`） | 出站提及直接用 acct，无需查host |
| 表态 | 自定义 emoji 反应 | 只有「收藏」 | `mastodon_favourite_status`，无 emoji 参数 |
| 重复表态 | 400 `ALREADY_REACTED` | 200，天然幂等 | 不需要哨兵错误 |
| 转发 | Renote（可带正文 = quote） | Boost（body 为空），quote 是 4.x 新特性 | `IsPureReblog` 仅作信息记录 |
| 纯转发回复 | 400 `CANNOT_REPLY_TO_A_PURE_RENOTE`，需硬抑制 | **允许**回复 boost | 不写 `core.MetaIsPureRenote` |
| 回复上下文 | streaming 内联 `reply` 对象 | **不内联**，只有 `in_reply_to_id` | 额外 `getStatus` + 缓存（`ResolveReplyContext` 可关） |
| mention 入口 | main 流 `mention` / `reply` 事件 | 通知流 `notification`（`type=mention`） | 强制订阅 `user:notification` |
| backfill 端点 | `notes/mentions` | `notifications?types[]=mention` | 锚点是**通知 ID**，非 status ID |
| 投票事件 | `pollVoted` 直推，含 choice + voter | **无事件**，结果只有票数、无投票人 | 轮询 `GET /api/v1/polls/:id` 按票数增量推断 |
| 投票选项数 | 2~10 | **2~4**（服务端硬限） | 超出截断到 4 |
| 投票最短时长 | 无限制 | **≥300 秒**，否则 422 | 自动 clamp |
| 帖子长度 | 固定 3000 rune | 实例级配置（默认 500） | 启动时探测 |

## mention 锚点与断连 backfill（防重复回复）

Mastodon streaming 同样不重放断连期间的消息，锚点机制与 Misskey 一致，但**锚点存的是通知 ID**：

- **锚点含义**：最近一次成功处理的 mention 通知 ID。实时 mention 处理与 backfill 每处理一条都会推进；启动时拉最新一条 mention 通知作种子。
- **单调递增守卫**：Mastodon ID 与 Misskey aid 一样是定长雪花 ID、按时间字典序递增，锚点只允许向更新的 ID 推进（`newerID`）。空 ID 与乱序旧 ID 均不更新。
- **backfill**：以锚点为 `since_id`（开区间）、用 `max_id` 翻页调 notifications，每页 100、封顶 300；复用 `handleStatus` 走与实时相同的路径，重叠由 `dedupSeen` 兜底。

> 事故背景：Misskey 曾因「锚点不推进 + 被旧值卡住」，两次断连把整晚 mention 重放两轮，重复外发。
> 这里的守卫与联动是照搬那次修复——2 分钟的 dedup 窗口挡不住几十分钟后的重放。

## 入站收藏通知（awareness-only）

通知流 `type=favourite` 归一化为「仅感知」消息：忽略自赞后，注入空 `Text` + `[Mastodon 收藏] …` InjectContext 的 1:1 私聊消息
（空 Text 不污染 L0 记忆）；metadata 带 `ack_only:true` 且**故意不设** `reply_target`（避免误串接到被收藏的帖子）。
出站硬抑制由 api 层 reaction-ack enricher 负责：不回复、不回赞、不转发、不为此调工具，除非对方同时发了文字在找 bot。

## user_choice 原生投票（轮询回填）

`Start` 时通过 `interaction.RegisterPollCreator("mastodon", c.CreatePollStatus)` 注册平台投票创建器。

- **发帖**：`POST /api/v1/statuses` 带 `poll{options, expires_in, multiple}`，拿到 status 与 poll ID。
- **回填语义**：单选首票立即回填；多选累计、debounce ~3s 后一次性回填（与 Misskey 一致）。
- **轮询**：Mastodon 不推送投票事件，故每条投票帖起一个 goroutine，每 3s 调 `GET /api/v1/polls/:id`，
  把「各选项票数相对基线的增量」当作新票。轮询在结算 / 超时 / channel 停止时终止。
- **投票人不可知**：Mastodon 的 poll 结果只暴露 `votes_count` 与自己的 `own_votes`，**无法得知是谁投的**。
  因此回填直接用问题登记的 `ChatID`，跳过 Misskey 那套「投票人必须匹配会话」的校验。这是平台限制，不是实现偷懒。

## 平台专属工具

`ChannelTools(ctx)` 返回以下工具，供 Agent 在对话中直接调用 Mastodon 操作：

| 工具名 | 说明 | 对应 Misskey |
|--------|------|--------------|
| `mastodon_follow_account` | 关注账号（locked 账号进入待批准） | `misskey_follow_user` |
| `mastodon_unfollow_account` | 取消关注账号 | `misskey_unfollow_user` |
| `mastodon_post_status` | 发布帖子（**仅用于非回复的新帖**） | `misskey_create_note` |
| `mastodon_boost_status` | 转发（Boost）帖子 | `misskey_create_renote` |
| `mastodon_delete_status` | 删除自己的帖子 | `misskey_delete_note` |
| `mastodon_favourite_status` | 收藏帖子（幂等） | `misskey_react_to_note` |
| `mastodon_unfavourite_status` | 取消收藏 | `misskey_unreact_to_note` |
| `mastodon_get_account_statuses` | 获取指定账号的最近帖子 | `misskey_get_user_notes` |
| `mastodon_search_statuses` | 按关键词搜索帖子（多数实例无全文索引，空结果是常态） | `misskey_search_notes` |
| `mastodon_search_accounts` | 搜索账号 | `misskey_search_user` |
| `mastodon_list_following` | 列出正在关注的账号 | `misskey_list_following` |

## API 配额

Mastodon 默认限流 **300 请求 / 5 分钟**。入站回复上下文解析（`ResolveReplyContext`）会给每条回复加一次
`GET /api/v1/statuses/:id`，公共时间线订阅的流量下很容易触顶。已用带 TTL 的父帖缓存（10 分钟）去重；
仍可在配置里关掉该能力换取配额。

## 架构

```
Mastodon WS Streaming → types.go (streamEvent/Status 解析)
                                     ↓
                        channel.go (归一化 + 去重 + HTML 转文本)
                                     ↑
              api.go (发帖/收藏/转发/关注/搜索/投票) ← Outbound Action
user_choice: channel.go (CreatePollStatus + 轮询票数回填) → interaction
```

- **api.go** — REST 客户端（statuses、favourite/unfavourite、reblog/unreblog、follow/unfollow、
  accounts 搜索、statuses 搜索、notifications、polls、instance 配置）
- **channel.go** — WebSocket 连接管理、消息归一化、重连与去重、锚点 backfill、Send/Reply/PostTimeline、
  收藏与转发、原生投票的创建与轮询回填
- **types.go** — API 数据结构（`Status`、`Account`、`MediaAttachment`、`Notification`、`Poll`）
- **html.go** — Mastodon HTML 正文 → 纯文本
- **tools.go** — 平台专属工具定义（`ChannelTools`）
