# session — 对话会话管理

Session 代表一个连续的对话上下文，填补单条消息处理（Pipeline/Envelope）与长期记忆（Memory）之间的层次空白。

```
┌─────────────────────────────────┐
│  Envelope (单条消息)              │  ← 最短命
├─────────────────────────────────┤
│  Session (当前对话上下文)          │  ← 中等寿命：一个话题/对话链
├─────────────────────────────────┤
│  Memory (长期记忆 L0~L3)          │  ← 长期：跨对话
└─────────────────────────────────┘
```

## 功能

- **会话实体**：线程安全的 `Session`，维护最近 N 轮对话的工作记忆（FIFO 淘汰）
- **会话解析**：`SessionResolver` 决定消息属于/是否应创建 session，按平台提供不同实现
- **生命周期管理**：`SessionManager` 缓存活跃 session，空闲超时自动归档并触发回调
- **Pipeline 集成**：`SessionStage` 注入上下文，`SessionWriteStage` 回写 Bot 回复
- **串行化执行**：`SessionRunner` 保证同一 session 内的消息串行处理，支持排队上限与取消

## 文件结构

```
session/
├── session.go   # Session 实体 + Message + Option
├── resolver.go  # SessionResolver 各实现 + FormatContext + Envelope 提取辅助
├── manager.go   # SessionManager + SessionStage + SessionWriteStage
└── runner.go    # SessionRunner + SessionRunnerManager（per-session 串行化）
```

## 关键类型

| 类型 | 说明 |
|------|------|
| `Session` / `SessionStatus` | 会话实体（`active` / `archived`），线程安全 |
| `Message` | 工作记忆中的一条记录（Role/Text/UserID/Timestamp） |
| `Option` | `WithMaxMessages` / `WithCreatedBy` |
| `SessionResolver` / `ResolveResult` | 会话解析接口与结果（SessionID/OK/CreatedBy） |
| `SessionManager` / `ManagerConfig` | 会话生命周期管理 |
| `SessionStage` / `StageConfig` | Pipeline Stage：解析 session 并注入上下文 |
| `SessionWriteStage` | Pipeline Stage：将 Bot 回复写回 session |
| `SessionRunner` / `RunnerConfig` | per-session 串行执行器 |
| `SessionRunnerManager` | 按 sessionID 管理 Runner 实例 |

## Session 实体

```go
s := session.NewSession("sess-1", "bot1", "chat-123",
    session.WithMaxMessages(20),
    session.WithCreatedBy("user"),
)

s.AppendMessage(session.Message{Role: "user", Text: "hi", UserID: "u1"})
recent := s.RecentMessages(10)
```

访问器：`ID` / `BotID` / `Channel` / `Topic` / `SetTopic` / `Status` / `IsActive` /
`StartedAt` / `LastActivityAt` / `CreatedBy` / `MessageCount` / `Messages` / `IdleDuration`。

上下文操作：
- `Clear()` —— 清空工作记忆但保留会话本身（用于 `/clear`）
- `Compact(keepRecent)` —— 只保留最近 N 条，`<=0` 时默认保留 3 条（用于 `/compact`）
- `Archive()` —— 标记为已归档

## 会话解析

| Resolver | 策略 |
|----------|------|
| `DefaultResolver` | `reply_id` → `{prefix}:thread:{id}`；`Mentioned` → `{prefix}:channel:{ch}`；否则 `OK=false` |
| `TelegramResolver` | 每个 chat 一个 session → `tg:{channel}`，恒 `OK=true` |
| `MisskeyResolver` | 回复链 → `mk:thread:{replyID}`；被 @ → `mk:channel:{ch}`；时间线原创帖 `OK=false` |
| `ChannelResolver` | 按 `Message.Source` 路由到已注册的子解析器，未注册回退 `DefaultResolver` |
| `NeverResolver` | 恒 `OK=false`（RSS 等纯信息流） |

注意：`reply_target` metadata **不用于** session 解析 —— 它是 outbound 回复目标标识，
每条消息都有；只有 `reply_id` 才表示本消息参与了对话链。

## SessionManager

```go
mgr := session.NewSessionManager(
    session.NewTelegramResolver(),
    session.DefaultManagerConfig(), // MaxMessages=20 / IdleTimeout=30m / SweepInterval=5m
    tp, logger,
)

mgr.OnArchive(func(s *session.Session) {
    // 归档时将工作记忆精华写入长期 Memory
})

stop := mgr.StartSweeper(ctx)
defer stop()
```

API：`GetOrCreate(sessionID, botID, channel, createdBy)`、`Get`、`ActiveCount`、
`Archive`、`Sweep`、`StartSweeper`、`Resolve`。
`Archive` 先从活跃 map 移除再触发回调，避免并发获取到已归档 session。

## Pipeline 集成

`SessionStage` 建议放在靠前位置（如 Order=50，在 MemoryStage 之前），
`SessionWriteStage` 放在靠后（如 Order=850，ReplyStage 之后、MemoryWriteStage 之前）。

```go
readStage := session.NewSessionStage("session", mgr, session.DefaultStageConfig(), tp, logger)
writeStage := session.NewSessionWriteStage("session_write", mgr, tp, logger)
```

**SessionStage 写入的 Envelope KV**：
`session.active`、`session.id`、`session.is_new`、`session.message_count`、
`session.context`（由 `FormatContext` 生成，`ContextMaxMessages` 默认 10 条）。
解析结果 `OK=false` 时仅设置 `session.active=false` 并放行。

**SessionWriteStage**：从 Envelope 的 `ActionReply` 中提取 string payload，
以 `assistant` 角色追加到对应 session；无 session 或无回复文本时跳过。

**旁路事件**：`SessionStage` 通过 `outbound.EmitterFromContext(ctx)` 发射 `session.resolved`。

**Envelope 提取辅助**：`SessionIDFromEnvelope`、`SessionContextFromEnvelope`、
`IsNewSessionFromEnvelope`。

## 串行化执行

保证同一 session 内的消息串行处理，避免并发竞争。

```go
runners := session.NewSessionRunnerManager(session.DefaultRunnerConfig()) // MaxQueueDepth=32
runner := runners.GetOrCreate(sessionID)

// 阻塞排队执行；队列满时返回 ErrSessionBusy
err := runner.Run(ctx, func(ctx context.Context) error {
    return processMessage(ctx, msg)
})

// 非阻塞：忙时立即返回 ErrSessionBusy
err = runner.TryRun(ctx, fn)
```

状态机：`RunnerStateIdle` ⇄ `RunnerStateBusy`。
`acquire` 基于 channel 等待（而非 `sync.Cond`），因此支持 context 取消。

Runner API：`State`、`QueueDepth`、`IsBusy`、`IsIdleAndEmpty`、`Cancel`。
Manager API：`GetOrCreate`、`Get`、`Delete`、`ActiveRunners`、`BusyRunners`、
`Cleanup`（清理空闲且无排队的 Runner）。

错误：`ErrSessionBusy`、`ErrSessionCancelled`。
