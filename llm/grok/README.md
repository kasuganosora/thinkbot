# grok — xAI Grok Provider 实现

xAI (Grok) API 的 `llm.Provider` 实现，兼容 OpenAI Chat Completions 协议格式。

## 功能概览

- 实现 `llm.Provider` 接口（`DoGenerate` / `DoStream`）
- 基于 OpenAI 风格 Chat Completions（`/v1/chat/completions`）
- 多模态输入：文本 + 图片（URL 或 base64 data URI）
- 推理内容（Reasoning Content）透传
- 工具调用（function calling）+ `ToolChoice`
- 图片生成 / 编辑（`/v1/images/generations`、`/v1/images/edits`）
- 视频生成 / 编辑 / 扩展（`/v1/videos/...`，异步轮询）
- 音频：文本转语音 TTS（`/v1/tts`）与语音转文本 STT（`/v1/stt`）
- Files API：上传、列表、查询、下载、删除
- 统一错误分类（转 `llm.LLMError`）

> 注意：本包未实现 `llm.ModelLister` 或可选能力接口（如 `SpeechProvider`/`TranscriptionProvider`）。模型列表、TTS/STT、图片/视频生成均通过下方**直接 API 方法**调用。

## 构造与选项

```go
prov := grok.New(
    grok.WithAPIKey("xai-xxx"),
    grok.WithBaseURL("https://api.x.ai"), // 默认即此
)

result, err := prov.DoGenerate(ctx, llm.GenerateParams{
    Model:    llm.ChatModel("grok-3"),
    Messages: []llm.Message{llm.UserMessage("你好")},
})
```

| Option | 说明 |
|--------|------|
| `WithAPIKey(key)` | 设置 API Key（默认 header `Authorization: Bearer`） |
| `WithBaseURL(url)` | 自定义基础 URL，默认 `https://api.x.ai` |
| `WithTimeout(d)` | HTTP 超时 |
| `WithMaxBodySize(n)` | 响应体大小上限（字节），`-1` 无限制 |
| `WithRetry(cfg retry.Config)` | 重试配置 |
| `WithHTTPClient(*http.Client)` | 自定义底层客户端 |
| `WithSharedClient(*httputil.Client)` | 共享已有客户端（复用连接池/代理，独立认证） |
| `WithDump()` | 输出请求/响应 dump 日志 |

`Client.Name()` 返回 `"grok"`。

## Provider 接口适配

通过统一 `llm.GenerateParams` 调用：

- 透传：`Temperature`、`TopP`、`MaxTokens`、`Seed`、`StopSequences`、`FrequencyPenalty`、`PresencePenalty`。
- `ResponseFormat`：`JSONObject`→`json_object`；`JSONSchema`→`json_schema`（取 `name`/`schema`）；其余→`text`。
- `ReasoningEffort` 原样映射为 `reasoning_effort`（`ReasoningEffort` 类型）。
- `Tools` 转为 OpenAI function 工具；`ToolChoice` 转为原始 JSON（string 或 object）。延迟加载（参数 `nil`）降级为 `{"type":"object"}`。
- 多模态：用户消息含 `llm.ImagePart` 时，自动构造 `[]ContentPart`（图片 + 文本）；否则为纯文本字符串。
- 推理内容：`reasoning_content` 映射到 `result.Reasoning` / 流式 `ReasoningPart`。
- `Usage`：`PromptTokens`/`CompletionTokens`/`TotalTokens`。
- 结束原因：`stop`→Stop；`length`→Length；`tool_calls`→ToolCalls；`content_filter`→ContentFilter；其他→Other。

## Chat Completions 直接方法

| 方法 | 说明 |
|------|------|
| `CreateChatCompletion(ctx, model, []Message, ...RequestOption)` | 便捷封装，返回 `*ChatCompletionResponse` |
| `DoChatCompletion(ctx, ChatCompletionRequest)` | 发送完整请求 |
| `StreamChatCompletion(ctx, model, []Message, onChunk, ...RequestOption)` | 流式回调 |
| `StreamChatCompletionWithConfig(ctx, model, msgs, StreamConfig, onChunk, opts...)` | 支持看门狗/重试 |
| `StreamChatCompletionChannel(ctx, model, msgs, StreamConfig, opts...)` | 通过 channel 返回 `(<-chan, <-chan error)` |
| `NewStreamAccumulator()` + `OnChunk` / `Result()` | 将 chunk 累积为完整响应 |

`StreamConfig`：`WatchdogTimeout time.Duration`、`RetryConfig *retry.Config`。

### RequestOption 与消息构造

- `RequestOption`：`WithTemperature` / `WithMaxTokens` / `WithTopP` / `WithReasoningEffort` / `WithResponseFormat` / `WithTools` / `WithSeed` / `WithFrequencyPenalty` / `WithPresencePenalty` / `WithN`。
- 响应格式辅助：`JSONSchemaResponseFormat(name, schema, strict)`、`JSONObjectResponseFormat()`。
- 消息构造：`SystemMessage(s)`、`UserMessage(s)`、`AssistantMessage(s)`、`ToolMessage(toolCallID, s)`、`UserMessageWithImage(text, url)`、`UserMessageWithBase64Image(text, mediaType, base64Data)`；`Message.ContentStr()` 解析 content 字符串。

## 图片

- `GenerateImage(ctx, model, prompt, ...ImageOption)` → `*ImageResponse`。`ImageOption`：`WithImageCount(n)`、`WithImageFormat("url"|"b64_json")`(`ImageFormatURL`/`ImageFormatBase64`)、`WithAspectRatio("16:9"等)`、`WithImageResolution("1k"|"2k")`。
- `EditImage(ctx, model, prompt, imageURL, ...ImageOption)`：编辑图片（URL 或 base64 data URI）。
- 底层：`DoGenerateImage(ctx, ImageRequest)`、`DoEditImage(ctx, ImageRequest)`。
- 结果提取：`ImageResponse.FirstImageURL()`、`FirstImageBase64()`。

## 视频（异步轮询）

- `GenerateVideo(ctx, model, prompt, ...VideoOption)`：自动轮询，默认超时 10 分钟、间隔 5 秒。
- `GenerateVideoWithPolling(ctx, model, prompt, timeout, interval, ...VideoOption)`：自定义超时/间隔。
- 手动流程：`StartVideoGeneration(ctx, req) (*VideoStartResponse, error)`、`GetVideoStatus(ctx, requestID) (*VideoStatusResponse, error)`、`PollVideo(ctx, requestID, timeout, interval) (*VideoResult, error)`。
- `EditVideo(ctx, model, prompt, videoURL)`、`ExtendVideo(ctx, model, prompt, videoURL)`：编辑/扩展，返回 `*VideoStartResponse`（仍需轮询）。
- `VideoOption`：`WithVideoDuration(1-15秒)`、`WithVideoAspectRatio`、`WithVideoResolution("480p"|"720p")`(`VideoResolution480p`/`720p`)、`WithVideoImage(url)`（image-to-video）。
- 状态常量：`VideoStatusPending`/`VideoStatusDone`/`VideoStatusExpired`/`VideoStatusFailed`。

## 音频

### 文本转语音（TTS）

- `TTS(ctx, text, voiceID, language, ...TTSOption) ([]byte, contentType, error)`：返回音频字节。
- `DoTTS(ctx, TTSRequest)`、`ListVoices(ctx) (*ListVoicesResponse, error)`。
- `TTSOption`：`WithTTSSpeed`(0.7-1.5)、`WithTTSOutputFormat(codec, sampleRate, bitRate)`、`WithTTSOptimizeStreamingLatency(level)`、`WithTTSTextNormalization(bool)`。
- 语音常量：`VoiceEve`（默认）、`VoiceAra`、`VoiceRex`、`VoiceSal`、`VoiceLeo`。

### 语音转文本（STT）

- `SpeechToText(ctx, STTRequest, ...STTOption)`、`SpeechToTextFromBytes(ctx, filename, data, opts...)`、`SpeechToTextFromURL(ctx, url, opts...)`、`DoSpeechToText(ctx, params) (*STTResponse, error)`。
- `STTOption`：`WithSTTLanguage`、`WithSTTFormat`、`WithSTTMultichannel`、`WithSTTChannels`、`WithSTTDiarize`、`WithSTTKeyTerms(...)`、`WithSTTFillerWords`、`WithSTTRawFormat(format, sampleRate)`。
- 响应含 `Text`、词级时间戳 `Words`、多通道 `Channels`。

## Files API

| 方法 | 说明 |
|------|------|
| `UploadFile(ctx, UploadFileParams{Filename, Reader})` | 上传，返回 `*FileInfo` |
| `ListFiles(ctx, *ListFilesOptions)` | 列出（`Limit`/`Order`/`SortBy`/`PaginationToken`） |
| `GetFile(ctx, fileID)` | 文件元数据 |
| `GetFileContent(ctx, fileID)` | 下载内容（原始字节） |
| `GetFileContentReader(ctx, fileID)` | 下载内容（`*bytes.Reader`） |
| `DeleteFile(ctx, fileID)` | 删除，返回 `*DeleteFileResponse` |

## 错误分类

底层错误经 `parseAPIError` 转为 `*llm.LLMError`，按 HTTP 状态映射：

- `429`→RateLimit；`401`/`403`→Authentication；`402`→QuotaExceeded；`>=500`→ProviderInternal；`>=400`→InvalidRequest；网络层→Transport。
- 注意：当前不解析 `Retry-After` 头。

## 关键类型与常量

- 常量：`DefaultBaseURL="https://api.x.ai"`、`RoleSystem`/`RoleUser`/`RoleAssistant`/`RoleTool`、`ContentTypeText`/`ContentTypeImageURL`、`ResponseFormatText`/`ResponseFormatJSONObject`/`ResponseFormatJSONSchema`、`ImageFormatURL`/`ImageFormatBase64`、`VideoStatus*`、`VideoResolution480p`/`VideoResolution720p`、`FinishReason*`(`stop`/`length`/`tool_calls`/`content_filter`)。
- 模型常量：
  - 文本：`ModelGrok43`、`ModelGrok420NonReasoning`、`ModelGrok420Reasoning`、`ModelGrok420MultiAgent`、`ModelGrokBuild`。
  - 图片：`ModelGrokImage`、`ModelGrokImageQuality`。
  - 视频：`ModelGrokVideo`、`ModelGrokVideo15Preview`。
- 推理：`ReasoningEffort`：`ReasoningNone`/`ReasoningLow`(默认)/`ReasoningMedium`/`ReasoningHigh`。
- 消息/工具：`Message`(含 `ReasoningContent`/`ToolCalls`)、`ContentPart`、`ImageURL`、`ToolCall`、`FunctionCall`、`Tool`、`ToolFunction`、`ResponseFormat`/`JSONSchemaConfig`、`ChatCompletionRequest`(含 `ReasoningEffort`/`ResponseFormat`/`Tools`)、`ChatCompletionResponse`/`Choice`/`Delta`/`Usage`。
- 图片/视频/音频：`ImageRequest`/`ImageResponse`/`ImageData`、`VideoGenerationRequest`/`VideoStartResponse`/`VideoStatusResponse`/`VideoResult`/`VideoError`、`TTSRequest`/`TTSOutputFormat`/`TTSVoice`、`STTRequest`/`STTResponse`/`STTWord`/`STTChannel`。
- 文件：`FileInfo`、`ListFilesResponse`、`DeleteFileResponse`。
- 错误：`APIError`(`Type`/`Message`/`Code`/`Param`)、`ErrorResponse`。
