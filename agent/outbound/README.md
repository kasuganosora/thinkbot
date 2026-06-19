# outbound — Pipeline 输出子系统

Pipeline 处理完消息后产出一组 `core.Action`，`outbound` 子系统负责将这些 Action 派发到正确的目的地——Channel 回复、备注写入记忆、回调通知等。同时内置 **EventBus**（事件总线），作为旁路输出供 Web SSE 实时展示处理进度。

## 架构概览

```
Pipeline 产出 []core.Action
          │
          ▼
  ┌───────────────┐
  │  Dispatcher   │  按 ActionType 路由
  │  (多路派发器)   │
  └───┬───────┬───┘
      │       │
      ▼       ▼
  ChannelReply  NoteHandler      SilentHandler    CallbackHandler
  (回写消息)     (写入记忆)        (静默记录)        (回调通知)
  
  ──────── 旁路 ────────
  EventBus ──► Subscribe / SubscribeWithReplay ──► SSE / 日志 / 监控
```

## 文件结构

| 文件 | 职责 |
|------|------|
| `dispatcher.go` | `Dispatcher` 接口、`LogDispatcher`（开发用）、`MultiDispatcher`（按 ActionType 路由） |
| `channel_handler.go` | `ChannelReplyHandler` — 将 Action 路由到对应 Channel 的 Sender |
| `note_handler.go` | `NoteHandler` — 将 ActionNote 写入统一记忆仓储 |
| `callback_handler.go` | `CallbackRegistry` + `CallbackHandler` — 回调注册与调用 |
| `silent_handler.go` | `SilentHandler` — ActionSilent 处理（仅 trace，无外部 I/O） |
| `eventbus.go` | `EventBus` 事件总线 + `EventEmitter` 发射器 + `EventStore` 事件回放 |
| `event_emitter.go` | `EventEmitter` — 类型化的事件发射便捷封装 |
| `module.go` | fx 依赖注入模块定义 |

---

## Dispatcher — Action 派发

### 接口

```go
type Dispatcher interface {
    Dispatch(ctx context.Context, actions []core.Action) error
}
```

### MultiDispatcher

按 `ActionType` 路由到不同的 `ActionHandler`，支持运行时动态注册。

```go
dispatcher := outbound.NewMultiDispatcher(logger, tp)

// 注册处理器
dispatcher.Register(core.ActionReply, channelReplyHandler)
dispatcher.Register(core.ActionNote, noteHandler)
dispatcher.Register(core.ActionSilent, silentHandler)
dispatcher.Register(core.ActionCallback, callbackHandler)

// 可选：设置兜底处理器
dispatcher.SetFallback(fallbackHandler)

// 启动校验（确保关键 ActionType 都有 handler）
missing := dispatcher.Validate(core.ActionReply, core.ActionNote)
if len(missing) > 0 {
    log.Fatal("missing handlers:", missing)
}

// 派发
err := dispatcher.Dispatch(ctx, actions)
```

### LogDispatcher

开发/测试用的派发器，将所有 Action 记录到日志不做实际投递。通过 fx Module 默认提供。

---

## ActionHandler 处理器

所有处理器实现统一接口：

```go
type ActionHandler interface {
    Handle(ctx context.Context, action core.Action) error
}
```

### ChannelReplyHandler

将 `ActionReply` / `ActionForward` 等回写型 Action 路由到对应 Channel 的 `Sender`。

路由键：`Action.Metadata["source_channel"]` → 在 Sender 注册表中查找 → 调用 `Send(ctx, action)`。

```go
handler := outbound.NewChannelReplyHandler(logger, tp)

// Channel 实现 ChannelSender 接口（同时满足 bot.Sender）
handler.Register("tg-bot", telegramChannel)

dispatcher.Register(core.ActionReply, handler)
```

### NoteHandler

将 `ActionNote` 转换为 `NoteEntry` 写入统一记忆仓储。Bot 的自主备注因此可被后续 LLM 检索回忆。

Action 字段约定：

| 字段 | 说明 |
|------|------|
| `Payload` | 备注文本（string） |
| `Metadata["category"]` | 分类（默认 `"observation"`） |
| `Metadata["importance"]` | 重要程度 float64（默认 0.5） |
| `Metadata["bot_id"]` | 所属 Bot ID |
| `Metadata["message_id"]` | 触发此备注的原始消息 ID |

Scope 确定逻辑：`Action.Channel` 非空 → `channel` scope；否则有 `bot_id` → `bot` scope；否则 → `global` scope。

### CallbackHandler

处理 `ActionCallback`，通过 `CallbackRegistry` 查找并调用回调函数。支持 TTL 自动过期清理。

典型场景：父 Agent 创建子任务时注册回调，子 Agent 完成后产出 `ActionCallback` 回传结果。

```go
registry := outbound.NewMemoryCallbackRegistry(outbound.WithCallbackTTL(10 * time.Minute))
defer registry.Close()

// 注册回调
cbID := registry.Register("task-123", func(ctx context.Context, result outbound.CallbackResult) error {
    log.Info("子任务完成:", result.Payload)
    return nil
})

// 将 cbID 传给子 Agent，子 Agent 完成后产出：
//   Action{Type: ActionCallback, Metadata: {"callback_id": cbID, "status": "success"}, Payload: resultData}

handler := outbound.NewCallbackHandler(registry, logger, tp)
dispatcher.Register(core.ActionCallback, handler)
```

回调为**一次性语义**：`Invoke` 后自动移除，防止并发重复调用。

### SilentHandler

处理 `ActionSilent`，不做任何外部 I/O，仅记录 trace 和日志。用于显式确认"有意的静默决策"（如群聊闲聊、重复消息），便于后续分析 Bot 决策分布。

---

## EventBus — 旁路事件总线

Pipeline / Workflow 在处理过程中通过 EventBus 发布进度事件，Web 端可通过 SSE 订阅实时查看。

### 核心接口

```go
type EventBus interface {
    Publish(ctx context.Context, event Event)
    Subscribe(traceID string) *Subscription
    SubscribeBot(botID string) *Subscription
    SubscribeWithReplay(traceID string, sinceSeq uint64) *Subscription  // 断线重连
    LatestSeq() uint64
    Unsubscribe(sub *Subscription)
    Close()
}
```

### 事件类型

| 分类 | 事件 | 说明 |
|------|------|------|
| **消息生命周期** | `message.received` / `message.dropped` / `message.done` / `message.error` | 消息进出 Pipeline |
| **Stage** | `stage.enter` / `stage.exit` / `stage.skip` / `stage.error` | Pipeline 各阶段 |
| **LLM** | `llm.start` / `llm.text_delta` / `llm.reason_delta` / `llm.tool_call` / `llm.tool_result` / `llm.step_done` / `llm.done` / `llm.error` | LLM 流式输出 |
| **决策** | `decision` | ReplyDecider 输出 |
| **Dispatch** | `dispatch.start` / `dispatch.done` / `dispatch.error` | Action 派发 |
| **Workflow** | `workflow.submitted` / `.analyzed` / `.completed` / `.failed` / `.terminated` / `.recovered` | 工作流生命周期 |
| **Workflow 节点** | `workflow.node.started` / `.completed` / `.failed` / `.reviewing` / `.retrying` | DAG 节点状态 |

所有事件都携带全局单调递增的 `Seq` 序列号，用于断线重连。

### EventEmitter

类型化的发射封装，bus 为 nil 时所有调用静默返回（NoOp）。

```go
emitter := outbound.NewEventEmitter(bus, "bot-1")

// 在 Pipeline / Bot 中使用
emitter.EmitStageEnter(ctx, traceID, "filter")
emitter.EmitLLMTextDelta(ctx, traceID, "Hello, ")
emitter.EmitLLMDone(ctx, traceID, 150, "stop")

// 通过 context 传递
ctx = outbound.ContextWithEmitter(ctx, emitter)
// ... 任何地方 ...
outbound.EmitterFromContext(ctx).EmitLLMError(ctx, traceID, err)
```

### 断线重连（EventStore）

EventBus 内置 `EventStore`（环形缓冲 + TTL），解决用户关闭页面再打开后 SSE 历史事件丢失的问题。

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `StoreCapacity` | 10000 | 环形缓冲最大事件数，超出后最旧事件被覆盖 |
| `StoreTTL` | 30 min | 超过此时间的事件在回放时被跳过 |

**工作流程**：

1. 每次 `Publish` 时，事件自动写入 EventStore 并分配 `Seq`
2. 前端建立/重连 SSE 时，携带 `Last-Event-ID`（即上次收到的 `Seq`）
3. 后端调用 `SubscribeWithReplay(traceID, sinceSeq)`：先回放 `Seq > sinceSeq` 的历史事件，再转入实时推送
4. 回放在**写锁保护**下执行，保证与实时推送之间**无间隙、无重复**

```go
// SSE Handler — 支持断线重连
sinceSeq := parseLastEventID(r) // 从 Last-Event-ID 请求头解析，默认 0
sub := bus.SubscribeWithReplay(workflowID, sinceSeq)
defer bus.Unsubscribe(sub)

for event := range sub.C() {
    // event.Seq 为全局序列号，作为 SSE id 字段发送
    writeSSE(event.Seq, event)
}
```

---

## fx 集成

```go
// 默认提供 LogDispatcher（开发用）和 NoOp TracerProvider
var Module = fx.Module("outbound", ...)

// 生产环境覆盖为 MultiDispatcher
app := fx.New(
    outbound.Module,
    fx.Provide(outbound.NewMultiDispatcher),  // 覆盖 LogDispatcher
    // ...
)
```

---

## 关键设计决策

| 决策 | 理由 |
|------|------|
| `ChannelSender` 镜像 `bot.Sender` | 避免 outbound → bot 循环依赖，Channel 实现一次即可满足两个接口 |
| `NoteWriter` 最小接口 | 避免直接依赖 memory 包，通过隐式接口 + 适配器桥接 |
| EventBus 非阻塞 | Publish 不阻塞 Pipeline，channel 满时丢弃（旁路不应影响主流程） |
| 回调一次性语义 | Invoke 后自动移除，防止并发重复调用 |
| EventStore 写锁回放 | 回放在写锁下执行，消除回放与实时推送之间的竞态 |
