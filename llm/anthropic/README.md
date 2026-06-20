# anthropic — Anthropic (Claude) Provider 实现

Anthropic Claude API 的 `llm.Provider` 实现。

## 功能

- 实现 `llm.Provider` 接口（`DoGenerate` / `DoStream`）
- 支持 Messages API 流式与非流式调用
- 完整支持 Extended Thinking（推理过程）、签名验证
- 支持 Prompt Caching（`cache_control` 断点）
- 支持工具调用（tool_use）和视觉理解（vision）

## 关键类型

| 类型 | 说明 |
|------|------|
| `Client` | Anthropic API 客户端 |
| `Option` | 函数式配置选项 |

## 使用示例

```go
prov := anthropic.New(
    anthropic.WithAPIKey("sk-ant-xxx"),
)

result, err := prov.DoGenerate(ctx, llm.GenerateParams{
    Model:       llm.ChatModel("claude-sonnet-4-20250514"),
    Messages:    []llm.Message{llm.UserMessage("你好")},
    CachePolicy: llm.CachePolicyAuto,
})
```
