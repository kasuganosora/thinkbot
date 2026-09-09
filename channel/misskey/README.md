# channel/misskey — Misskey 平台适配器

通过 WebSocket streaming 连接 Misskey 实例，监听 mention/reply/timeline 事件，归一化为统一的 `core.Message` 注入 Ingress。支持断线指数退避重连和消息去重。

## 核心类型

| 类型 | 说明 |
|------|------|
| `Config` | Misskey 渠道配置（见下） |
| `MisskeyChannel` | Misskey 平台适配器，实现 `bot.Channel` / `bot.Sender` 接口 |

### Config

```go
type Config struct {
    Host             string        // Misskey 实例 URL（如 "https://misskey.io"）
    Token            string        // 账号访问令牌
    WatchdogTimeout  time.Duration // WebSocket 看门狗超时，0 = 默认 120s
    PingInterval     time.Duration // 自动 Ping 间隔，0 = 默认 30s
    ReconnectDelay   time.Duration // 断线后重连间隔，0 = 默认 5s
    TimelineChannels []string      // 订阅的 timeline 频道列表
}
```

`TimelineChannels` 合法值（其余会被过滤）：`homeTimeline`、`localTimeline`、`hybridTimeline`、`globalTimeline`。
未配置时仅监听 mention/reply。

### 导出常量（帖子可见性）

| 常量 | 值 |
|------|-----|
| `VisibilityPublic` | `"public"` |
| `VisibilityHome` | `"home"` |
| `VisibilityFollowers` | `"followers"` |
| `VisibilitySpecified` | `"specified"` |

## 主要方法

```go
ch := misskey.NewChannel("misskey-main", "bot1", misskey.Config{
    Host: "https://misskey.example.com",
    Token: "your-token",
    TimelineChannels: []string{"homeTimeline", "localTimeline"},
})

ch.Start(ctx, ingress)                               // 启动 WS 监听
ch.Stop(ctx)                                         // 停止
ch.Reply(ctx, noteID, "回复内容")                     // 回复某条 note
ch.ReplyWithVisibility(ctx, noteID, "私密回复", misskey.VisibilityFollowers)
ch.React(ctx, noteID, "👍")                            // 添加反应
ch.Unreact(ctx, noteID)                              // 取消自己的反应
ch.Send(ctx, action)                                 // 按 core.Action 发送
ch.PostTimeline(ctx, "主动发帖", misskey.VisibilityHome, "") // 发布顶层新帖（心跳自主发声）
ch.CreatePollNote(ctx, "今天吃什么？", "", []string{"A", "B"}, false, 600, "q1") // user_choice 原生投票帖
ch.Name() / ch.Type() / ch.BotID()                   // 元信息，Type() 返回 "misskey"
ch.ChannelTools(ctx)                                 // 返回平台专属工具（见下）
```

- WebSocket 地址：`wss://{host}/streaming?i={token}`
- 单条帖子最大 3000 rune（`misskeyMaxNoteLength`），超出自动截断
- 重连采用 5s → 5min 指数退避；重连后按 mention 锚点 backfill 断连窗口（见下）
- 消息去重：基于 note ID 的 TTL 缓存（2min），每 30s 清理一次
- timeline 事件会加上 `[Timeline]` 前缀，并过滤 DM 与空帖
- main 流 `notification` 事件仅 `reaction` / `reaction:grouped` 入站为感知消息（见下），follow/renote 等其余通知忽略

## mention 锚点与断连 backfill（防重复回复）

Misskey streaming 断线期间不重放消息，mention 锚点（`lastMentionID`）就是重连后补齐窗口、且不重复回复的基准：

- **锚点含义**：最近一次成功处理的「指向本 Bot」帖子的 noteID。三条路径都会推进：main 流 mention/reply 实时处理、timeline 中 `timelineMentioned` 命中的帖子、backfill 每处理一条即推进（长 outage 中途再断线可断点续传）。启动时拉取最新一条提及作种子锚点。
- **单调递增守卫**：Misskey aid 定长且按时间字典序递增，锚点只允许向更新的 ID 推进；空 ID 与乱序到达的旧 ID 均不更新（timeline/main/backfill 并发时的防护）。
- **backfill**：以锚点为 `sinceId`（开区间）翻页调 `notes/mentions`，每页 100、封顶 300；复用 `handleNote` 走与实时完全相同的归一化/去重/注入路径，与实时流重叠由 `dedupSeen` 兜底。
- **事故背景（2026-09-01）**：timeline 路径不推进锚点 + 锚点被旧值卡住，两次断连 backfill 把整晚 mention 重放两轮——2min 的 dedup 窗口挡不住几十分钟后的重放，导致同一帖被重复生成回复甚至重复外发。上述守卫与联动即为此修复。

## 纯 Renote 回复抑制

Misskey 拒绝对**纯 Renote**（只有 renote 指针、无正文）发起文本回复（API 返回 400 `CANNOT_REPLY_TO_A_PURE_RENOTE`），故这类入站帖不生成回复：

- **触发条件**：`note.Renote != nil` 且正文去空白后为空（带正文的 quote 不算）。
- **行为**：`handleNote` 在 metadata 写 `core.MetaIsPureRenote=true`；`renoteFallback` 仍把被转帖的正文送入上下文，bot「看得到、能思考，只是不回复」。api 层 pure-renote enricher（order 48，先于 LLMStage(100) 产生 ActionReply）据此设置硬抑制标记 `KVSuppressReasonPureRenote`——属硬抑制，模型 `REPLY_CONTROL send:true` 也不能覆盖。

## 入站反应通知（awareness-only）

main 流 `type=notification` 中 `reaction` / `reaction:grouped` 被归一化为「仅感知」消息：校验被表态帖的作者确为 bot 自己、忽略自赞后，注入空 `Text` + `[Misskey 反应] …` InjectContext 的 1:1 私聊消息（空 Text 不污染 L0 记忆）；metadata 带 `ack_only:true` 且**故意不设** `reply_target`（避免误串接到被表态帖）。出站硬抑制由 api 层 reaction-ack enricher 负责：不回复、不回赞、不转发、不为此调工具，除非对方同时发了文字在找 bot。

## React 幂等（ALREADY_REACTED）

- **触发条件**：对 bot 已反应过的帖子再次调 `notes/reactions/create`（Misskey 400 `ALREADY_REACTED`）；撤销侧对称，未反应过时 400 `NOT_REACTED` → `ErrNotReacted`。
- **行为**：`createReaction` 将其包装为哨兵错误 `ErrAlreadyReacted`（幂等成功语义）；工具层 `misskey_react_to_note` 用 `errors.Is` 识别后返回 `success:true, already_reacted:true` 而非报错，避免编排层重复重试或误判失败。
- **关键实现点**：幂等判定必须写在 HTTP 客户端的 **err 分支**——本项目 HTTP 客户端把 4xx 直接作为 error 从 `Do()` 返回，响应体只存在于 `err.Error()` 里，只判 `resp.StatusCode==400` 的分支实际不可达（2026-08-30 生产验证：只写在 StatusCode 分支的旧修复完全失效）。`errHasMisskeyCode` 负责在错误文本中匹配 Misskey 错误码。
- **Renote 预检**：`misskey_react_to_note` 加反应前先 `getNote`，目标 `renoteId != ""` 时直接返回 `skipped:true, reason:"cannot_react_to_renote"` 的友好跳过，避免注定 400 `CANNOT_REACT_TO_RENOTE`。

## user_choice 原生投票

`Start` 时通过 `interaction.RegisterPollCreator("misskey", c.CreatePollNote)` 注册平台投票创建器。LLM 调 `user_choice` 工具提问时，本 channel 发布**原生 poll 帖**（至少 2 个选项），用户直接在 Misskey UI 点选：

- **投票事件协议**：pollVoted 只经 note capture 流投递，main 流不含投票事件。发帖后以顶层消息 `{"type":"subNote","body":{"id":"<noteId>"}}` 订阅该帖（**不是** channel 消息），事件以顶层 `{"type":"noteUpdated","body":{"id":..., "type":"pollVoted", "body":{"choice":N,"userId":...}}}` 返回；结束时发 `unsubNote` 退订。衍生版可能在 main 流投递 `pollVoted`（channel 消息体），两条路径都处理
- **回填语义**：single 模式首票立即 `interaction.ResolveFrom` 回填；multiple 模式每票累计、debounce ~3s 后一次性回填（Misskey 每个选项单独发 pollVoted，没有 done 信号）
- **过期**：poll 的 `expiresAt` 即问题超时；超时/未知帖子的投票事件忽略

## 平台专属工具

`ChannelTools(ctx)` 返回以下工具，供 Agent 在对话中直接调用 Misskey 操作：

| 工具名 | 说明 |
|--------|------|
| `misskey_follow_user` | 关注用户 |
| `misskey_unfollow_user` | 取消关注用户 |
| `misskey_create_note` | 发布帖子 |
| `misskey_create_renote` | 转发（Renote）帖子 |
| `misskey_delete_note` | 删除帖子 |
| `misskey_react_to_note` | 对帖子添加反应（幂等：已反应过返回成功；Renote 目标友好跳过，见上） |
| `misskey_unreact_to_note` | 取消对帖子的反应 |
| `misskey_get_user_notes` | 获取指定用户的最近帖子 |
| `misskey_search_notes` | 按关键词搜索实例内帖子 |
| `misskey_search_user` | 搜索用户 |
| `misskey_list_following` | 列出正在关注的用户 |

## 架构

```
Misskey WS Streaming → types.go (Note 解析) → channel.go (归一化 + 去重)
                                                        ↑
                         api.go (回帖/反应/取消反应/发送) ← Outbound Action
user_choice: channel.go (CreatePollNote + subNote/noteUpdated pollVoted 捕获 + debounce 回填) → interaction
```

- **api.go** — Misskey REST API 封装（createNote、getNote、react、unreact、deleteNote、follow/unfollow、searchUser、getUserNotes、searchNotes、createPoll 等）
- **channel.go** — WebSocket 连接管理、消息归一化、重连与去重逻辑；user_choice 原生 poll 的创建、note capture 订阅与 pollVoted 回填
- **types.go** — Misskey API 数据结构（`Note`、`File`、`User`）
- **tools.go** — 平台专属工具定义（`ChannelTools`）
