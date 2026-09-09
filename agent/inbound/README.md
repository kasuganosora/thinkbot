# inbound — 消息入口网关

统一消息入口层。各输入端（Webhook / WebSocket / Polling）自行管理启停，
只需调用 `Ingress.Receive()` 注入消息；Ingress 不关心消息来源，也不管理输入端生命周期。

## 功能

- **统一入口**：归一化（补全 ID / CreatedAt / TraceID）→ 封装 `core.Envelope` → 投递内部通道
- **背压控制**：内部带缓冲通道（`BufferSize` 默认 256），满时 `Receive` 阻塞、`TryReceive` 立即返回 false
- **链路追踪**：自动分配 TraceID（优先复用 `msg.TraceID` 或 context 中的值），并开启 `ingress.receive` span
- **自消息过滤**：通过 `SelfIDSet` 丢弃 Bot 自己发出的消息，防止自我回复死循环
- **消息去重**：按 `msg.ID` 做「检查并设置」去重（窗口 `ingressDedupTTL` = 2 分钟，后台每 30 秒清理过期项），防止 Misskey 多事件源（main + 多个 timeline 通道）或 WS 重连重放导致重复处理/重复回复；重复投递会打 Warn 日志（含 trace_id），空 ID 不去重、正常放行
- **fx 集成**：`inbound.Module` 提供 `IngressConfig` 与 `*Ingress`

Engine 的 worker goroutine 从 `Ingress.C()` 读取 Envelope 进行处理。

## 关键类型

| 类型 | 说明 |
|------|------|
| `Ingress` | 消息入口网关 |
| `IngressConfig` | 配置：`BufferSize`、可选外部 `SelfIDSet` |
| `SelfIDSet` | Bot 自身用户 ID 的线程安全集合（Add / Remove / Contains / Len） |
| `Channel` | 输入端适配器的**可选**元信息接口（`Name()` / `Type()`） |
| `MemoryChannel` | 内存输入端，用于测试与开发（`Send` / `TrySend`） |

## Ingress 方法

| 方法 | 说明 |
|------|------|
| `Receive(ctx, msg)` | 阻塞式注入；已关闭或 ctx 取消时返回错误 |
| `TryReceive(msg)` | 非阻塞注入；缓冲区满返回 false |
| `C()` | 返回 `<-chan *core.Envelope` 供 Engine 消费 |
| `Close()` | 关闭入口，缓冲区中已有消息仍可消费（幂等） |
| `Len()` | 当前缓冲区待处理消息数 |
| `RegisterSelfUserID(id)` / `UnregisterSelfUserID(id)` | 注册 / 移除 Bot 自身 ID |
| `IsSelfMessage(userID)` | 判断是否为 Bot 自身消息 |
| `SelfIDs()` | 返回内部 `*SelfIDSet` 引用，供 Engagement 层共享 |

## 自消息过滤

Channel 在 `Start()` 中发现自身身份后（Misskey 的 `getSelf`、Telegram 的 `getMe` 等）
调用 `RegisterSelfUserID` 注册。`SelfIDSet` 可通过 `IngressConfig.SelfIDSet` 由外部注入，
或通过 `Ingress.SelfIDs()` 取出交给 Engagement 的 `SelfExclusionRule`，
使两层防线引用同一份数据，无需时序协调。

## 消息去重

`Receive` / `TryReceive` 在自消息过滤之后、封装 Envelope 之前，对 `msg.ID` 做
「检查并设置」原子去重（`seen`）：窗口内（2 分钟）重复出现的相同 ID 直接丢弃并打
Warn 日志（`ingress: duplicate message dropped`，含 `entry` 标注入口）；`msg.ID` 为空
不去重、正常放行。后台 `seenCleanup` 每 30 秒清理过期记录，`Close()` 时随 `done`
channel 退出。测试见 `ingress_dedup_test.go`。

## 使用示例

```go
ingress := inbound.NewIngress(inbound.DefaultIngressConfig(), logger, tracerProvider)

// Channel 启动时注册自身 ID
ingress.RegisterSelfUserID("bot-user-id")

_ = ingress.Receive(ctx, core.Message{
    Source:  "telegram",
    Text:    "你好",
    UserID:  "u1",
    Channel: "chat-1",
})

// 测试场景
mem := inbound.NewMemoryChannel("test", ingress)
_ = mem.Send(ctx, core.Message{Text: "hello"})
```
