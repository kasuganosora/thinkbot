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
- 隐式前缀缓存（由 Gemini 自动管理，无需显式断点）
- 统一错误分类（转 `llm.LLMError`）

> 注意：本包提供直接的 `ListModels` / `GetModel` 方法，但**未实现**统一接口 `llm.ModelLister`（无 `ListModelsUnified`）。

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
| `WithMaxBodySize(n)` | 响应体大小上限（字节），`-1` 无限制 |
| `WithRetry(cfg retry.Config)` | 重试配置 |
| `WithHTTPClient(*http.Client)` | 自定义底层客户端 |
| `WithSharedClient(*httputil.Client)` | 共享已有客户端（复用连接池/代理，独立认证） |
| `WithProxy(proxyURL)` | 设置 HTTP/HTTPS/SOCKS5 代理 |
| `WithDump()` | 输出请求/响应 dump 日志 |

`Client.Name()` 返回 `"google"`。

## Provider 接口适配

通过统一 `llm.GenerateParams` 调用：

- `system` 映射到 `SystemInstruction`。
- 生成配置透传：`Temperature`、`TopP`、`MaxTokens`→`MaxOutputTokens`、`StopSequences`、`Seed`、`FrequencyPenalty`、`PresencePenalty`。
- `ResponseFormat`：`JSONObject`→`responseMIMEType="application/json"`；`JSONSchema`→额外设置 `responseSchema`。
- `ReasoningEffort` → `ThinkingConfig`（`effortToThinkingConfig`）：`minimal`/`none`/`low`→`low`，`medium`→`medium`，`high`→`high`，未知→`IncludeThoughts:true`。
- `ToolChoice`：`auto`→`AUTO`、`none`→`NONE`、`required`→`ANY`；`map[string]any{function:{name}}`→`ANY` + `AllowedFunctionNames`。
- 工具延迟加载（参数 `nil`）降级为 `{"type":"object"}`。
- `Usage` 映射：`PromptTokenCount`/`CandidatesTokenCount`/`TotalTokenCount`，`ThoughtsTokenCount`→`ReasoningTokens` 与 `OutputTokenDetails.TextTokens`；缓存 token 由 `CacheTokensDetails` 累加为 `CachedInputTokens` + `InputTokenDetails.CacheReadTokens`。
- 结束原因：`STOP`→Stop；`MAX_TOKENS`→Length；`SAFETY`/`RECITATION`/`BLOCKLIST`/`PROHIBITED`/`SPII`→ContentFilter；其他→Other。
- 流式：`GroundingMetadata.Web` 转为 `llm.StreamSourcePart`（URL 来源）。

## 直接 API 方法

| 方法 | 说明 |
|------|------|
| `GenerateContent(ctx, model, req)` | 同步请求，返回 `*GenerateContentResponse` |
| `StreamGenerateContent(ctx, model, req, onChunk)` | 流式 SSE，回调 `func(GenerateContentResponse) error` |
| `StreamGenerateContentWithConfig(ctx, model, req, StreamConfig, onChunk)` | 同上，支持看门狗与重试 |
| `StreamGenerateContentChannel(ctx, model, req, StreamConfig)` | 流式，返回 `(<-chan GenerateContentResponse, <-chan error)` |
| `CountTokens(ctx, model, req)` | Token 计数 |
| `ListModels(ctx, *ListModelsOptions)` | 列出模型 |
| `GetModel(ctx, modelID)` | 获取单模型信息（自动补 `models/` 前缀） |
| `UploadFile(ctx, reader, size, UploadFileOptions)` | 可恢复上传大文件 |
| `ListFiles(ctx, *ListFilesOptions)` | 列出文件 |
| `GetFile(ctx, name)` | 文件元数据（`files/xxx`） |
| `DeleteFile(ctx, name)` | 删除文件 |
| `GenerateImage(ctx, model, prompt, *ImageGenerationOptions)` | 文生图 |
| `EditImage(ctx, model, prompt, referenceImages, *ImageGenerationOptions)` | 图生图 |

`StreamConfig`：`WatchdogTimeout time.Duration`（0=禁用）、`RetryConfig *retry.Config`。

`NewStreamAccumulator()` + `acc.OnChunk` / `acc.Result()` 将流式 chunk 累积为完整 `*GenerateContentResponse`。

## 模型查询

- `ListModels(ctx, *ListModelsOptions)` → `*ListModelsResponse`（`Models []Model`、`NextPageToken`）。`ListModelsOptions`：`PageSize`(1-100,默认50)、`PageToken`、`Filter`（如 `supportsGenerateContent=true`）。
- `GetModel(ctx, modelID)` → `*Model`。

## Files API

超过约 20MB 的图片/音频/视频需先上传，再在请求中以 `fileUri` 引用。

- `File`：`Name`(如 `files/abc123`)、`DisplayName`、`MimeType`、`URI`、`SizeBytes`、`State`、`CreateTime` 等。
- `UploadFileOptions`：`DisplayName`、`MimeType`（必填）。
- `ListFilesOptions`：`PageSize`、`PageToken`。

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
- `ToolRegistry`：`NewToolRegistry()`、`Register(name, desc, ToolHandler, schema)`、`RegisterSimple(...)`、`Get` / `Names` / `BuildTool()`(→`Tool`)、`ExecuteFunctionCalls(resp) []Part`。`ToolHandler`：`func(map[string]any) (any, error)`；结果经 `normalizeResponse` 包装。
- `RunFunctionCallLoop(ctx, client, model, req, registry, *FunctionCallLoopOptions)` → `(*GenerateContentResponse, error)`：自动多轮直到无函数调用或达 `MaxRounds`（默认 10）。超轮返回 `ErrMaxRoundsExceeded`。`FunctionCallLoopOptions`：`MaxRounds`、`OnFunctionCall`、`OnFunctionResponse`、`PreserveSignatures *bool`（默认 true，保留思维签名）。
- 并行辅助：`BuildParallelFunctionResponses(calls, results)`、`BuildParallelFunctionResponsesWithErrors(calls, results)`（error 值自动转错误响应）。

## 思维签名（Thought Signatures）

Gemini 3 在函数调用时**强制要求**回传 `thoughtSignature`，否则返回 400。

- 常量：`ThoughtSignatureSkip="skip_thought_signature_validator"`、`ThoughtSignatureDummy="context_engineering_is_the_way_to_go"`（用于从其他模型迁移历史，官方不推荐注入自定义函数调用块）。
- 提取：`ExtractThoughtSignatureEntries(resp) []ThoughtSignatureEntry`、`ExtractFirstFunctionCallSignature(resp)`、`ExtractLastTextSignature(resp)`、`ExtractThoughtSignatures(resp)`（来自 multimodal.go）。
- 校验：`ValidateFunctionCallSignatures(contents) error`（返回 `*MissingSignatureError`）。
- 构建/保留：`PreserveModelContent(resp) *Content`、`AttachSignatureToFunctionCall(content, sig)`、`AttachSignaturesByPosition(content, entries)`、`BuildFunctionResponseTurn(resp, functionResponses) []Content`。
- 清理：`StripThoughtSignatures(contents)`、`StripOldTurnSignatures(contents, currentTurnStartIndex)`（仅清历史轮次签名，当前轮不可清除）。

## 多模态与图片生成

- Part 构造：`TextPart` / `ThoughtPart` / `InlineDataPart` / `FileDataPart` / `ImagePart` / `ImageFilePart` / `AudioPart` / `AudioFilePart` / `VideoPart` / `VideoFilePart` / `VideoPartWithMetadata` / `VideoFilePartWithMetadata` / `YouTubePart` / `YouTubePartWithMetadata`，以及函数相关 `FunctionCallPart`/`FunctionCallPartWithID`/`FunctionResponsePart`/`FunctionResponsePartWithID`/`FunctionCallPartWithSignature`/`FunctionCallPartWithIDAndSignature`/`TextPartWithSignature`/`SignaturePart`。
- `GenerateImage(ctx, model, prompt, *ImageGenerationOptions)`：模型如 `ModelGemini31FlashImage`、`ModelGemini3ProImage`、`ModelGemini25FlashImage`。`ImageGenerationOptions`：`AspectRatio`、`ImageSize`、`ResponseModalities`(默认 `[TEXT,IMAGE]`)、`ThinkingConfig`。
- `EditImage(ctx, model, prompt, referenceImages []Part, opts)`：基于参考图编辑。
- 响应提取：`ExtractImages(resp) ([]Blob, []string)`、`ExtractMedia(resp) []Blob`。
- Base64 工具：`EncodeBase64` / `DecodeBase64` / `EncodeFileToBase64` / `EncodeReaderToBase64` / `SaveImageToFile(blob, path)`。

## 错误分类

底层错误经 `parseAPIError` / `parseStreamAPIError` 转为 `*llm.LLMError`：

- status 映射：`INVALID_ARGUMENT`→InvalidRequest；`PERMISSION_DENIED`/`UNAUTHENTICATED`→Authentication；`RESOURCE_EXHAUSTED`→RateLimit；`FAILED_PRECONDITION`→QuotaExceeded；`INTERNAL`/`UNAVAILABLE`/`DEADLINE_EXCEEDED`→ProviderInternal；`NOT_FOUND`→NoRoute；默认→ProviderInternal。
- HTTP 层错误→Transport。

## 关键类型与常量

- 内容：`Content`(`Role`: `RoleUser="user"` / `RoleModel="model"`)、`Part`（含 `Text`/`InlineData`/`FileData`/`FunctionCall`/`FunctionResponse`/`ExecutableCode`/`CodeExecutionResult`/`Thought`/`ThoughtSignature`/`VideoMetadata`）、`Blob`、`FileData`、`VideoMetadata`、`ResponseFormat`/`ImageResponseFormat`。
- 函数：`FunctionCall`(含 `ID`，Gemini 3 每次返回唯一 ID)、`FunctionResponse`、`FunctionDeclaration`、`Tool`(含 `GoogleSearch`/`URLContext`/`CodeExecution`/`GoogleSearchRetrieval` 标记)、`ToolConfig`/`FunctionCallingConfig`、`ExecutableCode`、`CodeExecutionResult`。
- 生成配置：`ThinkingConfig`(`IncludeThoughts`/`ThinkingBudget`(Gemini 2.5)/`ThinkingLevel`(Gemini 3))、`GenerationConfig`(`Temperature`/`TopP`/`TopK`/`MaxOutputTokens`/`StopSequences`/`ResponseMIMEType`/`ResponseSchema`/`ThinkingConfig`/`PresencePenalty`/`FrequencyPenalty`/`Seed`/`ResponseLogprobs`/`Logprobs`/`ResponseModalities`/`ResponseFormat`/`MediaResolution`)、`SafetySetting`。
- 请求/响应：`GenerateContentRequest`、`Candidate`、`GenerateContentResponse`、`UsageMetadata`(`PromptTokenCount`/`CandidatesTokenCount`/`TotalTokenCount`/`ThoughtsTokenCount`/`CacheTokensDetails`)、`CacheTokenDetail`、`GroundingMetadata`。
- 模型/计数：`Model`、`ListModelsResponse`、`CountTokensRequest`/`CountTokensResponse`。
- 错误：`APIError`(`Code`/`Message`/`Status`)、`ErrorResponse`。
- 常量：`DefaultBaseURL`、`FinishReason*`(`STOP`/`MAX_TOKENS`/`SAFETY`/`RECITATION`/`OTHER`/`BLOCKLIST`/`PROHIBITED`/`SPII`/`MALFORMED_FUNCTION_CALL`)、`HarmCategory*`/`HarmProbability*`/`HarmBlockThreshold*`、`FunctionCallingMode*`(`AUTO`/`ANY`/`NONE`/`VALIDATED`)、`ThinkingLevel*`(`minimal`/`low`/`medium`/`high`)、`ResponseModality*`(`TEXT`/`IMAGE`)、`MediaResolution*`、`AspectRatio*`(`1:1`…`21:9`)、`ImageSize*`(`512`/`1K`/`2K`/`4K`)、图片/音频/视频 MIME 常量、图片生成模型名 `ModelGemini31FlashImage`/`ModelGemini3ProImage`/`ModelGemini25FlashImage`。
