# prompt — 系统提示词构建

从多个来源（Soul 文件、工具定义、记忆上下文、会话历史）组合构建发送给 LLM 的系统提示词。

## 功能

- **Soul 加载器**：从 `SOUL.md` 文件加载 Bot 的人格设定，支持热重载（文件变更自动刷新）
- **提示词组装**：按优先级和顺序拼接多个 `ToolPromptSection`（工具说明段落）
- **模板变量**：支持 `{{.BotID}}`、`{{.Channel}}` 等模板变量替换
- **配置覆盖**：支持通过配置键覆盖默认系统提示词

## 关键类型

| 类型 | 说明 |
|------|------|
| `Loader` | 提示词加载器，组装最终 SystemPrompt |
| `SoulLoader` | SOUL.md 文件加载器（带文件监听 + 热重载） |
| `ToolPromptSection` | 工具提示词段落（Name/Content/Order） |

## 文件结构

```
prompt/
├── prompt.go    # Loader + ToolPromptSection 定义
├── loader.go    # 提示词组装逻辑
├── soul.go      # SOUL.md 热重载加载器
└── stage.go     # Pipeline Stage（Order=20）
```
