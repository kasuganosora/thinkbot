# llm — 统一 LLM 抽象层

本模块是 thinkbot 的 LLM 核心抽象层。它的设计目标是：

- **一套 API，多家 Provider** — OpenAI / Anthropic / Google / Grok 等 provider 实现同一个 `Provider` 接口，上层代码不关心底层差异。
- **类型安全** — Message、Tool、Stream 全部强类型，告别 `map[string]any` 地狱。
- **多步编排** — 内置工具自动执行循环（Agent loop），支持流式输出、审批、并行执行。
- **零外部依赖** — Schema 推断用纯反射实现，不引入 jsonschema 库。
- **上下文安全** — 内置 `PatchToolCalls`（修补悬挂工具调用）和 `ContextReduction`（超限工具结果截断），保障多步编排的健壮性。

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
    fmt.Printf("Token 用量: input=%d output=%d\n",
        result.Usage.InputTokens, result.Usage.OutputTokens)
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
        MaxRetries: 3,
        BaseDelay:  time.Second,
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
| `ModelLister` | `ListModels(ctx)` | 列出可用模型 |
| `TestableProvider` | `Test(ctx)` / `TestModel(ctx, id)` | 健康检查 |
| `EmbeddingProvider` | `DoEmbed(ctx, params)` | 文本嵌入 |
| `SpeechProvider` | `DoSpeech(ctx, params)` | 文字转语音 |
| `TranscriptionProvider` | `DoTranscribe(ctx, params)` | 语音转文字 |

```go
// 探测能力
if lister, ok := prov.(llm.ModelLister); ok {
    models, _ := lister.ListModels(ctx)
}
```

### GenerateParams

所有参数都在一个结构体里：

```go
type GenerateParams struct {
    Model    *Model          // 模型 ID（用 llm.ChatModel("gpt-4o") 创建）
    System   string          // System prompt
    Messages []Message       // 对话消息
    Tools    []Tool          // 可用工具列表
    ToolChoice any           // "auto" | "none" | "required"
    ResponseFormat *ResponseFormat // JSON 输出格式
    Temperature  *float64
    MaxTokens    *int
    // ... 更多参数
}
```

### GenerateResult

```go
type GenerateResult struct {
    Text         string        // 生成的文本
    Reasoning    string        // 推理过程（o1/o3/Claude thinking）
    FinishReason FinishReason  // 停止原因
    Usage        Usage         // Token 用量
    ToolCalls    []ToolCall    // 模型请求的工具调用
    ToolResults  []ToolResult  // 工具执行结果（多步模式下）
    Steps        []StepResult  // 每一步的结果（多步模式下）
    Messages     []Message     // 所有输出消息（多步模式下）
}
```

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
| `TextPart` | `Text`, `CacheControl` | 文本 |
| `ReasoningPart` | `Text` | 模型推理/思考过程 |
| `ImagePart` | `Image`, `MediaType` | 图片（URL 或 base64） |
| `FilePart` | `Data`, `MediaType`, `Filename` | 任意文件 |
| `ToolCallPart` | `ToolCallID`, `ToolName`, `Input` | 工具调用（assistant 消息） |
| `ToolResultPart` | `ToolCallID`, `Result`, `IsError` | 工具结果（tool 消息） |

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
fmt.Printf("共 %d 步, %d tokens\n", len(sr.Steps), sr.Messages)
```

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
| `ToolApprovalDeferred` | 暂停循环，等待外部确认后恢复 |

### 回调一览

| 选项 | 时机 | 用途 |
|---|---|---|
| `WithMaxSteps(n)` | — | 0=单次, >0=最多 N 步, -1=无限 |
| `WithOnStep(fn)` | 每步完成后 | 观察/修改下一步参数 |
| `WithOnFinish(fn)` | 全部完成后 | 最终汇总 |
| `WithPrepareStep(fn)` | 每步开始前 | 动态调整参数 |
| `WithApprovalHandler(fn)` | 工具需要审批时 | 人工确认 |

### StepResult

每一步的结果都记录在 `StepResult` 中：

```go
type StepResult struct {
    Text         string       // 该步生成的文本
    Reasoning    string       // 推理过程
    ToolCalls    []ToolCall   // 该步请求的工具调用
    ToolResults  []ToolResult // 工具执行结果
    Usage        Usage        // 该步的 token 用量
    Messages     []Message    // 该步产生的消息
}
```

`GenerateResult.Steps` 是所有步骤的 `StepResult` 数组，`GenerateResult.Messages` 是所有步骤消息的汇总。

---

## 上下文安全

### PatchToolCalls — 修补悬挂工具调用

当历史消息中存在 assistant 发出的 tool call 但缺少对应的 tool result 时，部分 API（如 Anthropic）会拒绝请求。`PatchToolCalls` 自动检测并补全空的 tool result 消息：

```go
// 在发送请求前调用
params.Messages = llm.PatchToolCalls(params.Messages)
```

`OrchestrateGenerate` 和 `OrchestrateStream` 已内置此调用（包括单步快速路径）。

### ContextReduction — 工具结果截断

多步编排中，工具执行结果可能累积到超长上下文。`ContextReduction` 在 `PrepareStep` 回调中自动截断过长的 tool result 消息，将超出阈值的部分替换为摘要预览：

```go
result, err := llm.OrchestrateGenerate(ctx, prov, &llm.OrchestrateConfig{
    Params:   params,
    MaxSteps: 10,
    PrepareStep: llm.NewContextReduction(llm.ReductionConfig{
        MaxOutputTokens: 7500, // 超过此 token 估算的工具结果会被截断
    }).PrepareStep,
})
```

截断策略：
- 每个 tool result 保留前 `MaxOutputTokens` 字符（≈1/4 的 token 值）+ `... (truncated)` 标记
- 每步最多截断一个最长的 tool result，避免一次性修改过多历史

---

## 文件结构

```
llm/
├── llm.go              # Provider 接口 + 可选能力接口 + Embedding/Speech/Transcription
├── generate.go         # GenerateParams / GenerateResult / StepResult / ResponseMetadata
├── stream.go           # StreamResult + 所有 StreamPart 类型
├── message.go          # Message / MessagePart 类型 + 构造函数
├── message_json.go     # Message 的自定义 JSON 序列化
├── tool.go             # Tool / ToolCall / ToolResult + 审批类型
├── tool_schema.go      # NewTool[T] 泛型 + struct→JSONSchema 反射推断
├── orchestrate.go      # 多步编排：OrchestrateGenerate / OrchestrateStream
├── patchtoolcalls.go   # PatchToolCalls — 修补悬挂工具调用
├── reduction.go        # ContextReduction — 工具结果超限截断
├── quota_provider.go   # QuotaRecordingProvider — 拦截所有 LLM 调用自动记账 Token 用量
├── model.go            # Model 类型
├── usage.go            # Usage / Token 统计
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

3. 可选实现 `ModelLister`、`TestableProvider` 等接口。

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

## QuotaRecordingProvider — 全链路 Token 记账

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

### Context 辅助函数

| 函数 | 说明 |
|------|------|
| `WithQuotaDimension(ctx, dim)` | 将配额维度字符串注入 context |
| `QuotaDimensionFromContext(ctx)` | 从 context 读取配额维度（未设置时返回空串） |

`QuotaRecordingProvider` 在 `DoStream` 时通过拦截 `FinishPart` 的 `TotalUsage.TotalTokens` 完成记账。如果 context 中没有 dimension（未设置），则跳过记账，不影响正常调用。
