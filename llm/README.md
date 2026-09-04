# llm — 统一 LLM 抽象层

本模块是 thinkbot 的 LLM 核心抽象层。它的设计目标是：

- **一套 API，多家 Provider** — OpenAI / Anthropic / Google / Grok 等 provider 实现同一个 `Provider` 接口，上层代码不关心底层差异。
- **类型安全** — Message、Tool、Stream 全部强类型，告别 `map[string]any` 地狱。
- **多步编排** — 内置工具自动执行循环（Agent loop），支持流式输出、审批、并行执行、动态步数控制与循环检测。
- **零外部依赖** — Schema 推断用纯反射实现，不引入 jsonschema 库。
- **上下文安全** — 内置 `PatchToolCalls`（修补悬挂工具调用）、`TruncateOutput`（工具输出截断）、`Reduction`（轻量压缩）与 `Compactor`（对话级摘要压缩）多层防护。
- **统一错误分类** — 所有 provider 适配器将错误包装为 `*LLMError`，按 `ErrorReason` 归类以支持智能重试/路由。

---

## 目录

- [快速开始](#快速开始)
- [兼容 OpenAI 协议的第三方供应商](#兼容-openai-协议的第三方供应商)
- [核心概念](#核心概念)
- [消息系统](#消息系统)
- [工具调用](#工具调用)
- [流式输出](#流式输出)
- [多步编排](#多步编排)
- [上下文安全](#上下文安全)
- [Prompt 缓存策略](#prompt-缓存策略)
- [工具延迟加载（Tool Deferral）](#工具延迟加载tool-deferral)
- [统一错误处理](#统一错误处理)
- [Token 估算](#token-估算)
- [使用统计与配额记账](#使用统计与配额记账)
- [文件结构](#文件结构)
- [如何写一个新 Provider](#如何写一个新-provider)

---

## 快速开始

```go
package main

import (
    "context"
    "fmt"

    "github.com/kasuganosora/thinkbot/llm"
    "github.com/kasuganosora/thinkbot/llm/openai"
)

func main() {
    ctx := context.Background()

    // 1. 创建 provider
    prov := openai.New(
        openai.WithAPIKey("sk-xxx"),
    )

    // 2. 构造请求
    result, err := prov.DoGenerate(ctx, llm.GenerateParams{
        Model:    llm.ChatModel("gpt-4o"),
        System:   "你是一个有帮助的助手。",
        Messages: []llm.Message{
            llm.UserMessage("你好！"),
        },
    })
    if err != nil {
        panic(err)
    }

    fmt.Println(result.Text)
    fmt.Printf("Token 用量: input=%d output=%d total=%d\n",
        result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.TotalTokens)
}
```

---

## 兼容 OpenAI 协议的第三方供应商

很多供应商（DeepSeek、Moonshot/Kimi、SiliconFlow、Together AI、Groq、零一万物等）的 API 完全兼容 OpenAI 协议。这些供应商**不需要单独的 provider 实现**，直接复用 `openai.New()`，只需改 `BaseURL` 和模型 ID 即可：

```go
// DeepSeek
prov := openai.New(
    openai.WithAPIKey("sk-xxx"),
    openai.WithBaseURL("https://api.deepseek.com"),
)

// Moonshot (Kimi)
prov := openai.New(
    openai.WithAPIKey("sk-xxx"),
    openai.WithBaseURL("https://api.moonshot.cn/v1"),
)

// SiliconFlow (硅基流动)
prov := openai.New(
    openai.WithAPIKey("sk-xxx"),
    openai.WithBaseURL("https://api.siliconflow.cn/v1"),
)

// Together AI
prov := openai.New(
    openai.WithAPIKey("xxx"),
    openai.WithBaseURL("https://api.together.xyz/v1"),
)

// Groq
prov := openai.New(
    openai.WithAPIKey("gsk_xxx"),
    openai.WithBaseURL("https://api.groq.com/openai/v1"),
)

// 零一万物 (01.AI)
prov := openai.New(
    openai.WithAPIKey("xxx"),
    openai.WithBaseURL("https://api.lingyiwanwu.com/v1"),
)
```

> **注意**：`BaseURL` 的格式取决于供应商。有的需要 `/v1` 后缀，有的不需要。具体看各供应商文档。

使用时模型 ID 填供应商自己的：

```go
result, _ := prov.DoGenerate(ctx, llm.GenerateParams{
    Model:    llm.ChatModel("deepseek-chat"),  // 或 "moonshot-v1-8k"、"Qwen/Qwen2.5-72B-Instruct" 等
    Messages: []llm.Message{llm.UserMessage("你好")},
})
```

### 常见 BaseURL 速查表

| 供应商 | BaseURL | 示例模型 ID |
|---|---|---|
| DeepSeek | `https://api.deepseek.com` | `deepseek-chat`, `deepseek-reasoner` |
| Moonshot (Kimi) | `https://api.moonshot.cn/v1` | `moonshot-v1-8k`, `moonshot-v1-32k` |
| SiliconFlow | `https://api.siliconflow.cn/v1` | `Qwen/Qwen2.5-72B-Instruct` |
| Together AI | `https://api.together.xyz/v1` | `meta-llama/Llama-3.3-70B-Instruct-Turbo` |
| Groq | `https://api.groq.com/openai/v1` | `llama-3.3-70b-versatile` |
| 零一万物 | `https://api.lingyiwanwu.com/v1` | `yi-large` |
| Ollama (本地) | `http://localhost:11434/v1` | `llama3.2`, `qwen2.5` |
| vLLM (本地) | `http://localhost:8000/v1` | 部署的模型名 |

### 本地部署

Ollama 和 vLLM 等本地推理框架也兼容 OpenAI 协议：

```go
// Ollama（需要先 ollama serve）
prov := openai.New(
    openai.WithBaseURL("http://localhost:11434/v1"),
    openai.WithAPIKey("ollama"), // Ollama 不检查 key，随便填
)

// vLLM
prov := openai.New(
    openai.WithBaseURL("http://localhost:8000/v1"),
    openai.WithAPIKey("vllm"), // vLLM 默认也不检查 key
)
```

### 统一接口

无论底层是哪家供应商，`prov` 都满足 `llm.Provider` 接口，后续的所有操作（流式、工具调用、多步编排）完全一致：

```go
// 可以直接传给 OrchestrateGenerate
result, _ := llm.OrchestrateGenerate(ctx, prov, &llm.OrchestrateConfig{
    Params:   params,
    MaxSteps: 10,
    Tools:    tools,
})
```

### 共享 HTTP 客户端

当你同时使用多个 provider（比如同时调 OpenAI 和 Anthropic）时，可以用 `WithSharedClient` 共享底层连接池和基础设施：

```go
import httputil "github.com/kasuganosora/thinkbot/util/http"

// 1. 创建一个共享的 HTTP 客户端（统一配置代理、超时、连接池等）
sharedHTTP := httputil.New(
    httputil.WithTimeout(60 * time.Second),
    httputil.WithRetry(retry.Config{
        MaxRetries:    3,
        FixedInterval: time.Second,
    }),
)

// 2. 各 provider 共享底层 Transport / 连接池，但 baseURL 和认证头各自独立
openaiProv := openai.New(
    openai.WithAPIKey("sk-xxx"),
    openai.WithBaseURL("https://api.openai.com/v1"),
    openai.WithSharedClient(sharedHTTP),
)

deepseekProv := openai.New(
    openai.WithAPIKey("sk-yyy"),
    openai.WithBaseURL("https://api.deepseek.com"),
    openai.WithSharedClient(sharedHTTP),
)
```

`WithSharedClient` 内部会 `Clone` 出一个独立的 `Client` 实例（共享 `Transport` 和连接池，但 baseURL / headers / 重试配置各自独立设置），所以多个 provider 之间互不影响。

---

## 核心概念

### Provider 接口

```go
type Provider interface {
    Name() string
    DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error)
    DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error)
}
```

每个 provider（openai、anthropic、google、grok）都实现这个接口。你只需要面向 `llm.Provider` 编程。

### 可选能力接口

provider 可以选择性实现以下接口，上层代码通过类型断言探测：

| 接口 | 方法 | 用途 |
|---|---|---|
| `ModelLister` | `ListModels(ctx) ([]Model, error)` | 列出可用模型 |
| `TestableProvider` | `Test(ctx) *ProviderTestResult` / `TestModel(ctx, id) (*ModelTestResult, error)` | 健康检查 |
| `EmbeddingProvider` | `DoEmbed(ctx, EmbedParams) (*EmbedResult, error)` | 文本嵌入 |
| `SpeechProvider` | `DoSpeech(ctx, SpeechParams) (*SpeechResult, error)` | 文字转语音 |
| `TranscriptionProvider` | `DoTranscribe(ctx, TranscriptionParams) (*TranscriptionResult, error)` | 语音转文字 |

```go
// 探测能力
if lister, ok := prov.(llm.ModelLister); ok {
    models, _ := lister.ListModels(ctx)
}

// 健康检查
if tp, ok := prov.(llm.TestableProvider); ok {
    res := tp.Test(ctx)
    switch res.Status { // llm.ProviderStatusOK / ProviderStatusUnhealthy / ProviderStatusUnreachable
    case llm.ProviderStatusOK:
        // ...
    }
}
```

`ProviderStatus` 常量：`ProviderStatusOK` / `ProviderStatusUnhealthy` / `ProviderStatusUnreachable`。多模态/嵌入/语音相关参数与结果类型见下表：

| 类型 | 关键字段 |
|---|---|
| `EmbedParams` | `Model *Model`、`Values []string`、`Dimensions *int` |
| `EmbedResult` | `Embeddings [][]float64`、`Tokens int` |
| `SpeechParams` | `Model`、`Text`、`Voice`、`Format`、`Speed *float64`、`Instructions`、`Extra map[string]any` |
| `SpeechResult` | `Audio []byte`、`ContentType string` |
| `TranscriptionParams` | `Model`、`Audio []byte`、`Filename`、`ContentType`、`Language`、`Prompt`、`Extra map[string]any` |
| `TranscriptionResult` | `Text`、`Language`、`DurationSeconds`、`Words []TranscriptionWord`、`ProviderMetadata` |
| `TranscriptionWord` | `Text`、`Start`、`End`、`SpeakerID` |

### Model 类型

```go
// 聊天模型
model := llm.ChatModel("gpt-4o")

// 嵌入模型
model := llm.EmbeddingModel("text-embedding-3-small")
```

`Model` 含 `ID`、`DisplayName`、`Type` 三个字段；`Type` 为 `ModelTypeChat` 或 `ModelTypeEmbedding`。`GenerateParams.Model` 接受 `*Model`（聊天/多模态生成）。

### GenerateParams

所有参数都在一个结构体里：

```go
type GenerateParams struct {
    Model    *Model    // 模型 ID（用 llm.ChatModel("gpt-4o") 创建）
    System   string    // System prompt
    Messages []Message // 对话消息

    Tools      []Tool // 可用工具列表
    ToolChoice any    // "auto" | "none" | "required" | {"type":"function","function":{"name":"..."}}

    ResponseFormat *ResponseFormat // JSON 输出格式（text / json_object / json_schema）

    Temperature      *float64
    TopP             *float64
    MaxTokens        *int
    StopSequences    []string
    FrequencyPenalty *float64
    PresencePenalty  *float64
    Seed             *int
    ReasoningEffort  *string // 推理强度（如 "low" / "medium" / "high"）

    // CachePolicy 控制 prompt 缓存断点放置方式：
    //   ""     = provider 默认（anthropic 用 auto，其余用 none）
    //   "none" = 清除所有缓存标记
    //   "auto" = 在稳定前缀上自动放置断点
    CachePolicy CachePolicy
    // CacheKey 透传给支持缓存键提示的 provider（如 OpenAI promptCacheKey）。
    CacheKey string
    // SystemCacheControl 由缓存策略写入，标记 system 应携带缓存断点。
    SystemCacheControl *CacheControl
}
```

`ResponseFormat` 通过 `Type`（`ResponseFormatText` / `ResponseFormatJSONObject` / `ResponseFormatJSONSchema`）与可选的 `JSONSchema any` 控制结构化输出。

### GenerateResult

```go
type GenerateResult struct {
    Text                      string         // 生成的文本
    Reasoning                 string         // 推理过程（o1/o3/Claude thinking）
    ReasoningProviderMetadata map[string]any // 推理相关的 provider 元数据
    FinishReason              FinishReason   // 停止原因
    RawFinishReason           string         // provider 原始停止原因
    Usage                     Usage          // Token 用量
    Sources                   []Source       // 引用来源
    Files                     []GeneratedFile // 模型生成的文件
    ToolCalls                 []ToolCall     // 模型请求的工具调用
    ToolResults               []ToolResult   // 工具执行结果（多步模式下）
    Response                  ResponseMetadata // 响应级元数据（id/modelId/timestamp/headers）
    DeferredToolApproval      *ToolApprovalResult // 被 defer 的审批结果

    Steps   []StepResult // 每一步的结果（多步模式下）
    Messages []Message   // 所有输出消息（多步模式下，不含原始输入）

    // LoopStoppedByGuard / LoopStopReason 仅运行时有效（json:"-"），
    // 标记本次编排循环是否因步数守卫（撞硬上限或陷入重复循环）停止，
    // 而非模型自然收尾。
    LoopStoppedByGuard bool
    LoopStopReason     string
}
```

`FinishReason` 取值：`FinishReasonStop` / `FinishReasonLength` / `FinishReasonContentFilter` / `FinishReasonToolCalls` / `FinishReasonError` / `FinishReasonOther` / `FinishReasonUnknown`。

`StepResult` 的字段与 `GenerateResult` 类似：`Text`、`Reasoning`、`FinishReason`、`RawFinishReason`、`Usage`、`ToolCalls`、`ToolResults`、`Response`、`DeferredToolApproval`、`Messages`。

### Usage（Token 用量）

```go
type Usage struct {
    InputTokens        int
    OutputTokens       int
    TotalTokens        int
    ReasoningTokens    int
    CachedInputTokens  int
    InputTokenDetails  InputTokenDetail  // NoCacheTokens / CacheReadTokens / CacheWriteTokens / ...
    OutputTokenDetails OutputTokenDetail // TextTokens / ReasoningTokens
}

// 累加另一个 Usage（编排时汇总各步）
usage.Add(&stepUsage)
```

`InputTokenDetail` 含 `NoCacheTokens`、`CacheReadTokens`、`CacheWriteTokens`、`CacheWrite5mTokens`、`CacheWrite1hTokens`。

---

## 消息系统

### 消息构造

```go
// 最简方式
msg := llm.UserMessage("你好")

// 等价于
msg := llm.Message{
    Role:    llm.MessageRoleUser,
    Content: []llm.MessagePart{llm.TextPart{Text: "你好"}},
}
```

### 消息角色

| 构造函数 | 角色 | 用途 |
|---|---|---|
| `UserMessage(text, extra...)` | `user` | 用户输入 |
| `SystemMessage(text)` | `system` | 系统提示 |
| `AssistantMessage(text)` | `assistant` | 模型回复 |
| `ToolMessage(results...)` | `tool` | 工具执行结果 |

### 多模态消息

一条消息可以包含多个 Part（文本、图片、文件、工具调用等）：

```go
msg := llm.Message{
    Role: llm.MessageRoleUser,
    Content: []llm.MessagePart{
        llm.TextPart{Text: "这张图片是什么？"},
        llm.ImagePart{Image: "data:image/png;base64,iVBOR..."},
    },
}
```

### 支持的 Part 类型

| 类型 | 字段 | 说明 |
|---|---|---|
| `TextPart` | `Text`, `CacheControl`, `ProviderMetadata` | 文本 |
| `ReasoningPart` | `Text`, `Signature`, `ProviderMetadata` | 模型推理/思考过程（`Signature` 为 Anthropic 扩展思考的加密签名，后续请求须原样回传） |
| `ImagePart` | `Image`, `MediaType`, `CacheControl` | 图片（URL 或 base64） |
| `FilePart` | `Data`, `MediaType`, `Filename`, `CacheControl` | 任意文件 |
| `ToolCallPart` | `ToolCallID`, `ToolName`, `Input`, `ProviderMetadata` | 工具调用（assistant 消息） |
| `ToolResultPart` | `ToolCallID`, `ToolName`, `InvocationID`, `Result`, `IsError` | 工具结果（tool 消息） |

### JSON 序列化

`Message` 实现了自定义的 `MarshalJSON`/`UnmarshalJSON`：
- 单个纯文本消息 → content 序列化为字符串：`{"role":"user","content":"hi"}`
- 多 Part 消息 → content 序列化为数组，每个 Part 带 `type` 字段

可以直接 `json.Marshal` / `json.Unmarshal` 存取对话历史。

### Prompt 缓存（Anthropic）

```go
// 在 TextPart 上设置 cache control
llm.TextPart{
    Text:         veryLongContext,
    CacheControl: llm.EphemeralCacheControl(), // 5 分钟缓存
}
```

> 缓存断点的自动放置由 `CachePolicy` 控制（见 [Prompt 缓存策略](#prompt-缓存策略)）。仅在 Anthropic 系 provider 生效；OpenAI / Google 使用隐式前缀缓存，无需显式断点。

---

## 工具调用

### 方式一：NewTool 泛型创建（推荐）

从 Go struct 自动推断 JSON Schema，类型安全：

```go
type WeatherParams struct {
    Location string `json:"location" jsonschema:"城市名称"`
    Units    string `json:"units,omitempty" jsonschema:"metric 或 imperial"`
}

weatherTool := llm.NewTool("get_weather", "获取天气信息",
    func(ctx *llm.ToolExecContext, input WeatherParams) (any, error) {
        return fmt.Sprintf("%s 今天晴，22°C", input.Location), nil
    })
```

`NewTool[T]` 会：
1. 通过反射从 struct 生成 JSON Schema（`json` tag → 属性名，`jsonschema` tag → 描述）
2. 包装执行函数，自动将 `any` 类型的 input 反序列化为 `T`
3. 返回一个带 `Execute` 函数的 `Tool`

### 方式二：手动构造

```go
tool := llm.Tool{
    Name:        "calculate",
    Description: "执行数学计算",
    Parameters: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "expression": map[string]any{
                "type": "string",
                "description": "数学表达式",
            },
        },
        "required": []string{"expression"},
    },
}
```

### 工具审批

对于敏感操作（如删文件、发邮件），设置 `RequireApproval: true`：

```go
deleteTool := llm.NewTool("delete_file", "删除文件",
    func(ctx *llm.ToolExecContext, input DeleteParams) (any, error) {
        os.Remove(input.Path)
        return "deleted", nil
    })
deleteTool.RequireApproval = true
```

在编排时注册审批处理器（见[多步编排](#多步编排)）。

### 工具延迟加载标记

`Tool` 还有两个与延迟加载相关的字段（详见[工具延迟加载](#工具延迟加载tool-deferral)）：

```go
tool.DeferredLoad = true // 初始只向模型展示名称+描述，按需加载完整 schema
tool.Keywords = []string{"search", "web"} // 供 tool_search 匹配
```

`ToolExecContext` 提供 `ToolCallID`、`ToolName`、`InvocationID`（服务端本次执行唯一标识）与 `SendProgress(content any)`（流式进度回调，非流式时为 nil）。

---

## 流式输出

### 基本用法

```go
sr, err := prov.DoStream(ctx, params)
if err != nil {
    panic(err)
}

for part := range sr.Stream {
    switch p := part.(type) {
    case *llm.TextDeltaPart:
        fmt.Print(p.Text) // 逐字输出
    case *llm.ReasoningDeltaPart:
        fmt.Printf("[思考] %s", p.Text)
    case *llm.StreamToolCallPart:
        fmt.Printf("[工具调用] %s(%v)\n", p.ToolName, p.Input)
    case *llm.FinishPart:
        fmt.Printf("\n[完成] 原因=%s 用量=%d tokens\n",
            p.FinishReason, p.TotalUsage.TotalTokens)
    case *llm.ErrorPart:
        fmt.Printf("[错误] %v\n", p.Error)
    }
}
```

### 便捷方法

```go
// 只取最终文本
text, err := sr.Text()

// 转换为完整的 GenerateResult（自动消费整个流）
result, err := sr.ToResult()
```

### StreamPart 类型一览

| Part 类型 | 时机 |
|---|---|
| `*StartPart` | 流开始 |
| `*StartStepPart` / `*FinishStepPart` | 每步开始/结束 |
| `*TextStartPart` / `*TextDeltaPart` / `*TextEndPart` | 文本生成 |
| `*ReasoningStartPart` / `*ReasoningDeltaPart` / `*ReasoningEndPart` | 推理过程 |
| `*ToolInputStartPart` / `*ToolInputDeltaPart` / `*ToolInputEndPart` | 工具参数流式输入 |
| `*StreamToolCallPart` | 完整工具调用 |
| `*StreamToolResultPart` | 工具执行结果 |
| `*StreamToolErrorPart` | 工具执行错误 |
| `*ToolProgressPart` | 工具执行进度 |
| `*StreamSourcePart` | 引用来源 |
| `*StreamFilePart` | 生成的文件 |
| `*FinishPart` | 整个生成完成 |
| `*ErrorPart` | 错误 |
| `*AbortPart` | 中止 |
| `*RawPart` | 原始数据（调试用） |

---

## 多步编排

多步编排是本模块最强大的功能：**自动执行工具调用并喂回模型，循环直到模型不再请求工具或达到步数上限。**

```
用户消息 → LLM → 工具调用？→ 是 → 执行工具 → 结果喂回 → LLM → ...
                              ↓ 否
                           最终回复
```

### 非流式

```go
result, err := llm.OrchestrateGenerate(ctx, prov, &llm.OrchestrateConfig{
    Params: llm.GenerateParams{
        Model:    llm.ChatModel("gpt-4o"),
        System:   "你是一个助手。",
        Messages: []llm.Message{llm.UserMessage("北京天气怎么样？")},
        Tools:    []llm.Tool{weatherTool},
    },
    MaxSteps: 5, // 最多 5 轮 LLM 调用（0=单次调用，-1=无限）
    OnStep: func(step *llm.StepResult) *llm.GenerateParams {
        fmt.Printf("步骤完成: %s\n", step.Text)
        return nil // 返回 nil 保持原参数
    },
})

// result.Text    = 最终回复
// result.Steps   = 每一步的结果
// result.Usage   = 所有步骤的总 token
// result.Messages = 所有输出消息
```

### 流式

```go
sr, err := llm.OrchestrateStream(ctx, prov, &llm.OrchestrateConfig{
    Params:   params,
    MaxSteps: 10,
})

for part := range sr.Stream {
    switch p := part.(type) {
    case *llm.TextDeltaPart:
        fmt.Print(p.Text)
    case *llm.StreamToolCallPart:
        fmt.Printf("\n[调用工具] %s\n", p.ToolName)
    case *llm.StreamToolResultPart:
        fmt.Printf("[工具结果] %v\n", p.Output)
    case *llm.ToolProgressPart:
        fmt.Printf("[进度] %v\n", p.Content)
    }
}

// 流结束后可以读取汇总数据
fmt.Printf("共 %d 步, %d tokens\n", len(sr.Steps), sr.Usage.TotalTokens)
```

### 动态步数控制（loopController）

`MaxSteps` 是“软预算”：模型持续发起**新的**工具调用时，循环可在 `MaxSteps` 基础上自动延展，直到 `HardMaxSteps` 绝对上限（默认 `MaxSteps * 3`）；若检测到模型在重复同样的工具调用（陷入循环），则提前停止。`GenerateResult.LoopStoppedByGuard` / `LoopStopReason` 标记是否因守卫停止（撞硬上限或重复循环），供上游向用户给出明确提示，避免把“步数预算耗尽”误判为 Bot 卡死。

### 工具审批

```go
result, err := llm.OrchestrateGenerate(ctx, prov, &llm.OrchestrateConfig{
    Params: llm.GenerateParams{
        Model: llm.ChatModel("gpt-4o"),
        Tools: []llm.Tool{weatherTool, deleteTool}, // deleteTool.RequireApproval = true
        Messages: []llm.Message{llm.UserMessage("删除 /tmp/foo 并查天气")},
    },
    MaxSteps: 5,
    ApprovalHandler: func(ctx context.Context, call llm.ToolCall) (llm.ToolApprovalResult, error) {
        // 在这里弹 UI 让用户确认，或自动判断
        fmt.Printf("模型想调用 %s(%v)，是否允许？(y/n)", call.ToolName, call.Input)
        // ...
        return llm.ToolApprovalResult{
            Decision: llm.ToolApprovalApproved, // 或 Rejected / Deferred
        }, nil
    },
})
```

审批决策：

| 决策 | 效果 |
|---|---|
| `ToolApprovalApproved` | 执行工具 |
| `ToolApprovalRejected` | 跳过，告知模型被拒绝 |
| `ToolApprovalDeferred` | 暂停循环，通过 `ErrToolApprovalDeferred` / `ToolApprovalDeferredError` 返回 `DeferredToolApproval`，等待外部确认后恢复 |

### 回调与配置一览

`OrchestrateConfig` 字段：

| 字段 | 类型 | 说明 |
|---|---|---|
| `Params` | `GenerateParams` | 基础请求参数 |
| `MaxSteps` | `int` | 软步数预算（0=单次, >0=上限, -1=无限） |
| `HardMaxSteps` | `int` | 绝对步数上限（<=0 表示自动 = MaxSteps*3） |
| `OnFinish` | `func(*GenerateResult)` | 全部完成后调用 |
| `OnStep` | `func(*StepResult) *GenerateParams` | 每步完成后，返回非空则覆盖下一步参数 |
| `PrepareStep` | `func(*GenerateParams) *GenerateParams` | 每步开始前（第二步起），返回非空则覆盖参数 |
| `OnToolResults` | `func(int, []ToolResultPart) []ToolResultPart` | 工具执行后、写入历史前，可修改/截断结果 |
| `ApprovalHandler` | `func(ctx, ToolCall) (ToolApprovalResult, error)` | 工具需要审批时调用 |
| `ToolChoiceForStep` | `func(step int, toolsExecuted bool) any` | 按步覆盖 `tool_choice`（如验证门控：首步强制 "required"） |
| `ToolDeferral` | `*ToolDeferral` | 工具延迟加载（见[工具延迟加载](#工具延迟加载tool-deferral)） |
| `InterruptCh` | `chan string` | 生成过程中用户中途追加内容（Claude-CLI 风格），建议带缓冲（如 cap=16） |

对应 `WithXxx` Option：`WithMaxSteps`、`WithHardMaxSteps`、`WithOnFinish`、`WithOnStep`、`WithPrepareStep`、`WithOnToolResults`、`WithApprovalHandler`、`WithInterruptChannel`。

`SandboxToolPrefix`（`"sandbox_"`）会在编排时被剥离，使模型看到通用工具名（如 `exec`、`read_file`），而不感知沙箱实现细节。

---

## 上下文安全

### PatchToolCalls — 修补悬挂工具调用

当历史消息中存在 assistant 发出的 tool call 但缺少对应的 tool result 时，部分 API（如 Anthropic）会拒绝请求。`PatchToolCalls` 自动检测并补全空的 tool result 消息：

```go
// 在发送请求前调用
params.Messages = llm.PatchToolCalls(params.Messages)
```

`OrchestrateGenerate` 和 `OrchestrateStream` 已内置此调用（包括单步快速路径）。

### TruncateOutput — 工具输出截断

每次工具执行后，`runTool` 会按 `OrchestrateConfig.ToolOutput`（零值回退 `DefaultToolOutputConfig()`，默认 MaxLines=500 / MaxBytes=50KB）对结果做字节/行级截断（保留头部+尾部，中间省略），避免单个超长结果撑爆上下文：

```go
cfg := llm.DefaultTruncationConfig() // MaxLines=500, MaxBytes=50KB
res := llm.TruncateOutput(output, cfg)
// res.Output any; res.Truncated bool; res.OriginalSize int
```

`TruncationConfig` 字段：`MaxLines int`（默认 500）、`MaxBytes int`（默认 50×1024）。

### Reduction — 编排内轻量压缩

`Reduction` 在编排循环内提供两阶段“安全网”，与重量级的 `Compactor` 互补：

- **阶段 1（TruncateToolResults）**：工具执行后，单个结果 token 估算超过 `MaxOutputTokens` 时替换为预览+摘要（token 级，区别于 `TruncateOutput` 的字节级）。
- **阶段 2（ReduceHistory）**：每步 LLM 调用前，若总消息 token 超过 `ClearThresholdTokens`，将较早的工具结果替换为紧凑占位符（零成本，无额外 LLM 调用）。

```go
reductionCfg := llm.DefaultReductionConfig() // MaxOutputTokens=7500, ClearThresholdTokens=100000, RetainRecentSteps=4
result, err := llm.OrchestrateGenerate(ctx, prov, &llm.OrchestrateConfig{
    Params:   params,
    MaxSteps: 10,
    OnToolResults: llm.NewOnToolResultsCallback(reductionCfg),   // 阶段 1
    PrepareStep:   llm.NewReducePrepareStepCallback(reductionCfg), // 阶段 2
})
```

`ReductionConfig` 字段：`MaxOutputTokens int`、`ClearThresholdTokens int`、`RetainRecentSteps int`、`ExcludeTools []string`（这些工具的结果永不被截断/压缩）。也可直接调用 `llm.TruncateToolResults(results, cfg)` / `llm.ReduceHistory(messages, cfg)`。

> 注意：旧文档曾使用 `NewContextReduction(...).PrepareStep`，该 API 已不存在，请改用上面的回调工厂函数。

### Compactor — 对话级摘要压缩

`Compactor` 实现四层上下文压缩策略：

1. **Pruning**：从最新消息向回扫描，保护区（`PruneProtect`=40000 tokens）外的旧工具输出替换为 `"[compacted]"` 占位符；可裁剪量须超过 `PruneMinimum`（20000）才执行；`ProtectedTools`（如 `skill`）的输出永不裁剪。
2. **Compaction**：总 token 超阈值时，用 LLM 生成旧消息的结构化增量摘要（保留最近 `TailTurns` 轮完整对话 + 摘要替代旧消息）。
3. **Error-triggered**：provider 返回 context overflow 错误时自动触发压缩流程。
4. **Mid-conversation system message**：在对话中插入系统消息（如日期变更提醒），而非修改 system prompt。

```go
compactor := llm.NewCompactor(llm.DefaultCompactionConfig())
// 仅裁剪（无 LLM）：
result, _ := llm.OrchestrateGenerate(ctx, prov, &llm.OrchestrateConfig{
    Params:      params,
    MaxSteps:    10,
    PrepareStep: llm.CompactionPrepareStep(compactor), // func(*GenerateParams) *GenerateParams
})
```

`DefaultCompactionConfig()` 默认值（保守的上下文窗口预算）：`MaxTokens=64000`、`ReservedTokens=20000`、`TailTokens=8000`、`TailTurns=2`、`MinMessagesToCompact=6`、`SummaryMaxTokens=4096`、`ToolOutputThreshold=500`、`Auto=true`。`Compactor` 还提供 `IsOverflow` / `IsOverflowByUsage` / `ShouldCompact` / `PruneToolOutputs` / `Compact(ctx, params, provider)`，并通过 `DoomLoopThreshold`（连续压缩上限 3 次）防止无限压缩循环——doom-loop 计数只在 prune 后仍溢出时增加，prune 成功或摘要后不再溢出即清零，避免「每轮压缩都计数」造成的假性 doom-loop 卡死。

`CompactionPrepareStepWithProvider(compactor, provider)` 返回 `func(context.Context) func(*GenerateParams) *GenerateParams`，可提供 LLM 摘要能力（provider-backed compaction）。

错误触发压缩时可借助检测函数：

```go
if llm.IsContextOverflowError(err) {
    // 触发压缩重试
}
```

`IsContextOverflow(message string)` 直接匹配错误消息；`IsContextOverflowError(err error)` 兼容 `*LLMError`（`ErrorReasonContextOverflow`）与原始 error。

中间对话系统消息：

```go
mid := llm.NewDateChangeMessage("2026-08-03") // MidConversationMessage{Type, Content, Timestamp}
msgs := llm.InsertMidConversationMessages(history, mid)
```

---

## Prompt 缓存策略

`CachePolicy` 控制 prompt 缓存断点的放置方式：

```go
type CachePolicy string // "none" | "auto"

const MaxCacheBreakpoints = 4 // Anthropic 单次请求允许的最大断点数
```

- **Anthropic / Bedrock / Alibaba**：显式断点（`cache_control: {type:"ephemeral"}`），最多 4 个。
- **OpenAI / Azure / Copilot**：隐式前缀缓存，无需显式断点；可用 `GenerateParams.CacheKey` 透传缓存键提示（如 session ID）。
- **Google / Gemini**：隐式缓存，自动处理。

`GenerateParams.CachePolicy` 取值：`""`（provider 默认：anthropic 用 auto，其余 none）、`"none"`（清除所有缓存标记）、`"auto"`（在 system / 最后工具 / 最后用户消息上自动放置断点）。编排时 `OrchestrateGenerate`/`OrchestrateStream` 会根据 provider 名自动应用合适的策略（`ShouldApplyCacheBreakpoints("anthropic")` 等返回 true 的 provider 使用显式断点）。`ApplyCachePolicy(params, policy)` 也可手动调用。

---

## 工具延迟加载（Tool Deferral）

`ToolDeferral` 模仿 Anthropic 的 `defer_loading` / `tool_search` 行为：被标记 `DeferredLoad` 的工具初始只向模型展示**名称+描述**（隐藏 `Parameters`/输入 schema），直到被“加载”；模型通过注入的 `tool_search` 工具发现延迟工具，或在引用其名称时被自动加载。加载后完整 schema 才可见，模型方可带参数调用。

加载状态**非永久**：`DefaultIdleEvictSteps`（6 步）未使用的加载工具会被卸载（schema 重新隐藏）；`DefaultMaxLoaded`（12）软上限在同时加载过多时按 LRU 淘汰，使模型可见上下文有界而非无限增长。

```go
// 每个 bot/session 创建一个 DeferralStore，按 session 隔离加载状态
store := llm.NewDeferralStore(true) // enabled=true
deferral := store.ForSession("session-123")

// 在编排配置中启用
cfg := &llm.OrchestrateConfig{
    Params:      params,
    MaxSteps:    10,
    ToolDeferral: deferral,
}
```

`ToolDeferral` 关键方法：`NewToolDeferral(enabled)`、`SetTools(full)`、`HasDeferred()`、`View() []Tool`（返回给模型的工具视图+按需注入 `tool_search`）、`Search(query) []Tool`、`Load(name)`、`IsLoaded(name)`、`SetCapacity(maxLoaded, idleEvict)`、`SetStep(step)`、`Touch(name)`、`Unload(name)`、`SetLogger(l)`。

`DeferralStore`：`NewDeferralStore(enabled)`、`ForSession(sid) *ToolDeferral`（空 sid 回退到共享 deferral；`enabled=false` 时返回 nil，绕过延迟加载）。

在 `Tool` 上通过 `DeferredLoad bool` 与 `Keywords []string`（供 `tool_search` 匹配）配合启用。

---

## 统一错误处理

所有 provider 适配器应将 API 错误包装为 `*LLMError`，使编排层能基于结构化的 `Reason` 做智能重试/路由，而非解析原始错误字符串。

```go
// 构造错误
err := llm.NewLLMError(llm.ErrorReasonRateLimit, "openai", "rate limited",
    llm.WithRetryAfter(2*time.Second),
    llm.WithHTTPContext(&llm.HTTPContext{StatusCode: 429, URL: "..."}),
)

// 提取与判断
if llmErr, ok := llm.AsLLMError(err); ok {
    fmt.Println(llmErr.Reason, llmErr.ProviderName, llmErr.Retryable)
}
if llm.IsRetryableLLMError(err) {
    // 可重试
}
```

`ErrorReason` 取值：`ErrorReasonInvalidRequest` / `ErrorReasonContextOverflow` / `ErrorReasonAuthentication` / `ErrorReasonRateLimit` / `ErrorReasonQuotaExceeded` / `ErrorReasonContentPolicy` / `ErrorReasonProviderInternal` / `ErrorReasonTransport` / `ErrorReasonInvalidProviderOutput` / `ErrorReasonUnknownProvider` / `ErrorReasonNoRoute`。`ErrorReason.IsRetryable()` 对 `rate_limit` / `provider_internal` / `transport` 返回 true。

`LLMError` 字段：`Reason`、`Message`、`ProviderName`、`Retryable`、`RetryAfterMs`、`HTTPContext *HTTPContext`。`LLMErrorOpt`：`WithCause(err)`、`WithRetryAfter(d)`、`WithHTTPContext(*HTTPContext)`、`WithRetryable(bool)`。`RetryAfter()` 返回 `time.Duration`。

---

## Token 估算

`token_count.go` 提供轻量级 token 计数估算（无需外部 tokenizer，精度约 ±15%），仅用于触发上下文压缩的阈值判断，不用于精确计费。

```go
n := llm.EstimateTokens("hello 世界")        // 混合策略估算
n = llm.EstimateMessageTokens(msg)           // 单条消息（含角色开销）
n = llm.EstimateMessagesTokens(messages)     // 消息列表（含分隔开销）
n = llm.EstimateParamsTokens(params)         // 完整 GenerateParams（system + messages）
n = llm.EstimateSystemTokens(system)         // system prompt
runes := llm.CountRunes(s)                   // Unicode 字符数（非字节数）
```

`TokenCountConfig{Mode}` 支持 `"exact"` / `"chars"` / `"hybrid"`（默认混合：区分 CJK 与非 CJK）。`DefaultTokenCountConfig()` 返回混合模式。

---

## 使用统计与配额记账

### StatsRecordingProvider — 使用统计

装饰器包裹任意 `Provider`，在每次 `DoGenerate` / `DoStream` 完成后记录 token 使用统计。

```go
wrapped := llm.NewStatsRecordingProvider(originalProv, recorder, botID)
// recorder 实现 llm.UsageRecorder（RecordUsage(ctx, UsageMetric)）

// 通过 context 控制记录行为
ctx = llm.WithStatsFeature(ctx, "vision") // 标记功能维度（如 "reply"/"chat"/"vision"）
// ctx = llm.WithStatsSkip(ctx)           // 跳过记录（由调用方自行记录，避免重复计数）
```

`UsageMetric` 维度：`BotID`、`Model`、`Feature`、`Channel`、`Usage`、`ToolCalls`、`Steps`。`WithStatsFeature` 会同时清除 `WithStatsSkip` 标记（显式指定 feature 即表示希望记录）。

### QuotaRecordingProvider — 全链路 Token 记账

装饰器模式包裹任意 `Provider`，在每次 `DoGenerate` / `DoStream` 完成后自动从 context 读取配额维度并记账。确保 SubAgent、Workflow、Memory 等绕过 pipeline 中间件的调用也能被追踪。

```go
// 1. 准备 recorder（签名与 pipeline.TokenQuotaState.AddUsage 兼容）
recorder := llm.QuotaUsageRecorder(quotaState.AddUsage)

// 2. 包裹 Provider
wrappedProv := llm.NewQuotaRecordingProvider(originalProv, recorder)

// 3. 在调用链上游注入 dimension（通常由 pipeline 中间件完成）
ctx = llm.WithQuotaDimension(ctx, "bot:bot1:chat:telegram:-123")

// 之后所有经过 wrappedProv 的调用都会自动记账
result, err := wrappedProv.DoGenerate(ctx, params)
// → recorder("bot:bot1:chat:telegram:-123", result.Usage.TotalTokens)
```

Context 辅助函数：

| 函数 | 说明 |
|------|------|
| `WithQuotaDimension(ctx, dim)` | 将配额维度字符串注入 context |
| `QuotaDimensionFromContext(ctx)` | 从 context 读取配额维度（未设置时返回空串） |

`QuotaRecordingProvider` 在 `DoStream` 时通过拦截 `FinishPart` 的 `TotalUsage.TotalTokens` 完成记账。如果 context 中没有 dimension（未设置），则跳过记账，不影响正常调用。

---

## 文件结构

```
llm/
├── llm.go              # Provider 接口 + 可选能力接口 + Embedding/Speech/Transcription
├── model.go            # Model / ModelType
├── usage.go            # Usage / Token 统计
├── generate.go         # GenerateParams / GenerateResult / StepResult / ResponseFormat / ResponseMetadata / Source
├── stream.go           # StreamResult + 所有 StreamPart 类型
├── message.go          # Message / MessagePart 类型 + 构造函数
├── message_json.go     # Message 的自定义 JSON 序列化
├── tool.go             # Tool / ToolCall / ToolResult + 审批类型
├── tool_schema.go      # NewTool[T] 泛型 + struct→JSONSchema 反射推断
├── orchestrate.go      # 多步编排：OrchestrateGenerate / OrchestrateStream
├── orchestrate_loop.go # 动态步数控制 loopController
├── repetition_guard.go # RepetitionGuard — 重复退化检测（流式增量/一次性）
├── patchtoolcalls.go   # PatchToolCalls — 修补悬挂工具调用
├── reduction.go        # Reduction — 编排内轻量压缩（TruncateToolResults / ReduceHistory）
├── tool_truncate.go    # TruncateOutput — 工具输出字节/行级截断
├── compaction.go       # Compactor — 对话级摘要压缩 + 上下文溢出检测 + 中间系统消息
├── cache_policy.go     # CachePolicy / 断点自动放置
├── errors.go           # 统一错误分类（LLMError / ErrorReason）
├── token_count.go      # EstimateTokens 等 token 估算
├── tool_defer.go       # ToolDeferral / DeferralStore — 工具延迟加载
├── quota_provider.go   # QuotaRecordingProvider — 全链路 Token 记账
├── stats.go            # UsageMetric / UsageRecorder
├── stats_provider.go   # StatsRecordingProvider — 使用统计记录
├── invocation.go       # newInvocationID — 工具执行唯一标识生成
├── media.go            # 媒体校验（ValidateImagePart / ValidateFilePart 等）
├── openai/             # OpenAI provider 实现
├── anthropic/          # Anthropic (Claude) provider 实现
├── google/             # Google (Gemini) provider 实现
└── grok/               # Grok (xAI) provider 实现
```

---

## 如何写一个新 Provider

1. 创建包目录，如 `llm/myprovider/`
2. 实现 `Provider` 接口的三个方法：

```go
package myprovider

type Client struct { /* ... */ }

func (c *Client) Name() string { return "myprovider" }

func (c *Client) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
    // 1. 将 params 转换为你的 API 请求格式
    // 2. 发送 HTTP 请求
    // 3. 将响应转换为 *llm.GenerateResult
}

func (c *Client) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
    // 1. 将 params 转换为你的 API 请求格式
    // 2. 建立 SSE 连接
    // 3. 启动 goroutine，逐事件转换为 StreamPart 写入 channel
    ch := make(chan llm.StreamPart, 64)
    go func() {
        defer close(ch)
        ch <- &llm.StartPart{}
        ch <- &llm.StartStepPart{}
        // ... 逐个发送 TextDeltaPart / ReasoningDeltaPart / StreamToolCallPart 等
        ch <- &llm.FinishStepPart{FinishReason: llm.FinishReasonStop, Usage: usage}
        ch <- &llm.FinishPart{FinishReason: llm.FinishReasonStop, TotalUsage: usage}
    }()
    return &llm.StreamResult{Stream: ch}, nil
}
```

3. 可选实现 `ModelLister`、`TestableProvider` 等接口；建议将所有错误用 `llm.NewLLMError` 包装为 `*LLMError`。

参考 `llm/openai/adapter.go` 了解完整实现。

### StreamPart 发送顺序约定

一个完整的流式响应应该按以下顺序发送 parts：

```
StartPart
  StartStepPart
    TextStartPart → TextDeltaPart... → TextEndPart      (如果生成了文本)
    ReasoningStartPart → ReasoningDeltaPart... → ReasoningEndPart  (如果有推理)
    ToolInputStartPart → ToolInputDeltaPart... → ToolInputEndPart  (工具参数流式)
    StreamToolCallPart                                     (完整工具调用)
  FinishStepPart
FinishPart
```

多步编排时，每个步骤重复 `StartStepPart...FinishStepPart`，最后只有一个 `FinishPart`。

---
