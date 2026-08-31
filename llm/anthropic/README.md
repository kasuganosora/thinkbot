# anthropic — Anthropic (Claude) Provider 实现

Anthropic Claude Messages API 的 `llm.Provider` 实现。

## 功能概览

- 实现 `llm.Provider` 接口（`DoGenerate` / `DoStream`）；提供 `ListModelsUnified` 直接方法，但**未实现**统一接口 `llm.ModelLister`
- Messages API 流式与非流式调用
- Extended Thinking（扩展思考）及签名（Signature）保留，支持多轮验证
- Prompt Caching（`cache_control` 断点，单请求最多 4 个断点，超出自动静默丢弃）
- 工具调用（`tool_use`）+ `ToolRegistry` / `RunToolLoop` 自动调度
- 视觉理解（vision）与 PDF 文档输入
- Files API（beta，`files-api-2025-04-14` header）
- `CountTokens` 精确计数
- 统一错误分类（转 `llm.LLMError`，识别 `Retry-After` / `Retry-After-MS`）

## 构造与选项

```go
prov := anthropic.New(
    anthropic.WithAPIKey("sk-ant-xxx"),
    anthropic.WithBaseURL("https://api.anthropic.com"), // 默认即此
)
```

| Option | 说明 |
|--------|------|
| `WithAPIKey(key)` | 设置 API Key（默认 header `x-api-key`） |
| `WithAPIVersion(v)` | API 版本，默认 `2023-06-01` |
| `WithBaseURL(url)` | 自定义基础 URL，默认 `https://api.anthropic.com` |
| `WithTimeout(d)` | HTTP 超时 |
| `WithMaxBodySize(n)` | 响应体大小上限（字节），`-1` 无限制 |
| `WithRetry(cfg retry.Config)` | 重试配置 |
| `WithHTTPClient(*http.Client)` | 自定义底层客户端 |
| `WithSharedClient(*httputil.Client)` | 共享已有客户端（复用连接池/代理，各 Provider 独立认证） |
| `WithDump()` | 输出请求/响应 dump 日志 |

`Client.Name()` 返回 `"anthropic"`。

## Provider 接口适配

通过统一的 `llm.GenerateParams` 调用：

```go
result, err := prov.DoGenerate(ctx, llm.GenerateParams{
    Model:       llm.ChatModel("claude-sonnet-4-20250514"),
    Messages:    []llm.Message{llm.UserMessage("你好")},
    CachePolicy: llm.CachePolicyAuto,
})
```

- `MaxTokens` 未设置时默认 `DefaultMaxTokens = 4096`。
- `ReasoningEffort` 映射为 Extended Thinking 预算：`high=32000` / `medium=16000` / `low`(或`minimal`)=8000（未知值按 medium）。
- 顶层 `system` 字段支持缓存断点（`SystemCacheControl`）；工具与文本块同理。断点上限 `anthropicBreakpointCap = 4`，超出静默丢弃。
- 多轮 Extended Thinking 的 `Signature` 会被保留并在回传 `thinking` 块时原样发送。
- `llm.MessageRoleSystem` 的中间系统消息降级为 `<system-update>` 包裹的用户文本，避免静默丢失。
- `Usage` 中 `input_tokens` 仅含未缓存 token，适配器会还原为 `非缓存 + cacheRead + cacheWrite` 的总量，并拆分 `InputTokenDetails`（含 5m/1h 写入拆分）。

## 直接 API 方法（底层 Messages API）

| 方法 | 说明 |
|------|------|
| `CreateMessage(ctx, MessageRequest)` | 同步请求，返回 `*MessageResponse` |
| `StreamMessage(ctx, req, onEvent)` | 流式 SSE，回调处理 `StreamEvent` |
| `StreamMessageWithConfig(ctx, req, StreamConfig, onEvent)` | 同上，支持看门狗超时与重试 |
| `StreamMessageChannel(ctx, req, StreamConfig)` | 流式，通过 channel 返回 `(<-chan StreamEvent, <-chan error)` |
| `CountTokens(ctx, CountTokensRequest)` | 统计 token 数 |
| `NewStreamAccumulator()` + `acc.OnEvent` / `acc.Result()` | 将事件累积为完整 `*MessageResponse` |

`StreamConfig` 字段：`WatchdogTimeout time.Duration`（0=禁用）、`RetryConfig *retry.Config`。

## 模型查询

- `ListModels(ctx, *ListModelsOptions)` → `*ListModelsResponse`（`Data []Model`、`FirstID`、`LastID`、`HasMore`）。`ListModelsOptions`：`Limit`(1-1000,默认20)、`BeforeID`、`AfterID`。
- `GetModel(ctx, modelID)` → `*Model`（`Type`、`ID`、`DisplayName`、`CreatedAt`）。
- 统一接口 `ListModelsUnified(ctx)` → `[]llm.Model`（类型均为 `llm.ModelTypeChat`）。

## 工具（tool_use）

```go
schema := anthropic.NewSchema().
    PropString("location", "City name", true).
    PropStringEnum("unit", "Unit", false, "celsius", "fahrenheit").
    Build()
tool := anthropic.NewTool("get_weather", "Get weather", schema)
```

- `SchemaBuilder`：`NewSchema()` 链式 `PropString` / `PropStringFormat` / `PropInteger` / `PropNumber` / `PropBoolean` / `PropStringEnum` / `PropArray` / `Prop` → `Build()` 产出 `map[string]any`。
- 工具构造：`NewTool(name, desc, schema)`、`NewSimpleTool(name, desc)`（无参）、`NewStrictTool(name, desc, schema)`（`strict:true`）、`Tool.WithExamples(...)`。
- `ToolChoice` 构造：`ChoiceAuto()` / `ChoiceAny()` / `ChoiceTool(name)` / `ChoiceNone()`，`.WithDisableParallel(bool)`。
- 内容块构造：`ToolUseBlock(id, name, input)`、`ToolResultBlock(id, content)`、`ToolResultStringBlock(id, text)`、`ToolResultErrorBlock(id, errMsg)`（标记 `is_error`）。
- 响应解析：`HasToolUse(resp)`、`ExtractToolUse(resp) []ToolUseEntry`、`GetFirstToolUse(resp)`、`ToolUseEntry.ParsedInput(v)`、`ExtractText(resp)`。
- `ToolRegistry`：`NewToolRegistry()`、`Register(name, desc, ToolHandler, schema)`、`RegisterSimple(...)`、`Get` / `Names` / `BuildTools()`、`ExecuteToolCalls(resp) []ContentBlock`。`ToolHandler` 签名为 `func(map[string]any) (any, error)`。
- `RunToolLoop(ctx, client, req, registry, *ToolLoopOptions)` → `(*MessageResponse, error)`：自动多轮直到无 `tool_use` 或达到 `MaxRounds`（默认 10）。超轮返回 `ErrMaxRoundsExceeded`。`ToolLoopOptions`：`MaxRounds`、`OnToolUse`、`OnToolResult`。
- 并行辅助：`BuildParallelToolResults(entries, results)`、`BuildParallelToolResultsWithErrors(entries, results)`（`error` 值自动转 `is_error`）。

## 视觉与 PDF

- 图片块：`Base64ImageBlock(mediaType, data)`、`URLImageBlock(url)`、`FileImageBlock(fileID)`（需 Files API）。
- 视觉消息：`ImageWithText(image, text)`（推荐图片在前）、`MultiImageContent(text, images...)`。
- PDF：`MimeTypePDF = "application/pdf"`、`Base64DocumentBlock(data)`、`DocumentWithText(pdfData, text)`。
- Thinking 块回传：`ThinkingBlock(thinking, signature)`。

### 图片 token 计算与缩放

- `ImagePatchSize = 28`（每 28×28 像素补丁 = 1 token）。
- 标准分辨率：`ImageMaxEdgeStandard = 1568`、`ImageMaxTokensStandard = 1568`。
- 高分辨率（Opus 4.7+）：`ImageMaxEdgeHighRes = 2576`、`ImageMaxTokensHighRes = 4784`。
- `CountImageTokens(w, h)`、`ResizedSize(w, h, maxEdge, maxTokens)`、`ResizedSizeStandard(w, h)`、`ResizedSizeHighRes(w, h)`、`ToRelativeCoordinates(x, y, origW, origH, maxEdge, maxTokens)`。

## Files API（beta）

所有方法自动附加 `anthropic-beta: files-api-2025-04-14` header。

| 方法 | 说明 |
|------|------|
| `UploadFile(ctx, UploadFileParams)` | 上传（`Filename`、`MimeType`、`Reader`），返回 `*FileMetadata` |
| `ListFiles(ctx, *ListFilesOptions)` | 列出（`AfterID`/`BeforeID`/`Limit`/`ScopeID`） |
| `DownloadFile(ctx, fileID)` | 返回 `([]byte, contentType, error)` |
| `DownloadFileReader(ctx, fileID)` | 返回 `(*bytes.Reader, contentType, error)` |
| `GetFileMetadata(ctx, fileID)` | 文件元数据 |
| `DeleteFile(ctx, fileID)` | 删除，返回 `*DeletedFile` |

`BetaFilesAPI = "files-api-2025-04-14"`。

## 错误分类

底层错误经 `parseAPIError` / `parseStreamAPIError` 转为 `*llm.LLMError`：

- error type 映射：`invalid_request_error`/`request_too_large` → `ErrorReasonInvalidRequest`；`authentication_error`/`permission_error` → `ErrorReasonAuthentication`；`rate_limit_error` → `ErrorReasonRateLimit`；`not_found_error` → `ErrorReasonNoRoute`；`overloaded_error`/`api_error` → `ErrorReasonProviderInternal`；`content_policy_violation` → `ErrorReasonContentPolicy`；`billing_error` → `ErrorReasonQuotaExceeded`。
- HTTP 状态兜底：`429`→RateLimit、`401/403`→Authentication、`402`→QuotaExceeded、`>=500`→ProviderInternal、`>=400`→InvalidRequest。
- 识别 `Retry-After`（秒）与 `Retry-After-MS`（毫秒），通过 `llm.WithRetryAfter` 设置重试延迟。

## 关键类型与常量

- 消息类型：`Message`、`MessageContent`（字符串或 `[]ContentBlock`，含自定义 JSON 编解码）、`ContentBlock`、`ContentType`（Text/Image/ToolUse/ToolResult/Document/Thinking）、`ImageSource`（base64/url/file）、`TextContent`、`Base64ImageSource`/`URLImageSource`/`FileImageSource`。
- Prompt 缓存：`CacheControl{Type, TTL}`、`CacheTTL5m="5m"`、`CacheTTL1h="1h"`、`EphemeralCacheControl()`、`EphemeralCacheControl1h()`。
- system 块：`SystemBlock`、`SystemText(text)`、`SystemTextWithCache(text, cc)`、`SystemBlocks(...)`。
- 请求/响应：`MessageRequest`、`MessageResponse`、`Usage`（含 `CacheCreationTokens`/`CacheReadTokens`/`CacheCreation{Ephemeral5mTokens, Ephemeral1hTokens}`）、`ThinkingConfig`/`ThinkingEnabled(budget)`。
- 常量：`APIVersion="2023-06-01"`、`DefaultBaseURL="https://api.anthropic.com"`、`RoleUser`/`RoleAssistant`、`StopReason*`（EndTurn/MaxTokens/StopSequence/ToolUse）、`ContentType*`、`ToolChoice*`(Auto/Any/Tool/None)、图片 MIME（`ImageJPEG` 等）、`MimeTypePDF`。
- 流式：`StreamEvent`、`StreamEventType`（`EventMessageStart` 等）、`Delta`（`text_delta`/`input_json_delta`/`thinking_delta`/`signature_delta`/`message_delta`）。
- 错误：`APIError`、`ErrorResponse`。
- 文件：`FileMetadata`、`ListFilesResponse`、`DeletedFile`、`FileScope`、`UploadFileParams`、`ListFilesOptions`。
