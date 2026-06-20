# google — Google Gemini Provider 实现

Google Gemini API 的 `llm.Provider` 实现。

## 功能

- 实现 `llm.Provider` 接口（`DoGenerate` / `DoStream`）
- 支持 `generateContent` 和 `streamGenerateContent` 端点
- 支持函数调用（Function Calling），含思维签名
- 支持多模态输入（文本、图片、音频）
- 支持模型列表查询、文件上传、Token 计数
- 隐式前缀缓存（由 Gemini API 自动管理）

## 关键类型

| 类型 | 说明 |
|------|------|
| `Client` | Gemini API 客户端 |
| `Option` | 函数式配置选项 |

## 使用示例

```go
prov := google.New(
    google.WithAPIKey("AIza-xxx"),
)

result, err := prov.DoGenerate(ctx, llm.GenerateParams{
    Model:    llm.ChatModel("gemini-2.0-flash"),
    Messages: []llm.Message{llm.UserMessage("你好")},
})
```
