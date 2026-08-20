# core — 核心类型定义

Pipeline 框架的零业务依赖核心包，定义消息处理流水线中的所有基础类型。

## 功能

- 定义统一消息类型 `Message`（跨渠道归一化）
- 定义线程安全的消息信封 `Envelope`（Stage 间状态传递）
- 定义输出动作 `Action` 和 7 种 `ActionType`（reply/forward/broadcast/note/callback/silent/drop）
- 定义 `Stage` 接口及 `StageInfo` 元数据
- 定义条件匹配器 `Predicate`（路由 / 过滤复用）
- 定义 Pipeline 控制流错误类型（`PipelineError`/`AbortError`/`SkipError`）
- 定义多模态附件类型 `Attachment`
- **警告系统**（`warning.go`）：允许中间件向 Envelope 注入软/硬警告，Stage 可消费并合并到 System Prompt
- **活动轨迹**（`events.go`）：`EventSink` append-only 事件流（stage 边界、工具调用/结果、LLM 请求/响应、上下文注入、子 Agent 派生、HITL defer/resume），默认零侵入 `NoopSink`，可注入 `MemorySink`（有界环，供回放/自检）；经 `WithEventSink`/`EventSinkFromContext` 在 context 中传递

## 源文件

| 文件 | 内容 |
|------|------|
| `envelope.go` | `Message`、`Action`/`ActionType`、`Envelope` |
| `stage.go` | `Stage`、`StageFunc`、`StageInfo` |
| `predicate.go` | `Predicate`、`PredicateFunc` |
| `errors.go` | `PipelineError`、`AbortError`、`SkipError` 及判定函数 |
| `attachment.go` | `Attachment` 及类型判定/提取辅助函数 |
| `warning.go` | `Warning` 与延迟警告注入 |
| `events.go` | `Event`/`EventKind`、`EventSink` 接口 + `NoopSink`/`MemorySink`、context 传递辅助 |

## 关键类型

| 类型 | 说明 |
|------|------|
| `Message` | 归一化后的统一消息结构（含 `TraceID`/`BotID`/`Channel`/`ChatType`/`Mentioned` 等） |
| `Envelope` | 线程安全消息信封，含 KV 存储 + Action 累积 + Abort 控制 |
| `Action` / `ActionType` | 输出动作描述（7 种类型） |
| `Stage` / `StageFunc` | Pipeline 处理单元接口 + 函数适配器 |
| `StageInfo` | Stage 注册元数据（Order 排序 + Enabled 开关） |
| `Predicate` / `PredicateFunc` | Envelope 条件匹配器 + 函数适配器 |
| `Attachment` | 多模态附件（image/audio/video/file） |
| `Warning` | 中间件注入的运行时警告（`WarningLevelSoft` / `WarningLevelHard` 两级） |
| `Event` / `EventKind` | 活动轨迹事件（`stage/start`、`tool/call`、`llm/request`、`hitl/deferred` 等） |
| `EventSink` / `MemorySink` | 事件接收接口与有界内存实现（`NoopSink` 为默认零侵入实现） |

### Envelope 方法

`Set` / `Get` / `MustGet`（KV 存储）、`AddAction` / `Actions`（Action 累积，`Actions()` 返回深拷贝）、
`Abort` / `Aborted`（中止控制）、`Err` / `SetErr`（错误状态）。

### ChatType 常量

`ChatPrivate` / `ChatGroup` / `ChatSupergroup` / `ChatChannel`，空字符串表示未知类型。

## 使用示例

```go
env := core.NewEnvelope(core.Message{
    ID:     "msg-1",
    Source: "webhook",
    Text:   "Hello",
})

env.Set("user.profile", profile)
env.AddAction(core.Action{
    Type:    core.ActionReply,
    Channel: "general",
    Payload: "Hi!",
})
```

## 警告系统

中间件可在 Pipeline 执行期间向 Envelope 注入警告，下游 Stage（如 LLMRoute）可消费这些警告并将其合并到 System Prompt 中。

```go
// 中间件注入警告
core.QueueWarning(env, core.Warning{
    Source:  "loop_detection",
    Level:   core.WarningLevelSoft, // 或 WarningLevelHard
    Message: "检测到重复工具调用模式",
})

// 检查是否存在硬警告
if core.HasHardWarning(env) {
    // 触发降级策略
}

// 合并到 prompt（消费软警告，保留硬警告）
prompt := core.MergeWarnings(env, baseSystemPrompt)

// 清空队列
core.ClearWarnings(env)
```

警告统一存放在 Envelope KV 的 `core.WarningsKey`（`"pipeline.warnings"`）下，
采用延迟注入避免破坏 `AIMessage(tool_calls) → ToolMessage` 的配对。

## 多模态附件

附件挂在 `Message.Metadata["attachments"]`（`[]Attachment`），通过辅助函数读写：

```go
core.SetAttachments(&msg, []core.Attachment{{Type: core.AttachmentTypeImage, URL: "https://..."}})

if core.HasMultimodalAttachments(&msg) {
    for _, a := range core.GetAttachments(&msg) {
        uri := a.DataURI() // 有 URL 直接返回，否则构造 base64 data URI
        _ = uri
    }
}
```

类型判定辅助：`IsMultimodalType`（image/audio/video，不含 file）、`IsImageType`、
`IsAudioType`、`IsVideoType`、`IsDataURL`、`IsHTTPURL`。

## 错误语义

| 错误 | 语义 |
|------|------|
| `AbortError` | 立即中止 Pipeline，错误传播给调用方；`IsAbortError()` 判定 |
| `SkipError` | 跳过当前 Stage，继续下一个；`IsSkipError()` 判定 |
| `PipelineError` | 携带 Stage 名的包装错误，支持 `Unwrap()` |
