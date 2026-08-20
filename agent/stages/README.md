# stages — 内建 Pipeline Stage

提供开箱即用的 Pipeline `core.Stage` 实现，覆盖消息处理的核心环节。所有 Stage 均实现 `core.Stage` 接口（`Name() string` + `Process(ctx, env) (*core.Envelope, error)`），可直接包进 `core.StageInfo` 交给 `pipeline.New` 或 fx 的 `pipeline.ProvideStage` 注册。

## 功能

- **LLMStage**：调用 LLM Provider 编排生成（支持多步工具调用、流式输出、UsageRecorder、延迟警告合并、防偷懒门禁、上下文压缩、工具审批/ToolDeferral 注入、工具输出落盘、潜水观察与回复控制门控）。生成结果写入 `ActionReply` 并存入 Envelope KV `llm.result`。
- **ReplyStage**：在 LLMStage 基础上增加「输出决策」机制（`OutputDecision`），按决策产出 `ActionReply` / `ActionNote` / `ActionCallback` / `ActionSilent` 组合，并自动把回复捕获为 L0 工作记忆笔记。
- **EnricherStage**：消息预处理，通过 `EnrichFunc` 向 Envelope 注入用户画像、会话上下文或权限标记等元数据。
- **MultimodalStage**：多模态附件转写。当主力模型不支持多模态且配置了辅助 Vision 模型时，将 image/audio/video 转写为文本并追加到 `Message.Text`。
- **FilterStage**：基于 `core.Predicate` 的消息过滤（命中放行或命中丢弃）。
- **LoggerStage**：结构化日志记录每条消息的关键信息（用于审计/调试）。
- **NoteCaptureMiddleware**：中间件，在 LLM 产出回复后自动把**用户入站消息**捕获为 L0 笔记（`ActionNote`，speaker=user），并可选经 `UserMessageEventWriter` 写入用户消息事件流（供 dreaming 回灌），补齐记忆捕获路径。

## 关键类型

| 类型 | 说明 |
|------|------|
| `LLMStage` / `LLMConfig` | LLM 编排 Stage 及其配置（见下方字段表） |
| `StreamPublisher` | 流式增量发布接口（文本/工具调用/进度/结果） |
| `ToolResolver` | 按 Envelope 动态解析可用工具列表的接口 |
| `ReplyStage` / `ReplyStageConfig` | 带输出决策的回复 Stage 及其配置 |
| `OutputDecision` | 输出决策枚举（`DecisionReply`/`DecisionReplyWithNote`/`DecisionNoteOnly`/`DecisionCallback`/`DecisionSilent`/`DecisionDrop`） |
| `ReplyDecider` | 输出决策函数类型 |
| `PrefixDecider` | 基于文本前缀的默认决策函数 |
| `SystemPromptWithDecision` | 为 system prompt 追加决策指令前缀的辅助函数 |
| `EnricherStage` / `EnrichFunc` | 富化 Stage 及其富化函数类型 |
| `MultimodalStage` / `MultimodalConfig` | 多模态转写 Stage 及其配置 |
| `DefaultMultimodalPrompt` | 辅助模型默认系统提示词常量 |
| `FilterStage` / `FilterAction` | 过滤 Stage 及动作（`FilterPass`/`FilterDrop`） |
| `LoggerStage` | 日志 Stage（含 `LogPayload bool` 字段） |
| `NoteCaptureMiddleware` | 用户消息笔记捕获中间件 |
| `RecallStage` | 记忆召回 Stage（bot/channel/user 三 scope，渲染为 `MEMORY` 块注入 `KVMemoryRecall`） |
| `RhythmStage` / `RhythmPolicy` / `RhythmPolicyProvider` | 聊天节奏控制 Stage 及其策略（命中抑制时设 `KVSuppressReply`） |
| `DeferredApproval` / `DeferredApprovalStore` | HITL 审批锚点记录与持久化接口（`LLMStage.ResumeDeferredApproval` 续跑） |

### LLMConfig 主要字段

`SystemPrompt`(string)、`Model`(*llm.Model)、`Temperature`(*float64)、`MaxTokens`(*int)、
`MaxSteps`(int，软预算步数，0=单次，>0=多步，-1=无限)、`HardMaxSteps`(int，硬上限)、
`ReasoningEffort`(string，"minimal"/"low"/"medium"/"high")、`Tools`([]llm.Tool)、
`ToolResolver`(ToolResolver，动态工具解析，覆盖 `Tools`)、`MessageBuilder`(func(core.Message) []llm.Message)、
`UsageRecorder`(llm.UsageRecorder)、`StreamPublisher`(StreamPublisher，非 nil 走流式)、
`ReductionConfig`(*llm.ReductionConfig，两阶段上下文压缩)、
`ApprovalHandler`(func，HITL 工具审批门禁)、`ToolDeferral`(*llm.DeferralStore，延迟加载工具)、
`ToolOutputSink`(llm.ToolOutputOffloadSink，工具输出落盘)、`ToolOutput`(llm.ToolOutputConfig，输出截断阈值)、
`DeferredApprovalStore`(DeferredApprovalStore，HITL 审批锚点存储)、`ResumeDispatch`(func，HITL 续跑重跑入口)、
`RequireReplyControl`(bool，回复控制门控，开启后要求模型输出 `@@REPLY_CONTROL@@` 控制 JSON，fail-closed)。

## 源文件

| 文件 | 职责 |
|------|------|
| `llmroute.go` | `LLMStage`、`LLMConfig`、`StreamPublisher`、`ToolResolver`、工具解析与用量记录 |
| `reply_stage.go` | `ReplyStage`、`ReplyStageConfig`、`OutputDecision`、`ReplyDecider`、`PrefixDecider`、`SystemPromptWithDecision` |
| `enricher.go` | `EnricherStage`、`EnrichFunc` |
| `multimodal.go` | `MultimodalStage`、`MultimodalConfig`、`DefaultMultimodalPrompt` |
| `filter.go` | `FilterStage`、`FilterAction` |
| `logger.go` | `LoggerStage` |
| `note_capture.go` | `NoteCaptureMiddleware`、`UserMessageEventWriter` |
| `hitl.go` | `DeferredApproval`、`DeferredApprovalStore`、`NewDeferredApprovalStore`、`BuildDeferredApproval` |
| `recall.go` | `RecallStage`（长期记忆召回注入 system prompt） |
| `rhythm.go` | `RhythmStage`、`RhythmPolicy`、`RhythmPolicyProvider` |
| `lurk_contract.go` | 潜水（只读）观察模式的输出契约、解析与重试 |

## 各 Stage 说明

### LLMStage

```go
stage := stages.NewLLMStage("llm", provider, stages.LLMConfig{
    SystemPrompt: "...",
    Model:        model,
    Temperature:  &temp,
}, tp, logger)
```

`Process` 行为：
- 优先从 Envelope KV `system.prompt`（由 PromptStage 注入）读取动态提示词，回退到 `LLMConfig.SystemPrompt`；并通过 `core.MergeWarnings` 把延迟注入的 pipeline 警告（token 预算/循环检测等）合并到提示词末尾。
- 工具列表优先用 `ToolResolver.ResolveForEnvelope`，否则用静态 `Tools`。
- 若 Envelope 带 `verify.required` 且有可用工具，首步强制 `tool_choice=required`（防偷懒门禁）。
- `StreamPublisher` 非 nil 时走 `llm.OrchestrateStream`，否则 `llm.OrchestrateGenerate`。
- 完成后写入 `ActionReply`（outbound 目标取 `Metadata["reply_target"]`，回退 `Message.Channel`），并把 `*llm.GenerateResult` 存入 KV `llm.result`。

### ReplyStage

```go
stage := stages.NewReplyStage("reply", provider, stages.ReplyStageConfig{
    LLM:     stages.LLMConfig{SystemPrompt: "..."},
    Decider: stages.PrefixDecider, // 或自定义 ReplyDecider
}, tp, logger)
```

`ReplyDecider` 根据 `*llm.GenerateResult` 返回 `(decision, replyText, noteText, noteCategory)`，驱动不同 Action 组合。内置 `PrefixDecider` 依赖 LLM 用 `[REPLY]`/`[NOTE]`/`[REPLY+NOTE]`/`[SKIP]` 前缀标记，`SystemPromptWithDecision` 可把这些指令追加到提示词。纯回复（无显式备注）时会自动追加一条 `ActionNote`（category=`exchange`）喂给梦境巩固管线。

### EnricherStage / MultimodalStage / FilterStage / LoggerStage

```go
stages.NewEnricherStage("enricher", myEnrichFn, logger)
stages.NewMultimodalStage("multimodal", stages.MultimodalConfig{
    VisionProvider: vision, VisionModel: visionModel, MainMultimodal: false,
}, tp, logger)
stages.NewFilterStage("filter", predicate, stages.FilterDrop, logger)
stages.NewLoggerStage("logger", logger, true)
```

`MultimodalStage.ShouldProcess(msg)` 判定是否需要转写（有附件 && 主模型不支持多模态 && 有辅助模型）。`FilterStage` 的 `FilterPass`=命中放行/未中丢弃，`FilterDrop`=命中丢弃/未中放行，丢弃时返回 `nil` Envelope。`LoggerStage.LogPayload` 控制是否记录消息文本（生产环境建议关闭以策安全）。

## 使用方式

通过 `core.StageInfo` 构造 Pipeline（也可在 fx 中用 `pipeline.ProvideStage`/`ProvideStageInfo`）：

```go
p := pipeline.New([]core.StageInfo{
    {Stage: stages.NewLoggerStage("logger", logger, true), Order: 10, Enabled: true},
    {Stage: stages.NewMultimodalStage("multimodal", mmCfg, tp, logger), Order: 30, Enabled: true},
    {Stage: stages.NewLLMStage("llm", provider, llmCfg, tp, logger), Order: 100, Enabled: true},
    {Stage: stages.NewReplyStage("reply", provider, replyCfg, tp, logger), Order: 200, Enabled: true},
}, tp, mp, logger)
```

`NoteCaptureMiddleware(category, writer)` 作为 `core.Stage` 包装器叠加在回复路径上（writer 传 nil 表示不写事件流）：

```go
wrapped := stages.NoteCaptureMiddleware("exchange", umeWriter)(replyStage)
```

## Envelope KV 约定

- `system.prompt` (string)：PromptStage 注入的动态提示词，LLMStage/ReplyStage 优先使用。
- `llm.result` (*llm.GenerateResult)：LLM 生成结果。
- `verify.required` (bool)：VerificationGateMiddleware 标记的防偷懒强制门禁信号。
- `bot.id` (string)：`recordUsage` 提取的用量归属。
