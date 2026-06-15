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
│   ├── middleware.go        #   Middleware 拦截器
│   ├── observability.go    #   OTel 仪器封装
│   ├── module.go           #   fx Module
│   ├── pipeline_test.go
│   ├── predicate_test.go
│   └── middleware_test.go
├── outbound/               # 消息派发层
│   ├── dispatcher.go       #   Dispatcher 接口 + LogDispatcher + MultiDispatcher
│   ├── module.go           #   fx Module
│   └── dispatcher_test.go
├── stages/                 # 内置 Stage 实现
│   ├── llmroute.go         #   LLMStage（LLM 调用）
│   ├── logger.go           #   LoggerStage（结构化日志）
│   ├── filter.go           #   FilterStage（谓词过滤）
│   └── enricher.go         #   EnricherStage（消息富化）
├── engine.go               # 顶层 Engine（编排 Inbound→Pipeline→Outbound）
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
    │   每个 Stage：                                           │
    │   ┌─ OTel Span ──────────────────────────────────────┐  │
    │   │  input Envelope → process → output Envelope      │  │
    │   │  累积 Action / 设置 KV / 错误控制                  │  │
    │   └──────────────────────────────────────────────────┘  │
    └────────────────────┬────────────────────────────────────┘
                         │ Envelope.Actions()
                         ▼
    ┌─────────────────────────────────────────────────────────┐
    │                Outbound (Dispatcher)                    │
    │   按 ActionType 路由 → Reply / Forward / Broadcast      │
    └─────────────────────────────────────────────────────────┘
```

## 核心概念

### Message

从任意输入端归一化后的统一消息结构：

```go
type Message struct {
    ID        string         // 唯一标识
    Source    string         // 来源（"webhook" / "websocket" / ...）
    Channel   string         // 频道/会话 ID
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

| ActionType | 语义 |
|-----------|------|
| `ActionReply` | 回复原始消息 |
| `ActionForward` | 转发到指定频道/用户 |
| `ActionBroadcast` | 广播到多个频道 |
| `ActionDrop` | 丢弃，不做输出 |

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

### MemoryChannel（测试用）

```go
mem := inbound.NewMemoryChannel("test", ingress)
mem.Send(ctx, core.Message{ID: "1", Text: "hello"})
```

## Pipeline — Stage 链处理引擎

### 执行模型

Pipeline 按 `StageInfo.Order` 升序排列并依次执行所有已启用的 Stage。每个 Stage 处理一个 Envelope 并返回（可能修改过的）Envelope。

```go
stages := []core.StageInfo{
    {Stage: loggerStage,  Order: 10,  Enabled: true},
    {Stage: filterStage,  Order: 20,  Enabled: true},
    {Stage: enrichStage,  Order: 30,  Enabled: true},
    {Stage: routerStage,  Order: 50,  Enabled: true},
    {Stage: llmStage,     Order: 100, Enabled: true},
}

p, _ := pipeline.New(stages, tracerProvider, meterProvider, logger)
result, err := p.Execute(ctx, envelope)
```

### 错误处理

Pipeline 对不同错误类型有不同策略：

- **`AbortError`**：立即停止所有后续 Stage，返回错误给调用者
- **`SkipError`**：跳过当前 Stage，继续执行下一个（控制流信号，不是错误）
- **其他 `error`**：记录日志后继续执行（非致命）
- **`panic`**：自动恢复，包装为 `PipelineError` 后继续
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
| `RecoveryMiddleware()` | panic 恢复 → `PipelineError` |
| `TimeoutMiddleware(d)` | Stage 超时控制（goroutine + channel） |
| `LoggingMiddleware(logger)` | Stage 前后结构化日志 + duration |

## Outbound — 消息派发

### Dispatcher 接口

```go
type Dispatcher interface {
    Dispatch(ctx context.Context, actions []core.Action) error
}
```

### LogDispatcher（开发/测试用）

将所有 Action 记录到日志，不做实际投递：

```go
d := outbound.NewLogDispatcher(logger, tracerProvider)
```

### MultiDispatcher（生产用）

按 ActionType 路由到不同处理器：

```go
md := outbound.NewMultiDispatcher(logger, tracerProvider)

// 注册处理器
md.Register(core.ActionReply, outbound.ActionHandlerFunc(func(ctx context.Context, a core.Action) error {
    // 调用消息平台 API 发送回复
    return sendReply(ctx, a.Channel, a.Payload)
}))

md.Register(core.ActionForward, forwardHandler)

// 兜底处理器
md.SetFallback(outbound.ActionHandlerFunc(func(ctx context.Context, a core.Action) error {
    log.Warnf("unhandled action type: %s", a.Type)
    return nil
}))
```

## 内置 Stage

### LLMStage

对接项目 `llm` 模块的 `OrchestrateGenerate`，支持多步 tool-calling 循环：

```go
stage := stages.NewLLMStage("gpt", grokProvider, stages.LLMConfig{
    SystemPrompt: "You are a helpful bot.",
    MaxSteps:     5,           // 最多 5 步 tool-calling
    Tools:        myTools,     // 可用工具
    Model:        &myModel,
    Temperature:  &temp,
}, tracerProvider, logger)
```

LLM 结果自动存入 Envelope KV（`llm.result`），回复添加为 `ActionReply`。
OTel span 记录 provider、tokens、steps、finish_reason。

### LoggerStage

结构化日志记录，可选记录消息文本（截断 500 字符），只记录 metadata keys 不泄露 values：

```go
stage := stages.NewLoggerStage("audit-log", logger, true /* logPayload */)
```

### FilterStage

基于 Predicate 的消息过滤：

```go
// 只放行包含 "bot" 的消息
pass := stages.NewFilterStage("bot-filter",
    pipeline.MatchTextContains("bot"),
    stages.FilterPass,
    logger)

// 丢弃来自 "spam" 源的消息
drop := stages.NewFilterStage("spam-filter",
    pipeline.MatchSource("spam"),
    stages.FilterDrop,
    logger)
```

### EnricherStage

自定义函数为消息附加额外信息：

```go
stage := stages.NewEnricherStage("user-enricher",
    func(ctx context.Context, env *core.Envelope) error {
        profile, err := userService.GetProfile(ctx, env.Message.UserID)
        if err != nil {
            return err
        }
        env.Set("user.profile", profile)
        return nil
    },
    logger)
```

## Engine — 顶层编排

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

**处理流程：**

1. N 个 worker goroutine 从 `Ingress.C()` 并发取 Envelope
2. 每个 Envelope 经 `Pipeline.Execute()` 按序过 Stage 链
3. Pipeline 产出的 Action 列表交给 `Dispatcher.Dispatch()` 派发
4. `ctx` 取消时：关闭 Ingress → 排空缓冲区 → 等待 worker 退出 → 超时兜底

## fx 依赖注入

整个模块使用 [uber-go/fx](https://github.com/uber-go/fx) 做依赖注入。

### 快速启动

```go
app := fx.New(
    // 引入 Agent 模块（自动包含 pipeline + inbound + outbound + engine）
    agent.Module,

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

### Stage 注册方式

```go
// 方式一：ProvideStageInfo — 完全控制 Order 和 Enabled
pipeline.ProvideStageInfo(func(deps ...) core.StageInfo {
    return core.StageInfo{Stage: myStage, Order: 50, Enabled: true}
})

// 方式二：ProvideStage — 简化版，自动启用
pipeline.ProvideStage(stages.NewLoggerStage, 10)  // constructor + order
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
| `outbound.dispatch` | Dispatcher | actions.count |

### Metrics

| 指标 | 类型 | 说明 |
|------|------|------|
| `pipeline.messages.processed` | Counter | 进入 Pipeline 的消息总数 |
| `pipeline.messages.errors` | Counter | 处理错误总数 |
| `pipeline.messages.dropped` | Counter | 被 Stage 丢弃的消息总数 |
| `pipeline.stage.duration_seconds` | Histogram | Stage 处理耗时（含 `_pipeline_total` 标签） |

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

    // ... 其他配置
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

### 使用 StageFunc 快速创建

```go
stage := &core.StageFunc{
    StageName: "quick-transform",
    Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
        env.Message.Text = strings.ToUpper(env.Message.Text)
        return env, nil
    },
}
```

### 编写自定义 Dispatcher

```go
type TelegramDispatcher struct {
    client *telegram.Client
}

func (d *TelegramDispatcher) Dispatch(ctx context.Context, actions []core.Action) error {
    for _, a := range actions {
        switch a.Type {
        case core.ActionReply:
            d.client.SendMessage(ctx, a.Channel, a.Payload.(string))
        case core.ActionForward:
            d.client.ForwardMessage(ctx, a.Channel, a.Payload)
        }
    }
    return nil
}
```

### 编写自定义输入端

```go
// WebSocket 输入端示例
type WSChannel struct {
    name    string
    ingress *inbound.Ingress
    conn    *websocket.Conn
}

func (ws *WSChannel) Name() string { return ws.name }
func (ws *WSChannel) Type() string { return "websocket" }

func (ws *WSChannel) Listen(ctx context.Context) error {
    for {
        _, data, err := ws.conn.ReadMessage()
        if err != nil {
            return err
        }
        msg := parseWSMessage(data)
        ws.ingress.Receive(ctx, msg)  // 就这一行
    }
}
```

## 依赖

| 包 | 版本 | 用途 |
|----|------|------|
| `go.uber.org/fx` | v1.24.0 | 依赖注入 |
| `go.opentelemetry.io/otel` | v1.44.0 | OpenTelemetry API |
| `go.opentelemetry.io/otel/trace` | v1.44.0 | 分布式追踪 |
| `go.opentelemetry.io/otel/metric` | v1.44.0 | 指标采集 |
| `go.uber.org/zap` | — | 结构化日志 |

## 测试

```bash
# 运行 agent 全部测试
go test ./agent/... -v

# 运行特定子包
go test ./agent/core/ -v
go test ./agent/pipeline/ -v
go test ./agent/inbound/ -v
go test ./agent/outbound/ -v
go test ./agent/ -v             # Engine 集成测试
```

当前共 **56 个测试**，覆盖：

- **core**: Envelope、Stage、Error 类型（11 个）
- **pipeline**: Stage 链执行、排序、中止、跳过、panic 恢复（10 个）
- **pipeline/predicate**: 所有谓词 + 组合谓词 + Router（17 个）
- **pipeline/middleware**: Recovery、Timeout、Logging、执行顺序（6 个）
- **inbound**: Ingress + MemoryChannel 全场景（11 个）
- **outbound**: LogDispatcher + MultiDispatcher（5 个）
- **engine**: 端到端集成、多 Source、优雅关闭（7 个 ← 含 TestDefaults 等）
