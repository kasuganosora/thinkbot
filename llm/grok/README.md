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

> 注意：本包未实现 `llm.ModelLister` 或可选能力接口（如 `SpeechProvider`/`TranscriptionProvider`，二者要求 `DoSpeech`/`DoTranscribe` 统一签名）。模型列表不可用；TTS/STT、图片/视频生成均通过下方**直接 API 方法**调用。

## 构造与选项

```go
prov := grok.New(
    grok.WithAPIKey("xai-xxx"),
    grok.WithBaseURL("https://api.x.ai"), // 默认即此
)

result, err := prov.DoGenerate(ctx, llm.GenerateParams{
    Model:    llm.ChatModel("grok-4.3"), // 或常量 grok.ModelGrok43
    Messages: []llm.Message{llm.UserMessage("你好")},
})
```

| Option | 说明 |
|--------|------|
| `WithAPIKey(key)` | 设置 API Key（默认 header `Authorization: Bearer`） |
| `WithBaseURL(url)` | 自定义基础 URL，默认 `https://api.x.ai` |
| `WithTimeout(d)` | HTTP 超时 |
| `WithMaxBodySize(n)` | 响应体大小上限（字节），`-1` 无限制，默认 10MB（超限截断并打 warn 日志） |
| `WithRetry(cfg retry.Config)` | 重试配置；未自定义 `ShouldRetry` 时底层 HTTP 客户端默认仅重试 429/502/503/504/其余 5xx 与网络错误（确定性 4xx 不重试），并读取 429 响应的 `Retry-After` 覆盖退避 |
| `WithHTTPClient(*http.Client)` | 自定义底层客户端（本包无 `WithProxy`，需代理时用共享客户端或自定义 `http.Client`） |
| `WithSharedClient(*httputil.Client)` | 共享已有客户端（复用连接池/代理，独立认证） |
| `WithDump()` | 输出请求/响应 dump 日志 |

`Client.Name()` 返回 `"grok"`。

## Provider 接口适配

通过统一 `llm.GenerateParams` 调用：

- 透传：`Temperature`、`TopP`、`MaxTokens`、`Seed`、`StopSequences`（序列化为 JSON 字符串数组）、`FrequencyPenalty`、`PresencePenalty`。`N`、`User` 无统一参数对应，不透传。
- `ResponseFormat`（仅非 nil 时映射）：`JSONObject`→`json_object`；`JSONSchema`→`json_schema`（仅当 `JSONSchema` 为 `map[string]any` 时取 `name`/`schema`，否则回退 `json_object`）；其余（含 `text`）→`text`。
- `ReasoningEffort` 原样转为 `reasoning_effort`（`ReasoningEffort` 类型）。
- `Tools` 转为 OpenAI function 工具；`ToolChoice` 经 `json.Marshal` 转为原始 JSON（string 或 object），仅在 `Tools` 非空时设置。延迟加载（参数 `nil`）降级为 `{"type":"object"}`。
- 消息映射：`params.System` 与消息中的 system 角色都转为 `system` 消息（content 为 JSON 字符串字面量）；user 含 `llm.ImagePart` 时构造 `[]ContentPart`（按 `Content` 中出现顺序，`image_url.url` 取 `ImagePart.Image`，`Detail` 不设置），否则 content 为字符串字面量；assistant 的 `TextPart` 拼接为 content、`ToolCallPart` 转为 `tool_calls`（`Input` 序列化为 `arguments` 字符串），`ReasoningPart` 被丢弃；tool 角色的 `ToolResultPart` 逐个转为 `tool` 消息（`tool_call_id` + content 为结果 JSON 再转义的字符串，`IsError` 不特殊处理）。`FilePart` 不被处理。
- 推理内容：`reasoning_content` 映射到 `result.Reasoning` / 流式 `ReasoningStart/Delta/EndPart`（reasoning 与 text 切换时自动闭合前一段；两者同 chunk 并存时先发 reasoning）。
- 非流式工具调用：`tool_calls[].function.arguments` 经 `json.Unmarshal` 解析为 `Input`（空串则保持 nil）。
- `Usage`：`PromptTokens`→`InputTokens`、`CompletionTokens`→`OutputTokens`、`TotalTokens`→`TotalTokens`。
- 流式：`StartPart`→`StartStepPart`→（`Reasoning*` / `Text*` / `ToolInput*`）→`FinishStepPart`（含 usage 与响应元数据 `ID`/`ModelID`）→`ErrorPart`（若有）→`FinishPart`；流式中断（非用户取消）以 `ErrorPart` 形式下发后仍发送 `FinishPart`。工具调用按 `tool_calls[].index` 累积合并参数，流结束后统一发送 `ToolInputEndPart`+`StreamToolCallPart`（首个 delta 帧 ID 为空时以函数名兜底，后续帧带 ID 且当前为空时补上）。
- 结束原因：`stop`→Stop；`length`→Length；`tool_calls`→ToolCalls；`content_filter`→ContentFilter；其他→Other；流式无结束原因时默认 Stop。

## Chat Completions 直接方法

| 方法 | 说明 |
|------|------|
| `CreateChatCompletion(ctx, model, []Message, ...RequestOption)` | 便捷封装，返回 `*ChatCompletionResponse` |
| `DoChatCompletion(ctx, ChatCompletionRequest)` | 发送完整请求 |
| `StreamChatCompletion(ctx, model, []Message, onChunk, ...RequestOption)` | 流式回调 |
| `StreamChatCompletionWithConfig(ctx, model, msgs, StreamConfig, onChunk, opts...)` | 支持看门狗/重试 |
| `StreamChatCompletionChannel(ctx, model, msgs, StreamConfig, opts...)` | 通过 channel 返回 `(<-chan, <-chan error)` |
| `DoStreamChatCompletion(ctx, ChatCompletionRequest, StreamConfig, onChunk)` | 发送完整流式请求（回调形式） |
| `NewStreamAccumulator()` + `OnChunk` / `Result()` | 将 chunk 累积为完整响应：choices 按首次出现顺序排序（`choices[].index` 为键），`tool_calls` 增量按 index 合并（ID/type/name 非空帧覆盖，`arguments` 拼接），`content`/`reasoning_content` 累积，`created` 取最大值，`usage` 取最后一个非空 |

`StreamConfig`：`WatchdogTimeout time.Duration`（0=禁用）、`RetryConfig *retry.Config`。流式请求强制 `stream=true` 并附加 `stream_options.include_usage=true` 以获取 usage；SSE `[DONE]` 标记被忽略。

### RequestOption 与消息构造

- `RequestOption`：`WithTemperature` / `WithMaxTokens` / `WithTopP` / `WithReasoningEffort` / `WithResponseFormat` / `WithTools` / `WithSeed` / `WithFrequencyPenalty` / `WithPresencePenalty` / `WithN`（均为直接 API 方法使用，`DoGenerate`/`DoStream` 不经过它们）。
- 响应格式辅助：`JSONSchemaResponseFormat(name, schema, strict)`、`JSONObjectResponseFormat()`。
- 消息构造：`SystemMessage(s)`、`UserMessage(s)`、`AssistantMessage(s)`、`ToolMessage(toolCallID, s)`、`UserMessageWithImage(text, url)`（图片 part 在前、文本在后）、`UserMessageWithBase64Image(text, mediaType, base64Data)`（自动拼 `data:` URI）；`Message.ContentStr()` 解析 content 字符串（content 统一为 `json.RawMessage`，纯文本时为 JSON 字符串字面量，多模态时为 `ContentPart` 数组；无法按字符串解析时原样返回原始 JSON）。

## 图片

- `GenerateImage(ctx, model, prompt, ...ImageOption)` → POST `/v1/images/generations`，返回 `*ImageResponse`。`ImageOption`：`WithImageCount(n)`、`WithImageFormat("url"|"b64_json")`(`ImageFormatURL`/`ImageFormatBase64`，不设置时由服务端默认)、`WithAspectRatio("16:9"等)`、`WithImageResolution("1k"|"2k")`。模型通常用 `ModelGrokImageQuality`（注释推荐，非代码强制）。
- `EditImage(ctx, model, prompt, imageURL, ...ImageOption)`：编辑图片（URL 或 base64 data URI；`imageURL` 为空时本地报错）。
- 底层：`DoGenerateImage(ctx, ImageRequest)`、`DoEditImage(ctx, ImageRequest)`。
- 结果提取：`ImageResponse.FirstImageURL()`、`FirstImageBase64()`。

## 视频（异步轮询）

- `GenerateVideo(ctx, model, prompt, ...VideoOption)`：POST `/v1/videos/generations` 后自动轮询 `GET /v1/videos/{request_id}`，默认超时 10 分钟、间隔 5 秒。
- `GenerateVideoWithPolling(ctx, model, prompt, timeout, interval, ...VideoOption)`：自定义超时/间隔。
- 手动流程：`StartVideoGeneration(ctx, req) (*VideoStartResponse, error)`（`model`/`prompt` 为空本地报错）、`GetVideoStatus(ctx, requestID) (*VideoStatusResponse, error)`、`PollVideo(ctx, requestID, timeout, interval) (*VideoResult, error)`（`done` 但无视频时报错；`failed`/`expired` 分别返回对应错误；首次轮询需等一个 interval）。
- `EditVideo(ctx, model, prompt, videoURL)`、`ExtendVideo(ctx, model, prompt, videoURL)`：分别 POST `/v1/videos/edits`、`/v1/videos/extensions`，返回 `*VideoStartResponse`（仍需轮询）。
- `VideoOption`：`WithVideoDuration(1-15秒)`、`WithVideoAspectRatio`、`WithVideoResolution("480p"|"720p")`(`VideoResolution480p`/`720p`)、`WithVideoImage(url)`（image-to-video）。
- 状态常量：`VideoStatusPending`/`VideoStatusDone`/`VideoStatusExpired`/`VideoStatusFailed`。

## 音频

### 文本转语音（TTS）

- `TTS(ctx, text, voiceID, language, ...TTSOption) ([]byte, contentType, error)`：POST `/v1/tts`，返回音频字节与响应 `Content-Type`（`text`/`language` 为空本地报错）。`language` 为 BCP-47 代码或 `"auto"`；未设置输出格式时 codec 默认 mp3（24kHz/128kbps，由服务端决定）。
- `DoTTS(ctx, TTSRequest)`、`ListVoices(ctx) (*ListVoicesResponse, error)`。
- `TTSOption`：`WithTTSSpeed`(0.7-1.5)、`WithTTSOutputFormat(codec, sampleRate, bitRate)`、`WithTTSOptimizeStreamingLatency(level)`、`WithTTSTextNormalization(bool)`。
- 语音常量：`VoiceEve`（代码注释标注"默认"）、`VoiceAra`、`VoiceRex`、`VoiceSal`、`VoiceLeo`；`TTS` 的 `voiceID` 未强制校验，空值原样下发。

### 语音转文本（STT）

- `SpeechToText(ctx, STTRequest, ...STTOption)`、`SpeechToTextFromBytes(ctx, filename, data, opts...)`、`SpeechToTextFromURL(ctx, url, opts...)`、`DoSpeechToText(ctx, params) (*STTResponse, error)`。
- `DoSpeechToText` 以 multipart 表单 POST `/v1/stt`：`File` 与 `URL` 均为空时本地报错；`url`/`audio_format`/`sample_rate`/`language`/`channels` 等仅非零时下发，布尔项为真时传 `"true"`，`keyterm` 每项一个字段，`file` 字段固定放在表单最后。
- `STTOption`：`WithSTTLanguage`、`WithSTTFormat`、`WithSTTMultichannel`、`WithSTTChannels`、`WithSTTDiarize`、`WithSTTKeyTerms(...)`、`WithSTTFillerWords`、`WithSTTRawFormat(format, sampleRate)`。
- 响应含 `Text`、`Language`、`Duration`、词级时间戳 `Words`（含 speaker 编号）、多通道 `Channels`。

## Files API

| 方法 | 说明 |
|------|------|
| `UploadFile(ctx, UploadFileParams{Filename, Reader})` | 上传，返回 `*FileInfo` |
| `ListFiles(ctx, *ListFilesOptions)` | 列出（`Limit`/`Order`（`asc` 或 `desc`）/`SortBy`（`created_at`/`filename`/`size`）/`PaginationToken`；零值字段不下发 query，`Limit` 默认值 100 由服务端决定） |
| `GetFile(ctx, fileID)` | 文件元数据 |
| `GetFileContent(ctx, fileID)` | 下载内容（原始字节） |
| `GetFileContentReader(ctx, fileID)` | 下载内容（`*bytes.Reader`） |
| `DeleteFile(ctx, fileID)` | 删除，返回 `*DeleteFileResponse` |

## 错误分类

底层错误经 `parseAPIError` 转为 `*llm.LLMError`，按 HTTP 状态映射：

- `429`→RateLimit；`401`/`403`→Authentication；`402`→QuotaExceeded；`>=500`→ProviderInternal；`>=400`→InvalidRequest；网络层/无状态码→Transport。HTTP ≥400 但响应体解析不出 `error.message` 时，按状态码映射并附 `HTTP <code>: <body>` 消息。
- 注意：错误分类本身不解析 `Retry-After`（`llm.LLMError.RetryAfter` 为 0）；但底层 HTTP 客户端的自动重试（`WithRetry` 配置后）默认仅重试 429/502/503/504/其余 5xx 与网络错误，并读取 429 的 `Retry-After` 覆盖退避时间。
- 请求校验：`model` 为空或 `messages` 为空时本地直接返回错误，不会发起 HTTP 请求（`DoChatCompletion`/`DoStreamChatCompletion` 均校验）。

## 关键类型与常量

- 常量：`DefaultBaseURL="https://api.x.ai"`、`RoleSystem`/`RoleUser`/`RoleAssistant`/`RoleTool`、`ContentTypeText`/`ContentTypeImageURL`、`ResponseFormatText`/`ResponseFormatJSONObject`/`ResponseFormatJSONSchema`、`ImageFormatURL`/`ImageFormatBase64`、`VideoStatus*`、`VideoResolution480p`/`VideoResolution720p`、`FinishReason*`(`stop`/`length`/`tool_calls`/`content_filter`)。
- 模型常量：
  - 文本：`ModelGrok43`、`ModelGrok420NonReasoning`、`ModelGrok420Reasoning`、`ModelGrok420MultiAgent`、`ModelGrokBuild`。
  - 图片：`ModelGrokImage`、`ModelGrokImageQuality`。
  - 视频：`ModelGrokVideo`、`ModelGrokVideo15Preview`。
- 推理：`ReasoningEffort` 类型及常量 `ReasoningNone`/`ReasoningLow`/`ReasoningMedium`/`ReasoningHigh`（`low` 的"默认"仅是注释标注，适配器不设置缺省值）。
- 消息/工具：`Message`(`Content` 为 `json.RawMessage`，含 `ReasoningContent`/`ToolCalls`/`ToolCallID`)、`ContentPart`、`ImageURL`、`ToolCall`、`FunctionCall`、`Tool`、`ToolFunction`、`ResponseFormat`/`JSONSchemaConfig`、`StreamOptions`、`ChatCompletionRequest`(含 `ReasoningEffort`/`ResponseFormat`/`Tools`/`N`/`User`)、`ChatCompletionResponse`/`Choice`/`Delta`/`Usage`。
- 图片/视频/音频：`ImageRequest`/`ImageResponse`/`ImageData`、`VideoGenerationRequest`/`VideoImage`/`VideoStartResponse`/`VideoStatusResponse`/`VideoResult`/`VideoError`/`VideoEditRequest`/`VideoExtendRequest`、`TTSRequest`/`TTSOutputFormat`/`TTSVoice`、`STTRequest`/`STTResponse`/`STTWord`/`STTChannel`。
- 文件：`FileInfo`、`ListFilesResponse`、`DeleteFileResponse`。
- 错误：`APIError`(`Type`/`Message`/`Code`/`Param`)、`ErrorResponse`。
