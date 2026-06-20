# channel/misskey — Misskey 平台适配器

通过 WebSocket streaming 连接 Misskey 实例，监听 mention/reply 事件，归一化为统一的 `core.Message` 注入 Ingress。支持断线指数退避重连和消息去重。

## 核心类型

| 类型 | 说明 |
|------|------|
| `Config` | Misskey 渠道配置（`Host`、`Token`、`WatchdogTimeout`、`SubscribeTimeline` 等） |
| `MisskeyChannel` | Misskey 平台适配器，实现 `channel.Channel` 接口 |

## 导出常量

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
})

ch.Start(ctx, ingress)   // 启动监听
ch.Reply(ctx, noteID, "回复内容")
ch.React(ctx, noteID, "👍")
ch.ReplyWithVisibility(ctx, noteID, "私密回复", misskey.VisibilityFollowers)
```

## 架构

```
Misskey WS Streaming → types.go (Note 解析) → channel.go (归一化) → Ingress
                                                        ↑
                         api.go (回帖/反应/发送) ← Outbound Action
```

- **api.go** — Misskey REST API 封装（createNote、react、删除反应）
- **channel.go** — WebSocket 连接管理、消息归一化、重连逻辑
- **types.go** — Misskey API 数据结构（`Note`、`File`、`User`）
