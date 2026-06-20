# stages — 内建 Pipeline Stage

提供开箱即用的 Pipeline Stage 实现，覆盖消息处理的核心环节。

## 功能

- **LLMRoute**：根据消息内容路由到不同 LLM 模型
- **Enricher**：消息预处理（用户信息注入、历史加载）
- **Multimodal**：多模态消息处理（图片/音频/视频附件）
- **Reply**：LLM 调用 + 回复生成（核心 Stage）
- **Filter**：消息过滤（黑名单/白名单/长度检查）
- **Logger**：请求/响应日志记录

## 关键类型

| 类型 | 说明 |
|------|------|
| `LLMStage` | LLM 调用 Stage（支持流式输出） |
| `LLMConfig` | LLM Stage 配置（SystemPrompt/Model/Temperature） |
| `EnricherStage` | 消息预处理 Stage |
| `MultimodalStage` | 多模态附件处理 Stage |
| `ReplyStage` | 回复发送 Stage |
| `FilterStage` | 消息过滤 Stage |

## 使用示例

```go
llmStage := stages.NewLLMStage("llm", provider, stages.LLMConfig{
    SystemPrompt: "你是一个助手",
    Model:        "gpt-4o",
    Temperature:  0.7,
})
pipeline.Register(pipeline.StageEntry{
    Stage: llmStage,
    Info:  core.StageInfo{Name: "llm", Order: 100, Enabled: true},
})
```
