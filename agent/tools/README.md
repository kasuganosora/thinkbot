# tools — Bot 工具管理

注册、管理和执行 Bot 可用的工具（Tool/Function Call），支持工具策略控制、提示词生成和流水线集成。

## 功能

- **工具注册**：通过 `ToolManager` 注册工具，自动生成 JSON Schema 描述
- **策略控制**：基于 Pattern 的允许/拒绝策略（`PatternPolicy`），限制工具使用范围
- **提示词生成**：自动将工具描述拼接为系统提示词段落
- **流水线集成**：`ToolStage`（Order=30）在 Pipeline 中注入工具能力

## 关键类型

| 类型 | 说明 |
|------|------|
| `ToolManager` | 工具注册中心 |
| `Policy` | 工具使用策略接口 |
| `PatternPolicy` | 基于 glob pattern 的允许/拒绝策略 |
| `ToolStage` | Pipeline Stage（Order=30） |
| `ToolPromptSection` | 工具提示词段落 |

## 使用示例

```go
mgr := tools.NewToolManager(logger)
mgr.Register("calc", "计算器", calcSchema, calcExecute)

policy := tools.NewPatternPolicy().
    Allow("calc").
    Allow("web_search").
    Deny("file_delete")
mgr.SetPolicy(policy)
```
