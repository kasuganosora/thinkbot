# bot — 机器人实例与多 Bot 管理

在 Engine 内核之上叠加业务能力的高级抽象：多 Channel 管理、Handler 自动注册、EventBus 集成、持久化工作空间和 SOUL.md 人格加载。

## 功能

- `Bot` 组合 Engine，自动注册内建 Handler（Reply/Note/Callback/Silent）
- `BotManager` 管理多 Bot 的注册、启停和状态查询
- `Channel` / `Sender` 双向通信接口（输入 + 输出）
- `BotConfig` / `AgentConfig` 分层配置（基础设施 + 行为参数）
- `LLMBundle` 从配置构建多层级 LLM 实例集（主力/轻量/多模态）
- 持久化工作空间（文件操作 + SOUL.md 热重载）
- 技能系统装配（`SetupSkills` 组合根）

## 关键类型

| 类型 | 说明 |
|------|------|
| `Bot` | 独立机器人实例，组合 Engine + Channel + Handler |
| `BotParams` | Bot 构造参数 |
| `BotConfig` | 基础设施配置（Workers/Model/Temperature 等） |
| `AgentConfig` | 行为配置（MaxSteps/ToolAllowlist/SystemPromptOverride 等） |
| `BotManager` | 多 Bot 生命周期管理器（线程安全） |
| `Channel` / `Sender` | 输入端 / 输出端接口 |
| `LLMBundle` | LLM 实例集（Main/Light/Vision） |

## 使用示例

```go
mgr := bot.NewBotManager(logger, tp)

myBot, _ := bot.New(bot.BotParams{
    ID:         "customer-service",
    Config:     bot.BotConfig{SystemPrompt: "你是客服"},
    Pipeline:   pipeline,
    Dispatcher: dispatcher,
    Channels:   []bot.Channel{misskeyCh, telegramCh},
    Logger:     logger,
    TP:         tp,
})
_ = mgr.Register(myBot)
mgr.RunAll(ctx)
mgr.StopAll()
```
