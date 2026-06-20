# inbound — 消息入口网关

统一消息入口层，各 Channel（Webhook/WebSocket/Polling）自行管理启停，只需调用 `Ingress.Receive()` 注入消息。

## 功能

- `Ingress` 统一消息入口：归一化（填充 ID/时间）→ 封装 Envelope → 提交 Engine
- `Source` 接口标识消息来源（渠道名 + 是否被 @）
- 线程安全的消息提交（带背压保护）
- Pipeline 集成：`IngressStage`（Order=10）

## 关键类型

| 类型 | 说明 |
|------|------|
| `Ingress` | 消息入口管理器 |
| `Source` | 消息来源描述（Channel + Mentioned） |

## 使用示例

```go
ingress := inbound.NewIngress(engine, logger)
ingress.Receive(ctx, core.Message{
    ID:      "msg-1",
    Source:   "telegram",
    Text:    "你好",
    UserID:  "u1",
    Channel: "chat-1",
})
```
