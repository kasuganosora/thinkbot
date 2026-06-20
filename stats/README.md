# stats — 用量统计

记录和查询 Bot 的 Token 使用量、消息数等运行指标。

## 功能

- **用量记录**：每次 LLM 调用后记录 Token 消耗（输入/输出/总计）
- **日聚合**：按天聚合统计，支持按 Bot/用户/模型维度查询
- **API 暴露**：通过 REST API 提供统计数据查询

## 关键类型

| 类型 | 说明 |
|------|------|
| `Recorder` | 用量记录器 |
| `Repository` | 统计数据仓储 |
| `Overview` | 统计概览快照 |

## 使用示例

```go
recorder := stats.NewRecorder(db)
recorder.Record(ctx, stats.UsageRecord{
    BotID:    "bot-1",
    Model:    "gpt-4o",
    InputTokens:  150,
    OutputTokens: 80,
})
overview := recorder.Overview(ctx)
```
