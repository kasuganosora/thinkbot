# openai — OpenAI Provider 实现

OpenAI API 的 `llm.Provider` 实现，兼容所有遵循 OpenAI 协议的第三方供应商（DeepSeek、Moonshot、SiliconFlow、Ollama 等）。

## 功能

- 实现 `llm.Provider` 接口（`DoGenerate` / `DoStream`）
- 双 API 模式：
  - **Responses API**（默认，`/v1/responses`）
  - **Chat Completions API**（通过 `WithChatMode()` 启用，`/v1/chat/completions`）
- 兼容第三方供应商：通过 `WithBaseURL` 一键切换（默认的 `DefaultBaseURL = "https://api.openai.com"`）
- 内置重试看门狗（retry watchdog）与流式 SSE 解析；流式请求支持首字节前的 429/5xx 重试
- 提供模型查询、音频（TTS 语音合成 / 翻译）等直接 API 方法
- 错误统一包装为 `llm.LLMError`（按 `ErrorReason` 归类，识别 `Retry-After`）

> 注意：本包的方法签名与 `llm.ModelLister` / `llm.SpeechProvider` / `llm.TranscriptionProvider` 等可选接口**并不一致**（例如 `ListModels` 返回 `*ListModelsResponse` 而非 `[]llm.Model`），因此本 Provider 仅实现 `llm.Provider`，不直接满足这些能力接口。语音仅支持 TTS 合成与翻译，**不提供** STT 转写（`DoTranscribe`）。

## 关键类型

| 类型 | 说明 |
|------|------|
| `Client` | OpenAI API 客户端（实现 `llm.Provider`） |
| `Option` | 构造期配置选项（`openai.New(...)` 使用） |
| `RequestOption` | 底层 Responses API 请求修饰选项（`CreateResponse` 等方法使用） |

## 使用示例

```go
prov := openai.New(
    openai.WithAPIKey("sk-xxx"),
    openai.WithBaseURL("https://api.deepseek.com"), // 切换第三方供应商
    openai.WithChatMode(), // 使用 Chat Completions 端点（仅兼容 Chat 的供应商需要）
)

result, err := prov.DoGenerate(ctx, llm.GenerateParams{
    Model:    llm.ChatModel("deepseek-chat"),
    Messages: []llm.Message{llm.UserMessage("你好")},
})
```

## 构造配置选项（Option）

| 选项 | 说明 |
|------|------|
| `WithAPIKey(key)` | 设置 API Key（Bearer 头） |
| `WithBaseURL(url)` | 自定义基础 URL（兼容第三方供应商） |
| `WithOrganization(org)` | 设置 `OpenAI-Organization` 头 |
| `WithProject(project)` | 设置 `OpenAI-Project` 头 |
| `WithTimeout(d)` | HTTP 超时 |
| `WithMaxBodySize(n)` | 响应体大小上限（字节），`-1` 为无限制 |
| `WithRetry(cfg)` | 重试配置（`retry.Config`） |
| `WithHTTPClient(hc)` | 自定义底层 `*http.Client` |
| `WithSharedClient(c)` | 复用已有 `*httputil.Client`（共享连接池/代理，各 Provider 独立 baseURL 与认证头） |
| `WithDump()` | 开启 dump 日志 |
| `WithChatMode()` | 启用 Chat Completions 模式（替代默认 Responses API） |
| `WithChatPath(path)` | 自定义 Chat Completions 端点路径（默认 `/v1/chat/completions`） |

## 两种 API 模式

- **Responses API（默认）**：`DoGenerate` / `DoStream` 经 `paramsToOpenAIRequest` 转换后调用 `/v1/responses`，支持 `ReasoningConfig`、结构化输出（json_schema）、`PreviousResponseID` 多轮等能力。
- **Chat Completions API**：当 `WithChatMode()` 生效时，`DoGenerate` / `DoStream` 改用 `/v1/chat/completions`，适用于仅实现 Chat Completions 的供应商（如智谱 BigModel、DeepSeek、Moonshot）。支持工具调用、JSON 模式；响应侧解析 `reasoning_content`（思考文本），请求侧历史中的思考块不回传。

两种模式下都会自动处理隐式前缀缓存：若 `GenerateParams.CacheKey` 非空，会透传为 `prompt_cache_key` 并设 `store=false`。

### Chat 模式消息规范化（GLM/BigModel 兼容）

Chat 请求构造时自动规范化消息结构，规避 GLM 整请求拒收（1210「API 调用参数有误」/ 1214「messages 参数非法」）：

- content 永不为空字符串：空/空白文本回退为占位符；工具结果为空时补「（工具无输出）」。
- 工具结果（tool 消息）的 content 序列化为 JSON 字符串，单层转义不双重编码。
- 消息严格交替：合并连续同 role 的纯文本消息；tool 消息与带 `tool_calls` 的 assistant 回合不合并，保持 `tool_call_id` 配对。
- 首条消息 role 必须为 user/system：历史以 assistant/tool 开头且无 system 时，前置一条占位 user 消息。
- 至少含一条 user 消息：system-only 对话在 system 之后补占位 user 消息。
- 工具 schema 规范化：无参工具输出 `{"type":"object","properties":{}}`，并递归为每个 object 节点补空 `properties`。

## 直接 API 方法（底层，非 llm 接口）

除 `llm.Provider` 外，本包还暴露以下便于直接调用的客户端方法：

**Responses API**
- `CreateResponse(ctx, model, input, opts...)` / `DoCreateResponse(ctx, req)` —— 同步请求
- `StreamResponse(ctx, model, input, onEvent, opts...)` / `StreamResponseWithConfig(...)` / `StreamResponseChannel(...)` —— 流式（回调 / channel）
- `StreamText(ctx, model, input, onDelta, opts...)` / `StreamTextChannel(...)` —— 仅文本增量
- `RetrieveResponse` / `DeleteResponse` / `CancelResponse`
- `StreamAccumulator` —— 将流式事件累积为完整 `Response`

**Chat Completions API**
- `DoChatCompletion(ctx, req)` / `DoStreamChatCompletion(ctx, req, cfg, onChunk)`

**模型**
- `ListModels(ctx)` / `RetrieveModel(ctx, id)` / `DeleteModel(ctx, id)`，`ListModelsResponse.FindModel(id)`

**音频**
- TTS：`Speech(ctx, model, voice, input, opts...)` / `DoSpeech(ctx, req)`，`SpeechOption`：`WithSpeechFormat` / `WithSpeechSpeed` / `WithSpeechInstructions` / `WithSpeechStreamFormat`
- 翻译：`Translate` / `TranslateFromBytes` / `DoTranslate`（`TranslationOption`：`WithTranslationPrompt` / `WithTranslationFormat` / `WithTranslationTemperature`）
- 创建语音：`CreateVoice(ctx, req)`

**Responses 请求修饰**（`RequestOption`）示例：`WithTemperature`、`WithTopP`、`WithMaxOutputTokens`、`WithReasoning(effort, summary)` / `WithReasoningEffort`、`WithJSONSchema`、`WithJSONText`、`WithFunctionTools`、`WithWebSearch`、`WithFileSearch`、`WithCodeInterpreter`、`WithPreviousResponse`、`WithStore`、`WithToolChoice`、`WithParallelToolCalls`、`WithInclude`、`WithInstructions`。还提供 `InputText` / `InputUserWithImage` / `InputFunctionCallOutput` 等输入构造辅助。

## 常量

- 模型：`ModelGPT5` / `ModelGPT5Mini` / `ModelGPT41` / `ModelGPT4o` / `ModelGPT4oMini` / `ModelO3` / `ModelO4Mini` 等；TTS：`ModelTTS1` / `ModelTTS1HD` / `ModelGPT4oMiniTTS`；转录/翻译：`ModelWhisper1` / `ModelGPT4oTranscribe` / `ModelGPT4oMiniTrans`
- 语音：`VoiceAlloy` / `VoiceNova` / `VoiceEcho` 等
- 音频格式：`AudioFormatMP3` / `AudioFormatOpus` / `AudioFormatAAC` / `AudioFormatFLAC` / `AudioFormatWAV` / `AudioFormatPCM`
- 推理努力：`ReasoningMinimal` / `ReasoningLow` / `ReasoningMedium` / `ReasoningHigh`

## 错误处理

所有 API 错误都会通过 `parseAPIError` 包装为 `*llm.LLMError`：根据 OpenAI 错误类型 / HTTP 状态码映射到 `ErrorReason`（如 `rate_limit_exceeded` → `ErrorReasonRateLimit`、`authentication_error` → `ErrorReasonAuthentication`、`insufficient_quota` → `ErrorReasonQuotaExceeded`），并对 429 响应解析 `Retry-After` 设置 `RetryAfter`。

---

完整实现参考 `llm/openai/adapter.go`。
