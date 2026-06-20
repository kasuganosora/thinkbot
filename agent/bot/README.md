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
- 梦境巩固子系统（`DreamingBundle` 按 Bot 独立配置，cron 调度定时整理记忆）

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
| `DreamingBundle` | 梦境巩固子系统封装（DreamManager + cron Scheduler） |
| `DreamExecutor` | cron.Executor 实现，桥接 cron 触发和 DreamManager.Run() |

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

## 梦境巩固子系统

`DreamingBundle` 封装了完整的梦境巩固流水线组件，按 Bot 独立配置：

```go
// 从 config 构建梦境配置
dreamCfg := builder.GetDreamingConfig(botID)
dreamCfg.Enabled = true

// 创建子系统（Enabled=false 时返回 nil）
bundle := bot.NewDreamingBundle(
    dreamCfg,         // memory.DreamConfig
    llmProvider,      // LLM 提供商
    location,         // 时区
    tp,               // TracerProvider
    logger,           // 日志
    botID,            // Bot ID
    cronFilePath,     // cron Job 持久化路径
)

// Bot.Run 中启动调度器
bundle.Scheduler.Start()

// Bot 关闭时优雅停止
defer bundle.Stop()
```

| 组件 | 说明 |
|------|------|
| `DreamManager` | 三相位记忆整理管线（Light → REM → Deep） |
| `DreamExecutor` | cron.Executor 实现，触发 DreamManager.Run() |
| `Scheduler` | cron 调度器，按 `dreamCfg.Schedule` 定时触发 |
| `CronStore` | cron Job 持久化（JSON 文件） |
| `TieredMgr` | 独立的分层记忆管理器（梦境管线专用） |
