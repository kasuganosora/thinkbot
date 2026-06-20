# grok — xAI Grok Provider 实现

xAI Grok API 的 `llm.Provider` 实现，兼容 OpenAI 协议格式。

## 功能

- 实现 `llm.Provider` 接口（`DoGenerate` / `DoStream`）
- 支持多模态输入（文本 + 图片）
- 支持音频和视频生成
- 基于 OpenAI Chat Completions 协议

## 关键类型

| 类型 | 说明 |
|------|------|
| `Client` | Grok API 客户端 |

## 使用示例

```go
prov := grok.New(
    grok.WithAPIKey("xai-xxx"),
)

result, err := prov.DoGenerate(ctx, llm.GenerateParams{
    Model:    llm.ChatModel("grok-3"),
    Messages: []llm.Message{llm.UserMessage("你好")},
})
```
