# bot — 机器人实例与多 Bot 管理

在 Engine 内核之上叠加业务能力的高级抽象：多 Channel 管理、Handler 自动注册、EventBus 集成、持久化工作空间和 SOUL.md 人格加载。

## 功能

- `Bot` 组合 Engine，自动注册内建 Handler 到 `MultiDispatcher`：`ChannelReplyHandler`（Reply/Forward/Broadcast）、`NoteHandler`、`CallbackHandler`、`SilentHandler`
- `BotManager` 管理多 Bot 的注册、启停和状态查询
- `Channel` / `Sender` 双向通信接口（输入 + 输出）
- `BotConfig` / `AgentConfig` 分层配置（基础设施 + 行为参数）
- `LLMBundle` 从配置构建多层级 LLM 实例集（主力/轻量/多模态）
- 持久化工作空间（文件操作 + SOUL.md 热重载；docker 后端经 `NewWorkspaceSoulStore` 直读容器 named volume，`NewSoulTool` 注册 `soul` 工具供 Bot 自读/自改人格文件）
- 技能系统装配（`SetupSkills` 组合根）
- 梦境巩固子系统（`DreamingBundle` 按 Bot 独立配置，cron 调度定时整理记忆）
- LLM 实例集构建（`CreateLLMBundle` / `CreateProvider`，按 `bot.<id>.main|light|vision` 装配 Provider，Light 未配置时回退 Main）
- 自适应 Engagement 组件注入（`AdaptiveSyncer` / `OutreachBreaker`）：`BotParams.OutreachBreaker` 非空时，`Bot.New` 把它接到 `ChannelReplyHandler.SetOnSent`，仅发送成功才记一次主动出价。入站硬拦在 `EngagementStage`（同一实例，由 `api/botservice.go` 注入）
- per-bot 浏览器 MCP（docker 后端 + `BrowserEnabled` 时，工具命名 `browser__<tool>`，cookie 与 Web 面板双向同步）
- `MemoryChannel`：内存双向 Channel（测试用，`NewMemoryChannel` + `Inject`/`SentActions`）
- `bot.Module` fx 模块：提供 `BotManager` 并绑定生命周期（`OnStart` 启动全部 Bot、`OnStop` 停止），`ProvideBot` 辅助注册

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
| `BotMetrics` | Bot 运行指标快照（`Metrics()` 返回，含 Engine 指标 + 派发错误数） |
| `MemoryChannel` | 内存双向 Channel（测试用） |

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
    modelName,        // LLM 模型名（从 bot 主模型/经济模型读取）
    location,         // 时区
    tp,               // TracerProvider
    logger,           // 日志
    botID,            // Bot ID
    cronFilePath,     // cron Job 持久化路径
    db,               // *gorm.DB（SQLite 持久化，重启可恢复）
)

// Bot.Run 中启动调度器
bundle.Scheduler.Start(ctx)

// Bot 关闭时优雅停止
defer bundle.Stop()
```

| 组件 | 说明 |
|------|------|
| `DreamManager` | 三相位记忆整理管线（Light → REM → Deep） |
| `DreamExecutor` | cron.Executor 实现，触发 DreamManager.Run() |
| `Scheduler` | cron 调度器，按 `dreamCfg.Schedule` 定时触发 |
| `CronStore` | cron Job 持久化（JSON 文件） |
| `TieredMgr` / `TieredStore` | 独立的分层记忆管理器/存储（梦境管线专用，SQLite 持久化） |
