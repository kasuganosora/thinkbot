# channel/misskey — Misskey 平台适配器

通过 WebSocket streaming 连接 Misskey 实例，监听 mention/reply/timeline 事件，归一化为统一的 `core.Message` 注入 Ingress。支持断线指数退避重连和消息去重。

## 核心类型

| 类型 | 说明 |
|------|------|
| `Config` | Misskey 渠道配置（见下） |
| `MisskeyChannel` | Misskey 平台适配器，实现 `channel.Channel` 接口 |

### Config

```go
type Config struct {
    Host             string        // Misskey 实例主机名（如 "misskey.io"）
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
    Host: "misskey.example.com",
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
ch.Name() / ch.Type() / ch.BotID()                   // 元信息，Type() 返回 "misskey"
ch.ChannelTools(ctx)                                 // 返回平台专属工具（见下）
```

- WebSocket 地址：`wss://{host}/streaming?i={token}`
- 单条帖子最大 3000 rune（`misskeyMaxNoteLength`），超出自动截断
- 重连采用 5s → 5min 指数退避
- 消息去重：基于 note ID 的 TTL 缓存（2min），每 30s 清理一次
- timeline 事件会加上 `[Timeline]` 前缀，并过滤 DM 与空帖

## 平台专属工具

`ChannelTools(ctx)` 返回以下工具，供 Agent 在对话中直接调用 Misskey 操作：

| 工具名 | 说明 |
|--------|------|
| `misskey_follow_user` | 关注用户 |
| `misskey_unfollow_user` | 取消关注用户 |
| `misskey_create_note` | 发布帖子 |
| `misskey_create_renote` | 转发（Renote）帖子 |
| `misskey_delete_note` | 删除帖子 |
| `misskey_react_to_note` | 对帖子添加反应 |
| `misskey_unreact_to_note` | 取消对帖子的反应 |
| `misskey_search_user` | 搜索用户 |
| `misskey_list_following` | 列出正在关注的用户 |

## 架构

```
Misskey WS Streaming → types.go (Note 解析) → channel.go (归一化 + 去重)
                                                        ↑
                         api.go (回帖/反应/取消反应/发送) ← Outbound Action
```

- **api.go** — Misskey REST API 封装（createNote、react、unreact、deleteNote 等）
- **channel.go** — WebSocket 连接管理、消息归一化、重连与去重逻辑
- **types.go** — Misskey API 数据结构（`Note`、`File`、`User`）
- **tools.go** — 平台专属工具定义（`ChannelTools`）
