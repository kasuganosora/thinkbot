# core — 核心类型定义

Pipeline 框架的零业务依赖核心包，定义消息处理流水线中的所有基础类型。

## 功能

- 定义统一消息类型 `Message`（跨渠道归一化）
- 定义线程安全的消息信封 `Envelope`（Stage 间状态传递）
- 定义输出动作 `Action` 和 7 种 `ActionType`（reply/forward/broadcast/note/callback/silent/drop）
- 定义 `Stage` 接口及 `StageInfo` 元数据
- 定义 Pipeline 控制流错误类型（`PipelineError`/`AbortError`/`SkipError`）
- 定义多模态附件类型 `Attachment`
- **警告系统**（`warning.go`）：允许中间件向 Envelope 注入软/硬警告，Stage 可消费并合并到 System Prompt

## 关键类型

| 类型 | 说明 |
|------|------|
| `Message` | 归一化后的统一消息结构 |
| `Envelope` | 线程安全消息信封，含 KV 存储 + Action 累积 + Abort 控制 |
| `Action` / `ActionType` | 输出动作描述（7 种类型） |
| `Stage` / `StageFunc` | Pipeline 处理单元接口 + 函数适配器 |
| `StageInfo` | Stage 注册元数据（Order 排序 + Enabled 开关） |
| `Attachment` | 多模态附件（image/audio/video/file） |
| `Warning` / `WarningLevel` | 中间件注入的运行时警告（soft/hard 两级） |

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
```
