# Agent — 消息生命周期处理引擎

Agent 模块实现了 Bot 的消息处理架构，采用 **Inbound → Pipeline → Outbound** 三段式生命周期模型。消息从任意输入端进入，经过可编排的 Stage 链加工处理，最终派发到输出渠道。

## 目录结构

```
agent/
├── core/                   # 核心类型定义（零业务依赖）
│   ├── envelope.go         #   Message、Envelope、Action
│   ├── stage.go            #   Stage 接口、StageInfo
│   ├── errors.go           #   PipelineError / AbortError / SkipError
│   └── core_test.go
├── inbound/                # 消息入口层
│   ├── ingress.go          #   Ingress 网关（公共入口）
│   ├── source.go           #   Channel 接口（可选元信息）
│   ├── memory.go           #   MemoryChannel（测试用）
│   ├── module.go           #   fx Module
│   └── memory_test.go
├── pipeline/               # Pipeline 引擎
│   ├── pipeline.go         #   Stage 链式执行引擎
│   ├── predicate.go        #   Predicate 谓词系统 + Router 条件路由
│   ├── middleware.go       #   Middleware 拦截器
│   ├── observable.go       #   Pipeline 可观测性（trace + metrics）
│   ├── observability.go    #   OTel 仪器封装
│   ├── module.go           #   fx Module
│   ├── pipeline_test.go
│   ├── predicate_test.go
│   ├── middleware_test.go
│   └── observable_test.go
├── outbound/               # 消息派发层
│   ├── dispatcher.go       #   Dispatcher 接口 + LogDispatcher + MultiDispatcher
│   ├── channel_handler.go  #   ChannelReplyHandler（Reply/Forward/Broadcast → Sender.Send）
│   ├── note_handler.go     #   NoteHandler（ActionNote → NoteStore 持久化）
│   ├── callback_handler.go #   CallbackHandler（ActionCallback → 回传父 Agent）
│   ├── silent_handler.go   #   SilentHandler（ActionSilent → trace/log only）
│   ├── eventbus.go         #   EventBus 旁路事件总线
│   ├── event_emitter.go    #   EventEmitter（便捷事件发射器）
│   ├── module.go           #   fx Module
│   ├── dispatcher_test.go
│   ├── channel_handler_test.go
│   ├── note_handler_test.go
│   ├── callback_handler_test.go
│   └── event_emitter_test.go
├── memory/                 # 记忆与上下文管理
│   ├── memory.go           #   领域模型：Entry、Scope、Store/Retriever/Repository 接口
│   ├── repository.go       #   MemoryRepository（map 实现，按 Scope 分桶 + 容量淘汰）
│   ├── context.go          #   ContextBuilder + ContextManager（记忆 → LLM context）
│   ├── window.go           #   Window（动态 token 窗口管理）
│   ├── compressor.go       #   Compressor 接口 + LLMCompressor（超限压缩 + [ref:ID]）
│   ├── expander.go         #   Expander（按 Entry ID 加载原文供 LLM 回溯）
│   ├── stage.go            #   MemoryStage(读,Order~100) + MemoryWriteStage(写,Order~900)
│   └── memory_test.go
├── prompt/                 # 系统提示词管理
│   ├── prompt.go           #   Section、Variable、Registry、Assembler
│   ├── stage.go            #   PromptStage（Pipeline 节点 Order=200）
│   └── prompt_test.go
├── bot/                    # Bot 高级抽象（组合 Engine）
│   ├── bot.go              #   Bot：组合 Engine + Channel + EventBus + Handler
│   ├── config.go           #   BotConfig（LLM/Prompt/Memory 等配置）
│   ├── channel.go          #   Channel/Sender 接口定义
│   ├── memory_channel.go   #   MemoryChannel（测试用 Channel + Sender）
│   ├── manager.go          #   BotManager（多 Bot 生命周期管理）
│   ├── module.go           #   fx Module
│   ├── bot_test.go
│   └── manager_test.go
├── stages/                 # 内置 Stage 实现
│   ├── llmroute.go         #   LLMStage（LLM 调用 + tool-calling 循环）
│   ├── reply_stage.go      #   ReplyStage（LLM 决策 + 5 种输出模式）
│   ├── logger.go           #   LoggerStage（结构化日志）
│   ├── filter.go           #   FilterStage（谓词过滤）
│   ├── enricher.go         #   EnricherStage（消息富化）
│   └── reply_stage_test.go
├── engine.go               # Engine 轻量级内核（Inbound→Pipeline→Outbound + Hook）
├── engine_test.go
├── module.go               # 顶层 fx Module
└── README.md
```

## 架构总览

```
    ┌─────────────────────────────────────────────────────────┐
    │                     外部输入端                           │
    │   Webhook Handler / WebSocket / Polling / CLI / ...     │
    └────────────────────┬────────────────────────────────────┘
                         │ ingress.Receive(ctx, msg)
                         ▼
    ┌─────────────────────────────────────────────────────────┐
    │                  Inbound (Ingress)                      │
    │   消息归一化 → 封装 Envelope → 投递到内部 channel         │
    └────────────────────┬────────────────────────────────────┘
                         │ Ingress.C()  (N 个 worker 并发消费)
                         ▼
    ┌─────────────────────────────────────────────────────────┐
    │                  Pipeline Engine                        │
    │   Stage₁ → Stage₂ → ... → StageN                       │
    │   (按 Order 排序，支持 Router 条件分支)                   │
    │                                                         │
    │   内置 Stage 编排（Order 参考值）：                        │
    │     10  LoggerStage      — 入口日志                      │
    │     20  FilterStage      — 谓词过滤                      │
    │     30  EnricherStage    — 消息富化                      │
    │    100  MemoryStage      — 记忆检索 → env.Set            │
    │    200  PromptStage      — 组装 system prompt → env.Set  │
    │    500  ReplyStage/LLM   — LLM 调用 + 输出决策           │
    │    900  MemoryWriteStage — 写入新记忆                    │
    └────────────────────┬────────────────────────────────────┘
                         │ Envelope.Actions()
                         ▼
    ┌─────────────────────────────────────────────────────────┐
    │                Outbound (Dispatcher)                    │
    │   按 ActionType 路由到对应 Handler：                      │
    │     ActionReply/Forward/Broadcast → ChannelReplyHandler │
    │     ActionNote       → NoteHandler → NoteStore          │
    │     ActionCallback   → CallbackHandler → 回传父 Agent   │
    │     ActionSilent     → SilentHandler → trace only       │
    └─────────────────────────────────────────────────────────┘
                         │
                         ▼
    ┌─────────────────────────────────────────────────────────┐
    │               EventBus（旁路事件流）                      │
    │   SSE / WebSocket / 日志 → 实时观察 Bot 运行状态         │
    └─────────────────────────────────────────────────────────┘
```

## 核心概念

### Message

从任意输入端归一化后的统一消息结构：

```go
type Message struct {
    ID        string         // 唯一标识
    BotID     string         // 目标 Bot ID
    Source    string         // 来源（"webhook" / "websocket" / ...）
    Channel   string         // 会话空间标识（频道/群/私聊 ID）
    ChatType  string         // 会话类型（"private" / "group" / "channel"）
    UserID    string         // 发送者 ID
    Text      string         // 文本内容
    MediaType string         // 媒体类型
    RawData   []byte         // 原始载荷
    Metadata  map[string]any // 扩展元数据
    CreatedAt time.Time      // 创建时间
}
```

### Envelope

消息信封，承载消息在 Pipeline 中流转的全部状态。**线程安全**，支持并发读写。

```go
env := core.NewEnvelope(msg)

// Stage 间传递数据
env.Set("user.profile", profile)
val, ok := env.Get("user.profile")

// 累积输出动作
env.AddAction(core.Action{
    Type:    core.ActionReply,
    Channel: msg.Channel,
    Payload: "Hello!",
})

// 控制流
env.Abort(err)       // 中止 Pipeline
env.Aborted() bool   // 检查是否已中止
env.SetErr(err)      // 记录错误（不中止）
```

### Stage

Pipeline 的基本处理单元：

```go
type Stage interface {
    Name() string
    Process(ctx context.Context, env *Envelope) (*Envelope, error)
}
```

**返回值约定：**

| 返回值 | 行为 |
|--------|------|
| `(env, nil)` | 正常继续下一个 Stage |
| `(nil, nil)` | 消息被丢弃，Pipeline 终止 |
| `(env, &AbortError{})` | 立即中止 Pipeline，返回错误 |
| `(env, &SkipError{})` | 跳过当前 Stage，继续下一个 |
| `(env, otherErr)` | 记录错误后继续（非致命） |

### Action

Stage 在处理过程中向 Envelope 累积输出动作，Pipeline 结束后由 Dispatcher 统一派发：

| ActionType | 语义 | Handler |
|-----------|------|---------|
| `ActionReply` | 回复原始消息 | ChannelReplyHandler |
| `ActionForward` | 转发到指定频道/用户 | ChannelReplyHandler |
| `ActionBroadcast` | 广播到多个频道 | ChannelReplyHandler |
| `ActionNote` | 记录内部备注 | NoteHandler |
| `ActionCallback` | 回传父 Agent/任务发起方 | CallbackHandler |
| `ActionSilent` | 主动静默（仅 trace） | SilentHandler |
| `ActionDrop` | 丢弃，不做输出 | — |

### OutputDecision（ReplyStage 决策模式）

ReplyStage 内部 LLM 调用后，由 `ReplyDecider` 决定输出模式：

| Decision | 行为 |
|----------|------|
| `reply` | 正常回复到 Channel |
| `reply_with_note` | 回复 + 记录备注 |
| `note_only` | 不回复，只记录备注 |
| `callback` | 回传给任务发起方 |
| `silent` | 什么都不做，仅 trace |
| `drop` | 完全跳过 |

## Inbound — 消息入口

### 设计哲学

Inbound 层是一个**纯公共接口**，不管理输入端的生命周期。输入端（Webhook server、WebSocket 连接、Polling loop 等）自行管理启停和重连，只需拿到 `Ingress` 实例调一个方法即可注入消息。

### Ingress 网关

```go
// 创建
ingress := inbound.NewIngress(inbound.IngressConfig{
    BufferSize: 256,  // 内部缓冲区大小
}, logger, tracerProvider)

// 注入消息（阻塞式，缓冲区满时等待或 ctx 取消）
err := ingress.Receive(ctx, core.Message{
    ID:     "msg-001",
    Source: "webhook",
    Text:   "Hello bot",
})

// 非阻塞注入（缓冲区满返回 false）
ok := ingress.TryReceive(msg)

// Engine worker 从这里消费
ch := ingress.C()

// 关闭（已缓冲消息仍可被消费）
ingress.Close()
```

### 在真实 Channel 中使用

```go
// Webhook handler 示例
func newWebhookHandler(ingress *inbound.Ingress) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var body WebhookPayload
        json.NewDecoder(r.Body).Decode(&body)

        err := ingress.Receive(r.Context(), core.Message{
            ID:      body.MessageID,
            Source:  "webhook",
            Channel: body.ChannelID,
            UserID:  body.SenderID,
            Text:    body.Content,
        })
        if err != nil {
            http.Error(w, "service unavailable", 503)
            return
        }
        w.WriteHeader(http.StatusOK)
    }
}
```

### Channel 接口（可选）

```go
type Channel interface {
    Name() string  // "misskey-ws"、"telegram-webhook"
    Type() string  // "webhook"、"websocket"、"polling"
}
```

这是可选的元信息接口，用于统一注册和日志。输入端不是必须实现它。

## Pipeline — Stage 链处理引擎

### 执行模型

Pipeline 按 `StageInfo.Order` 升序排列并依次执行所有已启用的 Stage。每个 Stage 处理一个 Envelope 并返回（可能修改过的）Envelope。

```go
stages := []core.StageInfo{
    {Stage: loggerStage,      Order: 10,  Enabled: true},
    {Stage: filterStage,      Order: 20,  Enabled: true},
    {Stage: enrichStage,      Order: 30,  Enabled: true},
    {Stage: memoryStage,      Order: 100, Enabled: true},
    {Stage: promptStage,      Order: 200, Enabled: true},
    {Stage: replyStage,       Order: 500, Enabled: true},
    {Stage: memoryWriteStage, Order: 900, Enabled: true},
}

p, _ := pipeline.New(stages, tracerProvider, meterProvider, logger)
result, err := p.Execute(ctx, envelope)
```

### 错误处理

Pipeline 对不同错误类型有不同策略：

- **`AbortError`**：立即停止所有后续 Stage，返回错误给调用者
- **`SkipError`**：跳过当前 Stage，继续执行下一个（控制流信号，不是错误）
- **其他 `error`**：记录日志后继续执行（非致命）
- **`panic`**：自动恢复（含完整堆栈 `runtime.Stack`），包装为 `PipelineError` 后继续
- **返回 `nil` Envelope**：消息被丢弃，Pipeline 终止

### Predicate 谓词系统

谓词用于条件路由和消息过滤：

```go
// 内置谓词
pipeline.MatchTextContains("help")
pipeline.MatchSource("webhook")
pipeline.MatchChannel("general")

&pipeline.TextHasPrefix{Prefix: "/cmd"}
&pipeline.TextRegex{Pattern: regexp.MustCompile(`\d+`)}
&pipeline.MetadataExists{Key: "priority"}
&pipeline.MetadataEquals{Key: "type", Value: "text"}
&pipeline.ValueExists{Key: "user.profile"}

// 组合谓词
&pipeline.And{Predicates: []pipeline.Predicate{pred1, pred2}}
&pipeline.Or{Predicates: []pipeline.Predicate{pred1, pred2}}
&pipeline.Not{Inner: pred1}

// 特殊谓词
pipeline.MatchAll()   // 始终匹配
pipeline.MatchNone()  // 始终不匹配

// 函数式谓词
pipeline.PredicateFunc(func(env *core.Envelope) bool {
    return env.Message.Text != ""
})
```

### Router 条件路由

Router 本身是一个 Stage，根据 Predicate 将消息分发到不同的 Stage 子链：

```go
router := pipeline.NewRouter("message-router",
    pipeline.Route{
        Name:      "command",
        Predicate: &pipeline.TextHasPrefix{Prefix: "/"},
        Stages:    []core.Stage{commandParser, commandExecutor},
    },
    pipeline.Route{
        Name:      "question",
        Predicate: pipeline.MatchTextContains("?"),
        Stages:    []core.Stage{llmStage},
    },
    pipeline.Route{
        Name:     "default",
        Fallback: true,
        Stages:   []core.Stage{echoStage},
    },
)
```

匹配规则：按顺序检查，第一个匹配的 Route 执行对应子链，无匹配走 Fallback，都没有则透传 Envelope。

### Middleware 拦截器

Middleware 在 Stage 执行前后插入逻辑：

```go
type Middleware func(next core.Stage) core.Stage

// 应用 middleware（从外到内包装）
wrapped := pipeline.WithMiddleware(myStage,
    pipeline.RecoveryMiddleware(),                    // panic 恢复
    pipeline.TimeoutMiddleware(5 * time.Second),      // 超时控制
    pipeline.LoggingMiddleware(logger),               // 前后日志
)
```

**内置 Middleware：**

| Middleware | 作用 |
|-----------|------|
| `RecoveryMiddleware()` | panic 恢复 → `PipelineError`（含完整堆栈） |
| `TimeoutMiddleware(d)` | Stage 超时控制（goroutine + channel） |
| `LoggingMiddleware(logger)` | Stage 前后结构化日志 + duration |

## Memory — 记忆与上下文管理

### 设计哲学

采用 DDD 领域驱动 + CQRS 读写分离架构。`Entry` 是核心聚合根，记忆按 `Scope` 分桶隔离。当前为纯内存实现（`map[string][]Entry`），接口设计面向未来持久化后端替换（SQLite / Redis / 向量 DB）。

### Scope 分层

| ScopeKind | 含义 | 典型场景 |
|-----------|------|---------|
| `channel` | 会话级 | 群聊上下文、私聊历史 |
| `user` | 用户级 | 用户偏好、跨对话长期记忆 |
| `bot` | Bot 级 | Bot 学到的通用知识 |
| `global` | 全局 | 平台级配置、共享知识 |

### 核心接口

```go
// Store — 写侧（只负责存储）
type Store interface {
    Save(ctx context.Context, entry Entry) error
}

// Retriever — 读侧（只负责检索）
type Retriever interface {
    Retrieve(ctx context.Context, scope Scope, opts ...RetrieveOption) ([]Entry, error)
}

// Repository — 聚合 Store + Retriever（完整仓储）
type Repository interface {
    Store
    Retriever
}
```

### 动态上下文窗口

```
LLM Usage → Window.RecordUsage → 下轮 Available() 缩小
→ 超限时 Compressor 压缩历史（保留 [ref:ID]）
→ LLM 需详情时 Expander.Expand(ids) → 加载原文
```

- **Window**：跟踪 LLM 每次返回的 token 用量，动态计算 memory 可用预算
- **Compressor**：超限时调 LLM 压缩历史上下文，保留 `[ref:ID]` 引用标记
- **Expander**：解析 `[ref:ID]` 引用，按 Entry ID 加载原文

### Pipeline 集成

| Stage | Order | 职责 |
|-------|-------|------|
| `MemoryStage` | ~100 | 读侧：检索相关记忆 → `env.Set("memory.context", ...)` |
| `MemoryWriteStage` | ~900 | 写侧：将本轮交互写入记忆存储 |

## Prompt — 系统提示词管理

### 设计理念

将 system prompt 从硬编码字符串升级为模块化、可组装、条件激活的模板系统。

### 核心组件

| 组件 | 职责 |
|------|------|
| `Section` | 提示词段落，带 Order 排序 + 条件激活 + 模板变量 |
| `Variable` | 变量定义，支持 3 种来源（静态 / Envelope KV / 动态函数） |
| `Registry` | Section 注册中心，线程安全，支持运行时动态增删 |
| `Assembler` | 组装器：解析变量 → 渲染模板 → 按 Order 拼接 |
| `PromptStage` | Pipeline 节点（Order=200） |

### Section 排序约定

| Order 范围 | 用途 |
|-----------|------|
| 0-99 | 身份定义（Bot 角色、人格） |
| 100-199 | 行为规则（回复限制、禁止事项） |
| 200-299 | 上下文注入（记忆、对话历史） |
| 300-399 | 工具说明（可用工具列表） |
| 400-499 | 格式约束（输出格式要求） |

### 变量来源

```go
// 静态值
Variable{Name: "bot_name", Source: SourceStatic, StaticValue: "栞娜"}

// 从 Envelope KV 读取
Variable{Name: "memory", Source: SourceEnvelopeKV, EnvelopeKey: "memory.context"}

// 动态函数
Variable{Name: "time", Source: SourceFunc, Func: func(ctx *AssemblyContext) string {
    return ctx.Timestamp.Format("2006-01-02 15:04")
}}
```

### 条件激活

```go
Section{
    Name:  "group_rules",
    Order: 150,
    Template: "在群聊中，请遵守以下规则：...",
    Conditional: func(ctx *AssemblyContext) bool {
        return ctx.ChatType == "group"  // 仅群聊时注入
    },
}
```

### Pipeline 集成

PromptStage 工作流：
1. 从 env KV 读取上游数据（`memory.context`、`bot.config` 等）
2. 收集 Registry 中所有 Section + Variable 引用的 Envelope KV
3. 调用 Assembler 组装完整 system prompt
4. `env.Set("system.prompt", result)` 供下游 LLM Stage 消费

LLMStage / ReplyStage 优先读 `env.Get("system.prompt")`，无则回退静态配置。

## Outbound — 消息派发

### Dispatcher 接口

```go
type Dispatcher interface {
    Dispatch(ctx context.Context, actions []core.Action) error
}
```

### MultiDispatcher（生产用）

按 ActionType 路由到不同处理器：

```go
md := outbound.NewMultiDispatcher(logger, tracerProvider)

md.Register(core.ActionReply, replyHandler)
md.Register(core.ActionNote, noteHandler)
md.Register(core.ActionCallback, callbackHandler)
md.Register(core.ActionSilent, silentHandler)

// 兜底处理器
md.SetFallback(outbound.ActionHandlerFunc(func(ctx context.Context, a core.Action) error {
    log.Warnf("unhandled action type: %s", a.Type)
    return nil
}))

// 校验必要 handler 已注册
md.Validate()
```

### ChannelReplyHandler

处理 Reply / Forward / Broadcast，通过注册的 `Sender` 发送到 Channel：

```go
handler := outbound.NewChannelReplyHandler(logger, tracerProvider)
handler.RegisterSender("misskey-ws", misskeyClient)
handler.RegisterSender("telegram", telegramClient)

// Sender 接口
type Sender interface {
    Send(ctx context.Context, channel string, payload any) error
}
```

### NoteHandler

处理 ActionNote，将备注持久化到 NoteStore：

```go
handler := outbound.NewNoteHandler(noteStore, logger, tracerProvider)

// NoteStore 接口
type NoteStore interface {
    Save(ctx context.Context, note Note) error
    List(ctx context.Context, botID string, opts ...NoteListOption) ([]Note, error)
}
```

内置 `MemoryNoteStore`（带 MaxNotes + TTL 容量控制）。

### CallbackHandler

处理 ActionCallback，将结果回传给任务发起方（sub-agent 场景）：

```go
handler := outbound.NewCallbackHandler(registry, logger, tracerProvider)

// 注册回调
registry.Register("task-123", func(ctx context.Context, result any) error {
    // 处理 sub-agent 返回的结果
    return nil
})
```

### EventBus（旁路事件总线）

非阻塞的事件广播机制，供 SSE/WebSocket 实时推送 Bot 状态：

```go
bus := outbound.NewEventBus(outbound.EventBusConfig{
    BufferSize: 256,
}, logger)

// 订阅
sub := bus.Subscribe()
defer sub.Unsubscribe()
for event := range sub.C() {
    // event.Type: "message.received" / "llm.text_delta" / ...
    // event.Data: map[string]any{...}
}

// 发布（非阻塞，满则丢弃 + 计数）
bus.Send(outbound.Event{
    Type:    outbound.EventLLMTextDelta,
    TraceID: traceID,
    Data:    map[string]any{"text": "Hello"},
})
```

**内置事件类型：**

| 分类 | 事件 | 说明 |
|------|------|------|
| 消息生命周期 | `message.received` / `message.done` / `message.dropped` / `message.error` | 消息处理全程 |
| Pipeline Stage | `stage.enter` / `stage.exit` / `stage.skip` / `stage.error` | Stage 进出 |
| LLM 流式 | `llm.start` / `llm.text_delta` / `llm.reason_delta` / `llm.tool_call` / `llm.done` | LLM 调用过程 |
| 记忆 | `memory.retrieved` / `memory.written` | 记忆读写 |
| 提示词 | `prompt.assembled` | 提示词组装完成 |

## Bot — 高级抽象

### 设计哲学

Bot 组合 Engine（has-a），在轻量级内核上叠加业务能力：多 Channel 管理、Handler 自动注册、EventBus 集成。**Engine 是可复用内核，Bot 是面向业务的完整实体。**

### 创建 Bot

```go
bot, err := bot.New(bot.BotParams{
    ID:   "customer-service",
    Name: "客服小助手",
    Config: bot.BotConfig{
        SystemPrompt: "你是一个友好的客服机器人...",
        LLM: bot.LLMParams{
            Model:       &model,
            Temperature: &temp,
            MaxTokens:   &maxTokens,
        },
    },
    Pipeline:   myPipeline,
    Dispatcher: multiDispatcher,
    Channels:   []bot.Channel{misskeyChannel, telegramChannel},
    EventBus:   eventBus,  // 可选
    Logger:     logger,
    TP:         tracerProvider,
})
```

### Bot 消息流转

```
[Inbound] Channel.onMessage()
  → msg.BotID = channel.BotID()
  → bot.Ingress().Receive(ctx, msg)
  → Engine worker 从 ingress.C() 消费
  → pipeline.Execute(ctx, env)
  → dispatcher.Dispatch(ctx, actions)

[Outbound] Dispatcher 路由 Action 到对应 Handler：
  ActionReply/Forward/Broadcast → ChannelReplyHandler → Sender.Send()
  ActionNote     → NoteHandler → NoteStore.Save()
  ActionCallback → CallbackHandler → CallbackRegistry.Invoke()
  ActionSilent   → SilentHandler → trace/log only
```

### BotManager

管理平台中所有 Bot 实例的注册、查找和生命周期：

```go
mgr := bot.NewBotManager(logger, tracerProvider)
mgr.Register(bot1)
mgr.Register(bot2)

// 批量启动（等待所有 Bot 真正就绪）
ctx, cancel := context.WithCancel(context.Background())
mgr.RunAll(ctx)

// 查询
b := mgr.Get("customer-service")
infos := mgr.Info()  // 所有 Bot 状态快照

// 优雅关闭
mgr.StopAll()
```

## Engine — 轻量级内核

Engine 组合 Inbound、Pipeline、Outbound 三层，运行消息处理循环：

```go
engine := agent.NewEngine(ingress, pipeline, dispatcher, agent.EngineConfig{
    Workers:         8,               // 8 个并发 worker
    ShutdownTimeout: 15 * time.Second,
}, logger, tracerProvider)

// 阻塞运行（直到 ctx 取消）
err := engine.Run(ctx)

// 优雅关闭
engine.Stop()
```

### EngineHook（生命周期扩展点）

Bot 通过实现 `EngineHook` 接口注入行为，无需修改 Engine 代码：

```go
type EngineHook interface {
    OnStart(ctx context.Context) error            // Engine 启动时
    OnStop(ctx context.Context) error             // Engine 停止时
    OnEnvelopeReceived(ctx context.Context, env *core.Envelope) // 收到消息
    OnEnvelopeProcessed(ctx context.Context, env *core.Envelope, err error) // 处理完成
    OnActionDispatched(ctx context.Context, actions []core.Action, err error) // 派发完成
    OnWorkerPanic(ctx context.Context, r any, stack []byte) // Worker panic
}
```

**处理流程：**

1. N 个 worker goroutine 从 `Ingress.C()` 并发取 Envelope
2. 调用 `Hook.OnEnvelopeReceived`
3. `Pipeline.Execute()` 按序过 Stage 链
4. 调用 `Hook.OnEnvelopeProcessed`
5. `Dispatcher.Dispatch()` 派发 Action
6. 调用 `Hook.OnActionDispatched`
7. `ctx` 取消时：关闭 Ingress → 排空缓冲区 → 等待 worker 退出 → 超时兜底

## 内置 Stage

### ReplyStage（推荐）

对接 `llm` 模块并根据 `ReplyDecider` 决策输出模式。支持 5 种输出组合：

```go
stage := stages.NewReplyStage("reply", stages.ReplyStageConfig{
    LLM: stages.LLMConfig{
        SystemPrompt: "You are a helpful bot.",
        Model:        &model,
        Temperature:  &temp,
    },
    Decider: stages.PrefixDecider(),  // 按 LLM 输出前缀决策
}, provider, tracerProvider, logger)
```

**PrefixDecider 协议：** LLM 输出以特殊前缀开头决定行为：
- `[REPLY]...` → 正常回复
- `[NOTE]...` → 只记备注
- `[SILENT]` → 主动静默
- 无前缀 → 默认回复

### LLMStage

纯 LLM 调用 + tool-calling 循环（无决策逻辑）：

```go
stage := stages.NewLLMStage("gpt", grokProvider, stages.LLMConfig{
    SystemPrompt: "You are a helpful bot.",
    MaxSteps:     5,           // 最多 5 步 tool-calling
    Tools:        myTools,
    Model:        &myModel,
    Temperature:  &temp,
}, tracerProvider, logger)
```

### LoggerStage

结构化日志记录，可选记录消息文本（截断 500 字符）：

```go
stage := stages.NewLoggerStage("audit-log", logger, true /* logPayload */)
```

### FilterStage

基于 Predicate 的消息过滤：

```go
// 只放行包含 "bot" 的消息
pass := stages.NewFilterStage("bot-filter",
    pipeline.MatchTextContains("bot"),
    stages.FilterPass, logger)

// 丢弃来自 "spam" 源的消息
drop := stages.NewFilterStage("spam-filter",
    pipeline.MatchSource("spam"),
    stages.FilterDrop, logger)
```

### EnricherStage

自定义函数为消息附加额外信息：

```go
stage := stages.NewEnricherStage("user-enricher",
    func(ctx context.Context, env *core.Envelope) error {
        profile, err := userService.GetProfile(ctx, env.Message.UserID)
        if err != nil { return err }
        env.Set("user.profile", profile)
        return nil
    }, logger)
```

## fx 依赖注入

整个模块使用 [uber-go/fx](https://github.com/uber-go/fx) 做依赖注入。

### 快速启动

```go
app := fx.New(
    // 引入 Agent 模块（pipeline + inbound + outbound + engine）
    agent.Module,

    // 引入 Bot 模块
    bot.Module,

    // 提供基础依赖
    fx.Provide(zap.NewDevelopment),
    fx.Provide(func(l *zap.Logger) *zap.SugaredLogger { return l.Sugar() }),

    // 注册 Stage 到 Pipeline（通过 fx group）
    pipeline.ProvideStageInfo(func(logger *zap.SugaredLogger) core.StageInfo {
        return core.StageInfo{
            Stage:   stages.NewLoggerStage("logger", logger, true),
            Order:   10,
            Enabled: true,
        }
    }),
    pipeline.ProvideStageInfo(func(logger *zap.SugaredLogger, tp trace.TracerProvider) core.StageInfo {
        return core.StageInfo{
            Stage:   stages.NewLLMStage("llm", myProvider, myConfig, tp, logger),
            Order:   100,
            Enabled: true,
        }
    }),

    // 使用 Ingress 注入消息
    fx.Invoke(func(ingress *inbound.Ingress) {
        // 启动你的 webhook server / ws 连接 / polling loop
        // 收到消息时调用 ingress.Receive(ctx, msg)
    }),
)

app.Run()
```

### Module 组合

```
agent.Module
├── pipeline.Module    (收集 group:"pipeline_stages" → 构建 Pipeline)
├── inbound.Module     (提供 Ingress)
├── outbound.Module    (提供默认 LogDispatcher)
├── EngineConfig       (默认配置)
├── Engine             (构建 + Lifecycle hooks)
└── 默认 OTel NoOp providers (TracerProvider, MeterProvider)

bot.Module
├── BotManager         (多 Bot 管理)
└── 默认 NoteStore / CallbackRegistry
```

## 可观测性 (OpenTelemetry)

全链路集成 OpenTelemetry，默认提供 NoOp 实现，接入只需替换 Provider。

### Traces

| Span | 位置 | 属性 |
|------|------|------|
| `ingress.receive` | Ingress | message.id, source, channel |
| `engine.process` | Engine | worker.id, message.id, source, channel |
| `pipeline.execute` | Pipeline | message.id, source, channel, actions count, duration |
| `stage.<name>` | 每个 Stage | stage.name, message.id, duration |
| `stage.llm.orchestrate` | LLMStage | llm.provider, tokens, steps, finish_reason |
| `stage.prompt.assemble` | PromptStage | sections_count, variables_resolved, result_length |
| `stage.memory.retrieve` | MemoryStage | scope, entries_found |
| `stage.memory.write` | MemoryWriteStage | scope, entries_written |
| `outbound.dispatch` | Dispatcher | actions.count |
| `outbound.note.save` | NoteHandler | note.id, note.category |
| `outbound.callback` | CallbackHandler | callback.id |

### Metrics

| 指标 | 类型 | 说明 |
|------|------|------|
| `pipeline.messages.processed` | Counter | 进入 Pipeline 的消息总数 |
| `pipeline.messages.errors` | Counter | 处理错误总数 |
| `pipeline.messages.dropped` | Counter | 被 Stage 丢弃的消息总数 |
| `pipeline.stage.duration_seconds` | Histogram | Stage 处理耗时 |
| `eventbus.events.published` | Counter | EventBus 发布事件总数 |
| `eventbus.events.dropped` | Counter | EventBus 因缓冲区满丢弃的事件数 |
| `memory.entries.stored` | Counter | 记忆存储操作数 |
| `memory.entries.retrieved` | Counter | 记忆检索操作数 |
| `prompt.assemblies` | Counter | 提示词组装次数 |
| `prompt.assembly.duration_ms` | Histogram | 提示词组装耗时 |

### 接入真实 Exporter

```go
app := fx.New(
    agent.Module,

    // 替换 NoOp 为真实 Provider
    fx.Provide(func() trace.TracerProvider {
        return jaegerExporter.TracerProvider()
    }),
    fx.Provide(func() metric.MeterProvider {
        return prometheusExporter.MeterProvider()
    }),
)
```

## 扩展开发指南

### 编写自定义 Stage

```go
type MyStage struct {
    logger *zap.SugaredLogger
}

func (s *MyStage) Name() string { return "my-stage" }

func (s *MyStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
    // 读取前序 Stage 的数据
    profile, ok := env.Get("user.profile")

    // 修改消息（通过 KV 传递给后续 Stage）
    env.Set("my-stage.result", someResult)

    // 累积输出动作
    env.AddAction(core.Action{
        Type:    core.ActionReply,
        Channel: env.Message.Channel,
        Payload: "processed!",
    })

    // 控制流：
    // return nil, nil                             // 丢弃消息
    // return env, &core.AbortError{Reason: "..."}  // 中止 Pipeline
    // return env, &core.SkipError{Reason: "..."}   // 跳过当前 Stage

    return env, nil
}
```

### 编写自定义 Dispatcher Handler

```go
type TelegramHandler struct {
    client *telegram.Client
}

func (h *TelegramHandler) Handle(ctx context.Context, action core.Action) error {
    return h.client.SendMessage(ctx, action.Channel, action.Payload.(string))
}

// 注册到 MultiDispatcher
md.Register(core.ActionReply, telegramHandler)
```

### 编写自定义输入端

```go
// WebSocket 输入端示例
type WSChannel struct {
    name    string
    botID   string
    ingress *inbound.Ingress
    conn    *websocket.Conn
}

func (ws *WSChannel) Name() string  { return ws.name }
func (ws *WSChannel) Type() string  { return "websocket" }
func (ws *WSChannel) BotID() string { return ws.botID }

func (ws *WSChannel) Listen(ctx context.Context) error {
    for {
        _, data, err := ws.conn.ReadMessage()
        if err != nil { return err }
        msg := parseWSMessage(data)
        msg.BotID = ws.botID
        ws.ingress.Receive(ctx, msg)
    }
}

// 实现 Sender 接口（可选，用于 Outbound 回写）
func (ws *WSChannel) Send(ctx context.Context, channel string, payload any) error {
    return ws.conn.WriteJSON(payload)
}
```

### 注册 System Prompt Section

```go
// 在 Bot 初始化时注册 prompt 段落
registry := prompt.NewRegistry()

registry.Register(prompt.Section{
    Name:     "identity",
    Order:    10,
    Template: "你是{{.bot_name}}，{{.bot_role}}。",
    Variables: []prompt.Variable{
        {Name: "bot_name", Source: prompt.SourceStatic, StaticValue: "栞娜"},
        {Name: "bot_role", Source: prompt.SourceStatic, StaticValue: "一个智慧且温柔的 AI 助手"},
    },
})

registry.Register(prompt.Section{
    Name:     "memory_context",
    Order:    200,
    Template: "以下是你的记忆上下文：\n{{.memory}}",
    Variables: []prompt.Variable{
        {Name: "memory", Source: prompt.SourceEnvelopeKV, EnvelopeKey: "memory.context"},
    },
})

registry.Register(prompt.Section{
    Name:     "group_rules",
    Order:    150,
    Template: "当前为群聊环境，请注意：不要过度活跃，只在被 @ 或话题相关时回复。",
    Conditional: func(ctx *prompt.AssemblyContext) bool {
        return ctx.ChatType == "group"
    },
})
```

## Envelope KV 约定

Pipeline 中各 Stage 通过 Envelope KV 传递数据，以下是已使用的 key：

| Key | 写入者 | 消费者 | 类型 | 说明 |
|-----|--------|--------|------|------|
| `memory.context` | MemoryStage | PromptStage | `string` | 格式化后的记忆上下文文本 |
| `memory.entries_used` | MemoryStage | — | `int` | 使用的记忆条目数 |
| `memory.compressed` | MemoryStage | — | `bool` | 是否触发了压缩 |
| `system.prompt` | PromptStage | LLMStage/ReplyStage | `string` | 组装好的完整 system prompt |
| `bot.id` | Bot/Engine | PromptStage | `string` | 当前 Bot ID |
| `bot.config` | Bot | PromptStage | `BotConfig` | Bot 配置 |
| `llm.result` | LLMStage | MemoryWriteStage | `*llm.GenerateResult` | LLM 调用结果 |

## 依赖

| 包 | 用途 |
|----|------|
| `go.uber.org/fx` | 依赖注入 |
| `go.opentelemetry.io/otel` | OpenTelemetry API |
| `go.opentelemetry.io/otel/trace` | 分布式追踪 |
| `go.opentelemetry.io/otel/metric` | 指标采集 |
| `go.uber.org/zap` | 结构化日志 |

## 测试

```bash
# 运行 agent 全部测试
go test ./agent/... -v

# 运行特定子包
go test ./agent/core/ -v
go test ./agent/pipeline/ -v
go test ./agent/inbound/ -v
go test ./agent/outbound/ -v
go test ./agent/memory/ -v
go test ./agent/prompt/ -v
go test ./agent/bot/ -v
go test ./agent/stages/ -v
go test ./agent/ -v             # Engine 集成测试
```

当前共 **222 个测试**（9 个子包全部通过），覆盖：

- **core**: Envelope、Stage、Error 类型、Action 全类型
- **pipeline**: Stage 链执行、排序、中止、跳过、panic 恢复、可观测性
- **pipeline/predicate**: 所有谓词 + 组合谓词 + Router
- **pipeline/middleware**: Recovery、Timeout、Logging、执行顺序
- **inbound**: Ingress + MemoryChannel 全场景
- **outbound**: MultiDispatcher + ChannelReplyHandler + NoteHandler + CallbackHandler + EventBus
- **memory**: Repository CRUD + 容量淘汰 + Window + Compressor + Expander + Stage 集成
- **prompt**: Section 排序 + 变量解析 + 条件激活 + Assembler + PromptStage 集成
- **bot**: Bot 创建/校验 + 端到端消息流 + 多 Channel + 5 种输出模式 + BotManager 生命周期
- **stages**: ReplyStage 决策模式 + LLMStage + FilterStage
- **engine**: 端到端集成、多 Source、优雅关闭、EngineHook
