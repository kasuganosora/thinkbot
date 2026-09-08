# google — Google Gemini Provider 实现

Google Gemini API 的 `llm.Provider` 实现。

## 功能概览

- 实现 `llm.Provider` 接口（`DoGenerate` / `DoStream`）
- `generateContent` 与 `streamGenerateContent`（SSE，`alt=sse`）端点
- 函数调用（Function Calling），含思维签名（Thought Signature）保留
- 多模态输入：文本、图片（base64 / File URI）、音频、视频、YouTube
- 图片生成与编辑（`GenerateImage` / `EditImage`）
- Files API（大文件可恢复上传、列表、查询、删除）
- 精确 Token 计数（`CountTokens`）
- 隐式前缀缓存（由 Gemini 自动管理，`GenerateParams.CachePolicy`/`CacheKey` 对本包无效）
- 统一错误分类（转 `llm.LLMError`）

> 注意：本包提供直接的 `ListModels` / `GetModel` 方法（带分页/过滤选项、返回本包 `Model` 类型），与统一接口 `llm.ModelLister` 要求的 `ListModels(ctx) ([]llm.Model, error)` 签名不符，故**未实现**该接口。

## 构造与选项

```go
prov := google.New(
    google.WithAPIKey("AIza-xxx"),
    google.WithBaseURL("https://generativelanguage.googleapis.com"), // 默认即此
)

result, err := prov.DoGenerate(ctx, llm.GenerateParams{
    Model:    llm.ChatModel("gemini-2.0-flash"),
    Messages: []llm.Message{llm.UserMessage("你好")},
})
```

| Option | 说明 |
|--------|------|
| `WithAPIKey(key)` | 设置 API Key（默认 header `x-goog-api-key`） |
| `WithBaseURL(url)` | 自定义基础 URL，默认 `https://generativelanguage.googleapis.com` |
| `WithTimeout(d)` | HTTP 超时 |
| `WithMaxBodySize(n)` | 响应体大小上限（字节），`-1` 无限制，默认 10MB（超限截断并打 warn 日志） |
| `WithRetry(cfg retry.Config)` | 重试配置；未自定义 `ShouldRetry` 时底层 HTTP 客户端默认仅重试 429/502/503/504/其余 5xx 与网络错误（确定性 4xx 不重试），并读取 429 响应的 `Retry-After` 覆盖退避 |
| `WithHTTPClient(*http.Client)` | 自定义底层客户端 |
| `WithSharedClient(*httputil.Client)` | 共享已有客户端（复用连接池/代理，独立认证） |
| `WithProxy(proxyURL)` | 设置 HTTP/HTTPS/SOCKS5 代理 |
| `WithDump()` | 输出请求/响应 dump 日志 |

`Client.Name()` 返回 `"google"`。

## Provider 接口适配

通过统一 `llm.GenerateParams` 调用：

- `params.System` 映射到 `systemInstruction`；`Messages` 中的 system 角色消息被跳过，不产生 `Content`。
- 消息映射：user → `role:"user"`（`TextPart`→文本，`ImagePart`/`FilePart`→`inlineData`）；assistant → `role:"model"`（`TextPart`→文本、`ReasoningPart`→`thought:true` 摘要、`ToolCallPart`→`functionCall`）；tool → `role:"user"` 的 `functionResponse`（结果经 `toStringMap` 归一，`IsError` 时响应体附加 `"error": true`）。无有效 part 的消息被丢弃。
- 生成配置透传：`Temperature`、`TopP`、`MaxTokens`→`MaxOutputTokens`、`StopSequences`、`Seed`、`FrequencyPenalty`、`PresencePenalty`；全部未设置时省略整个 `generationConfig`。
- `ResponseFormat`（仅非 nil 时映射）：`JSONObject`→`responseMIMEType="application/json"`；`JSONSchema`→同设 MIME，且仅当 `JSONSchema` 为 `map[string]any` 且含 `schema` 键时写入 `responseSchema`（其余情况仅设 MIME）。
- `ReasoningEffort` → `ThinkingConfig`（`effortToThinkingConfig`）：`minimal`/`none`/`low`→`ThinkingLevel="low"`，`medium`→`"medium"`，`high`→`"high"`，其他值→`IncludeThoughts:true`（均不写 `ThinkingBudget`）。
- `Tools` 转为单个 `Tool{functionDeclarations}`；`ToolChoice`：`auto`→`AUTO`、`none`→`NONE`、`required`→`ANY`，`map[string]any{function:{name}}`→`ANY`+`AllowedFunctionNames`；`toolConfig` 仅在 `Tools` 非空且设置了 `ToolChoice` 时携带。
- 工具延迟加载（参数 `nil`）降级为 `{"type":"object"}`。
- 函数调用 ID：响应 `functionCall.id` 为空时以函数名兜底作为 `ToolCallID`（流式与非流式一致）。
- `Usage` 映射：`PromptTokenCount`/`CandidatesTokenCount`/`TotalTokenCount`，`ThoughtsTokenCount`→`ReasoningTokens` 与 `OutputTokenDetails.TextTokens`（= `max(0, CandidatesTokenCount - ThoughtsTokenCount)`）；缓存 token 由 `CacheTokensDetails` 累加为 `CachedInputTokens` + `InputTokenDetails.CacheReadTokens`（并推导 `NoCacheTokens`）。
- 结束原因：`STOP`→Stop；`MAX_TOKENS`→Length；`SAFETY`/`RECITATION`/`BLOCKLIST`/`PROHIBITED`/`SPII`→ContentFilter；其他（含 `MALFORMED_FUNCTION_CALL`、`OTHER`）→Other。响应含函数调用且原因为 `STOP` 时改判为 ToolCalls（仅非流式 `DoGenerate`；流式按原始 finishReason 输出）。
- 流式：`GroundingMetadata.GroundingChunks[].Web` 转为 `llm.StreamSourcePart`（`SourceType="url"`，带 `URL`/`Title`）。
- 流式时序：`StartPart`→`StartStepPart`→（`Reasoning*` / `Text*` / `ToolInput*`+`StreamToolCallPart` / `StreamSourcePart`）→`FinishStepPart`（含 usage 与 `Response.ModelID=modelVersion`）→`ErrorPart`（流失败且非 `context.Canceled` 时）→`FinishPart`；无 finishReason 时默认 Stop。

## 直接 API 方法

| 方法 | 说明 |
|------|------|
| `GenerateContent(ctx, model, req)` | 同步请求，返回 `*GenerateContentResponse` |
| `StreamGenerateContent(ctx, model, req, onChunk)` | 流式 SSE，回调 `func(GenerateContentResponse) error` |
| `StreamGenerateContentWithConfig(ctx, model, req, StreamConfig, onChunk)` | 同上，支持看门狗与重试 |
| `StreamGenerateContentChannel(ctx, model, req, StreamConfig)` | 流式，返回 `(<-chan GenerateContentResponse, <-chan error)` |
| `CountTokens(ctx, model, req)` | Token 计数（`model` 为空或 `contents` 为空时本地报错） |
| `ListModels(ctx, *ListModelsOptions)` | 列出模型 |
| `GetModel(ctx, modelID)` | 获取单模型信息（自动补 `models/` 前缀） |
| `UploadFile(ctx, reader, size, UploadFileOptions)` | 可恢复上传大文件 |
| `ListFiles(ctx, *ListFilesOptions)` | 列出文件 |
| `GetFile(ctx, name)` | 文件元数据（`files/xxx`） |
| `DeleteFile(ctx, name)` | 删除文件 |
| `GenerateImage(ctx, model, prompt, *ImageGenerationOptions)` | 文生图 |
| `EditImage(ctx, model, prompt, referenceImages, *ImageGenerationOptions)` | 图生图 |

`StreamConfig`：`WatchdogTimeout time.Duration`（0=禁用）、`RetryConfig *retry.Config`。

`GenerateContent` / `StreamGenerateContent*` 会先做本地校验：`model` 为空或 `contents` 为空直接返回错误，不发请求。

`NewStreamAccumulator()` + `acc.OnChunk` / `acc.Result()` 将流式 chunk 累积为完整 `*GenerateContentResponse`：所有非思考文本合并为单个文本 part 并置于首位（签名附加其上，无文本时输出仅含签名的空 part），其余非空 part（`thought`、`functionCall`、`inlineData` 等）按到达顺序追加；只处理 `candidates[0]`，空文本占位 part 被跳过。

## 模型查询

- `ListModels(ctx, *ListModelsOptions)` → `*ListModelsResponse`（`Models []Model`、`NextPageToken`）。`ListModelsOptions`：`PageSize`(1-100, 默认 50 为服务端缺省)、`PageToken`、`Filter`（如 `supportsGenerateContent=true`）；零值字段不下发 query。
- `GetModel(ctx, modelID)` → `*Model`（自动补 `models/` 前缀）。

## Files API

超过约 20MB 的图片/音频/视频需先上传，再在请求中以 `fileUri` 引用。

- `File`：`Name`(如 `files/abc123`)、`DisplayName`、`MimeType`、`URI`、`SizeBytes`、`State`(如 `FILE_STATE_ACTIVE`)、`CreateTime`/`UpdateTime`/`ExpirationTime`。
- `UploadFile(ctx, reader, size, opts)`：可恢复上传两步协议——先 `POST /upload/v1beta/files`（`X-Goog-Upload-Protocol: resumable` + `X-Goog-Upload-Command: start` 等 header）取得 `X-Goog-Upload-Url`，再向该 URL 发送 `upload, finalize`；缺少上传 URL 时报错。`opts.MimeType` 为空直接报错。
- `UploadFileOptions`：`DisplayName`（可选）、`MimeType`（必填）。
- `ListFilesOptions`：`PageSize`、`PageToken`。
- `GetFile`/`DeleteFile`：`name` 为空直接报错。

## 函数调用（Function Calling）

```go
schema := google.NewSchema().
    PropString("location", "City name", true).
    PropStringEnum("unit", "Unit", false, "celsius", "fahrenheit").
    Build() // 返回 json.RawMessage
fd := google.NewFunctionDeclaration("get_weather", "Get weather", schema)
```

- `SchemaBuilder`：`NewSchema()` 链式 `PropString` / `PropInteger` / `PropNumber` / `PropBoolean` / `PropStringEnum` / `PropArray` / `Prop` → `Build()`（返回 `json.RawMessage`）。
- 声明/工具：`NewFunctionDeclaration(name, desc, schema)`、`NewSimpleFunctionDeclaration(name, desc)`、`NewFunctionTool(decl)`、`NewFunctionToolFromDecls(decls...)`、`NewToolConfig(mode, allowedNames...)`（`FunctionCallingMode`：`AUTO`/`ANY`/`NONE`/`VALIDATED`）。
- 响应解析：`HasFunctionCalls(resp)`、`ExtractFunctionCalls(resp) []*FunctionCall`、`GetFirstFunctionCall(resp)`、`ExtractText(resp)`。
- `ToolRegistry`：`NewToolRegistry()`、`Register(name, desc, ToolHandler, schema)`、`RegisterSimple(...)`、`Get` / `Names` / `BuildTool()`(→`Tool`)、`ExecuteFunctionCalls(resp) []Part`。`ToolHandler`：`func(map[string]any) (any, error)`；结果经 `normalizeResponse` 包装（`map[string]any` 原样，nil→`{"result":nil}`，其余→`{"result": value}`）；未注册函数或处理器出错时响应体写入 `error` 字段，且保留原 `FunctionCall` 的 `ID`。
- `RunFunctionCallLoop(ctx, client, model, req, registry, *FunctionCallLoopOptions)` → `(*GenerateContentResponse, error)`：同步 `generateContent` 自动多轮，直到响应无函数调用或达 `MaxRounds`（默认 10）；`req.Tools` 为空且 `registry` 非空时自动注入 `registry.BuildTool()`。每轮把模型响应（`PreserveModelContent`，含签名）作为 `model` 消息、函数响应 parts 作为 `user` 消息追加进 `contents`。超轮返回最后一个响应和 `ErrMaxRoundsExceeded`。`FunctionCallLoopOptions`：`MaxRounds`、`OnFunctionCall`（返回 error 时返回当前响应与包装错误，中断循环）、`OnFunctionResponse`、`PreserveSignatures *bool`（默认 true；为 false 时复制模型 parts 并清除 `ThoughtSignature`）。`registry` 为 nil 时每个调用回填 `"error": "no tool registry provided"`。
- 并行辅助：`BuildParallelFunctionResponses(calls, results)`、`BuildParallelFunctionResponsesWithErrors(calls, results)`（`results` 按 `fc.ID` 匹配；缺失回 `"no result for this function call"`，后者把 `error` 值转为错误响应）。

## 思维签名（Thought Signatures）

Gemini 3 在函数调用时**强制要求**回传 `thoughtSignature`，否则返回 400。

- 常量：`ThoughtSignatureSkip="skip_thought_signature_validator"`、`ThoughtSignatureDummy="context_engineering_is_the_way_to_go"`（用于从其他模型迁移历史，官方不推荐注入自定义函数调用块）。
- 提取：`ExtractThoughtSignatureEntries(resp) []ThoughtSignatureEntry`、`ExtractFirstFunctionCallSignature(resp)`、`ExtractLastTextSignature(resp)`（以上在 thought_signatures.go）；`ExtractThoughtSignatures(resp) []Part`（multimodal.go，返回所有携带签名的 part 本身，用于多轮图片编辑回传）。
- 校验：`ValidateFunctionCallSignatures(contents) error`（仅检查每条 model 消息的**第一个** `FunctionCall` part 是否带签名——并行调用时签名只附在首个 FC 上；缺失时返回 `*MissingSignatureError`，含 `ContentIndex`/`PartIndex`/`FunctionName`）。
- 构建/保留：`PreserveModelContent(resp) *Content`、`AttachSignatureToFunctionCall(content, sig)`、`AttachSignaturesByPosition(content, entries)`、`BuildFunctionResponseTurn(resp, functionResponses) []Content`。
- 清理：`StripThoughtSignatures(contents)`、`StripOldTurnSignatures(contents, currentTurnStartIndex)`（仅清历史轮次签名，当前轮不可清除）。

## 多模态与图片生成

- Part 构造：`TextPart` / `ThoughtPart` / `InlineDataPart` / `FileDataPart` / `ImagePart` / `ImageFilePart` / `AudioPart` / `AudioFilePart` / `VideoPart` / `VideoFilePart` / `VideoPartWithMetadata` / `VideoFilePartWithMetadata` / `YouTubePart` / `YouTubePartWithMetadata`，以及函数相关 `FunctionCallPart`/`FunctionCallPartWithID`/`FunctionResponsePart`/`FunctionResponsePartWithID`/`FunctionCallPartWithSignature`/`FunctionCallPartWithIDAndSignature`/`TextPartWithSignature`/`SignaturePart`。
- `GenerateImage(ctx, model, prompt, *ImageGenerationOptions)`：模型如 `ModelGemini31FlashImage`、`ModelGemini3ProImage`、`ModelGemini25FlashImage`。`ImageGenerationOptions`：`AspectRatio`、`ImageSize`（两者任一非空才写 `responseFormat.image`）、`ResponseModalities`(默认 `[TEXT,IMAGE]`)、`ThinkingConfig`(透传，可为 nil)。
- `EditImage(ctx, model, prompt, referenceImages []Part, opts)`：基于参考图编辑（`referenceImages` 为空时报错；请求 parts 为 `[prompt, 参考图...]`）。
- 响应提取：`ExtractImages(resp) ([]Blob, []string)`（图片 Blob 列表 + 非思考文本列表）、`ExtractMedia(resp) []Blob`（所有非空 `inlineData`，不限图片）。
- Base64 工具：`EncodeBase64` / `DecodeBase64` / `EncodeFileToBase64` / `EncodeReaderToBase64` / `SaveImageToFile(blob, path)`。

## 错误分类

底层错误经 `parseAPIError`（非流式）/ `parseStreamAPIError`（流式，从 `*httputil.StreamHTTPError` 提取状态码与响应体）转为 `*llm.LLMError`：

- status 映射：`INVALID_ARGUMENT`→InvalidRequest；`PERMISSION_DENIED`/`UNAUTHENTICATED`→Authentication；`RESOURCE_EXHAUSTED`→RateLimit；`FAILED_PRECONDITION`→QuotaExceeded；`INTERNAL`/`UNAVAILABLE`/`DEADLINE_EXCEEDED`→ProviderInternal；`NOT_FOUND`→NoRoute；默认→ProviderInternal。HTTP ≥400 但响应体解析不出 `error.message` 时，非流式路径按 `googleStatusToReason("")`（即 ProviderInternal）+ `"HTTP <code>"` 兜底；流式路径原样返回底层错误。
- 无状态码的网络层错误→Transport。

## 关键类型与常量

- 内容：`Content`(`Role`: `RoleUser="user"` / `RoleModel="model"`)、`Part`（含 `Text`/`InlineData`/`FileData`/`FunctionCall`/`FunctionResponse`/`ExecutableCode`/`CodeExecutionResult`/`Thought`/`ThoughtSignature`/`VideoMetadata`）、`Blob`、`FileData`、`VideoMetadata`、`ResponseFormat`/`ImageResponseFormat`。
- 函数：`FunctionCall`(含 `ID`，Gemini 3 每次返回唯一 ID)、`FunctionResponse`、`FunctionDeclaration`、`Tool`(含 `GoogleSearch`/`URLContext`/`CodeExecution`/`GoogleSearchRetrieval` 标记)、`ToolConfig`/`FunctionCallingConfig`、`ExecutableCode`、`CodeExecutionResult`。
- 生成配置：`ThinkingConfig`(`IncludeThoughts`/`ThinkingBudget`(Gemini 2.5)/`ThinkingLevel`(Gemini 3))、`GenerationConfig`(`Temperature`/`TopP`/`TopK`/`MaxOutputTokens`/`StopSequences`/`ResponseMIMEType`/`ResponseSchema`/`ThinkingConfig`/`PresencePenalty`/`FrequencyPenalty`/`Seed`/`ResponseLogprobs`/`Logprobs`/`ResponseModalities`/`ResponseFormat`/`MediaResolution`)、`SafetySetting`。
- 请求/响应：`GenerateContentRequest`、`Candidate`、`GenerateContentResponse`、`UsageMetadata`(`PromptTokenCount`/`CandidatesTokenCount`/`TotalTokenCount`/`ThoughtsTokenCount`/`CacheTokensDetails`)、`CacheTokenDetail`、`GroundingMetadata`。
- 模型/计数：`Model`、`ListModelsResponse`、`CountTokensRequest`/`CountTokensResponse`。
- 错误：`APIError`(`Code`/`Message`/`Status`)、`ErrorResponse`。
- 常量：`DefaultBaseURL`、`FinishReason*`(`STOP`/`MAX_TOKENS`/`SAFETY`/`RECITATION`/`OTHER`/`BLOCKLIST`/`PROHIBITED`/`SPII`/`MALFORMED_FUNCTION_CALL`)、`HarmCategory*`/`HarmProbability*`/`HarmBlockThreshold*`、`FunctionCallingMode*`(`AUTO`/`ANY`/`NONE`/`VALIDATED`)、`ThinkingLevel*`(`minimal`/`low`/`medium`/`high`)、`ResponseModality*`(`TEXT`/`IMAGE`)、`MediaResolution*`、`AspectRatio*`(`1:1`…`21:9`)、`ImageSize*`(`512`/`1K`/`2K`/`4K`)、图片/音频/视频 MIME 常量、图片生成模型名 `ModelGemini31FlashImage`/`ModelGemini3ProImage`/`ModelGemini25FlashImage`。
