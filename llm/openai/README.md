# openai — OpenAI Provider 实现

OpenAI API 的 `llm.Provider` 实现，兼容所有遵循 OpenAI 协议的第三方供应商（DeepSeek、Moonshot、SiliconFlow、Ollama 等）。

## 功能

- 实现 `llm.Provider` 接口（`DoGenerate` / `DoStream`）
- 支持 Responses API 和 Chat Completions API 两种模式
- 支持模型列表查询、音频（TTS/STT）
- 内置重试看门狗（retry watchdog）和流式 SSE 解析
- 通过 `WithBaseURL` 一键兼容第三方供应商

## 关键类型

| 类型 | 说明 |
|------|------|
| `Client` | OpenAI API 客户端 |
| `Option` | 函数式配置选项 |

## 使用示例

```go
prov := openai.New(
    openai.WithAPIKey("sk-xxx"),
    openai.WithBaseURL("https://api.deepseek.com"),
    openai.WithChatMode(), // 使用 Chat Completions 端点
)

result, err := prov.DoGenerate(ctx, llm.GenerateParams{
    Model:    llm.ChatModel("deepseek-chat"),
    Messages: []llm.Message{llm.UserMessage("你好")},
})
```
