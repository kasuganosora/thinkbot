# agent/heartbeat — Per-Bot 自主唤醒模块

心跳（heartbeat）给每个 bot 一个周期性「被触发」的机会：让 bot 自己审视记忆、待办与关注项，决定此刻是否有需要主动处理的事——有事就做（发帖 / 记笔记），没事就安静结束。它**不是**健康监视器：bot 是行动主体，心跳只是唤醒信号。

设计与演进背景见 [`docs/heartbeat-redesign.md`](../../docs/heartbeat-redesign.md)。

## 职责

- **定时唤醒**：复用 `cron.Scheduler` + `cron.Executor` 模式（与 Dreaming 同构），按 `Config.Interval` 周期触发。
- **走完整编排链路**：唤醒消息进入与 `@bot` 完全相同的 pipeline（工具 / 记忆 / SOUL 全在线），而不是独立的"健康检查 LLM"。
- **三级节制**，防止 bot 自激刷屏、白烧 token：
  1. **准入关卡**（Admission Guard）：自上次唤醒以来无新信号（无新消息 / 新笔记）→ 0-step 直接结束，不消耗主 LLM 调用；但连续拒绝达到 `IdleWakeEvery` 次时强制放行一次（时间本身也是信号）。
  2. **两级发言闸门**：平台策略（`AllowPost` / `AllowPostFn`，任一 false 即压制）优先于 bot 自主决策。
  3. **连续唤醒硬频控**：连续「产生行动的唤醒」超过 `MaxConsecutiveWakes` 或处于冷却窗内 → 降级为不发言；真实外部消息会立即恢复预算（`NotifyUserActivity`）。

## 一次唤醒的生命周期（`Executor.Execute`）

```
读取配置（interval / allow_post / 频控 / 准入）
  → 频控判定（连续上限 + 冷却窗，超阈降级 degraded）
  → 准入关卡（无信号 → 记 0-step 日志后返回；达上限强制放行）
  → 构造唤醒消息（Text 留空，提示词走 InjectContext，不污染 L0 记忆）
  → 设置 Envelope KV（心跳决策模式 + 恒 SuppressReply）
  → ProcessSync 进入真实编排（pipeline + dispatcher 全链路）
  → 从 llm.result 解析结构化决策（解析失败一律安全降级 silent）
  → 按决策路由（post / note / silent，含压制记录）
  → 更新频控计数 → 落一条心跳日志
```

安全原则贯穿始终：**宁可多睡，不可误发**。解析失败、目标渠道不在可发列表、内容为空、发帖器缺失，全部降级为 silent。

## 核心类型与函数

### 配置与决策

| 类型 / 函数 | 说明 |
|---|---|
| `Config` / `DefaultConfig()` | 心跳配置：`Enabled`、`Interval`（分钟，1–1440，默认 30）、`AllowPost`（默认 false）、`MaxConsecutiveWakes`（默认 3）、`CooldownMin`（默认 0=退化为心跳周期）、`IdleWakeEvery`（默认 4，设 1 即关闭准入关卡） |
| `HeartbeatDecision` | LLM 返回的结构化决策：`Decision`（`silent` / `post` / `note`）+ 目标渠道 / 会话 / 内容 / 理由 |
| `ChannelTarget` | 一个可主动发帖的真实目标（渠道名、类型、平台会话 ID、给 LLM 看的 Label） |
| `Log` / `LogStore` | 单条心跳日志与日志文件结构；`Status` 五态：`acted` / `note` / `silent` / `suppressed` / `error` |

决策是**结构化 JSON 契约**（用确定枚举代替自由文本「静默」表达）：

```json
{"decision": "post", "channel": "Misskey", "conversation_id": "",
 "content": "要发的内容", "reason": "为什么"}
```

`extractJSON` 对 LLM 的已知畸形输出做容错抽取（markdown 围栏、尾随逗号、顶层数组 / 并列对象按「先出现者优先」合并），尽量避免整轮决策被静默丢弃。

### 能力注入（解耦外部依赖）

| 类型 | 说明 |
|---|---|
| `TriggerRunner`（接口） | 触发真实编排的能力（由 `*agent.Engine` 实现），避免对 agent 根包的循环 import。必须走 `ProcessSync` 完整链路（pipeline + dispatcher），只跑 pipeline 会让 bot「想了但什么都没落地」 |
| `AdmissionFn` | 准入信号探测：`(ctx, since) → (是否有信号, 描述)` |
| `ChannelLister` / `ChannelPoster` | 枚举 / 投递真实发帖目标。**绕过伪频道 `"heartbeat"` 的 dispatcher**（历史 Bug 根因：`no sender for channel "heartbeat"`） |
| `NoteSaver` | 把决策的内部笔记写入本 bot 长期记忆（复用 `ActionNote` 链路，bot 全局 scope，可跨渠道召回） |

### Executor 与 Bundle

- `NewExecutor(ExecutorConfig)`：创建心跳执行器（实现 `cron.Executor` 接口）。频控状态为纯内存态（单 bot 串行触发，重启重置可接受）。
- `Executor.NotifyUserActivity()`：通知「有真实外部消息进来了」，立即重置连续唤醒预算。挂在每条入站消息路径上，必须廉价。
- `NewBundle(BundleConfig)`：封装完整子系统（Executor + Scheduler + Store）。配置 disabled 时返回 nil；内部会清理同名残留 cron job，防止跨重启累积多个同名 job 导致同一心跳被触发 N 次。`Runner` 可为 nil，稍后 `SetRunner` 注入。

典型接线（见 `api/botservice.go`）：

```go
hb := heartbeat.NewBundle(heartbeat.BundleConfig{
    BotID:         id,
    Store:         s.heartbeatStore, // 复用外部 Store，共享同一把 per-bot 锁
    Location:      loc,
    Logger:        s.logger,
    AdmissionFn:   s.newHeartbeatAdmissionFn(id), // 新消息/新笔记探测
    ChannelLister: s.heartbeatChannelLister(id),
    ChannelPoster: s.heartbeatChannelPoster(id),
    NoteSaver:     s.heartbeatNoteSaver(id),
})
// …bot 构建完成后：
hb.SetRunner(b.Engine()) // Engine 在 bot.New 内部创建 → 构建顺序倒挂，必须后置注入
hb.Start(ctx)            // 必须在 SetRunner 之后，否则首次心跳以 runner=nil 失败
```

## 与存储的关系（store.go）

`Store` 是心跳配置与日志的文件系统存储，线程安全（`sync.Map` 维护 per-bot 互斥锁）：

```
data/heartbeat/{botId}/config.json   — 心跳配置（LoadConfig / SaveConfig）
data/heartbeat/{botId}/logs.json     — 心跳日志（LoadLogs / AppendLog / ClearLogs）
data/heartbeat/{botId}/.cron.json    — cron 调度器状态（CronFilePath，交给 cron.Store）
```

- **滚动窗口**：`AppendLog` 头部插入（最新在前），超过 `MaxLogEntries = 200` 条时删除最老条目；`LogStore.Total` 保留历史总数（含已滚出的）。
- 写入为「读-改-写整文件」（`MkdirAll` + `MarshalIndent` + `0644`），因此 API Server 与 Executor 必须共享同一个 `Store` 实例，保证同一把 per-bot 锁串行化并发写。
- 配置在每次 `Execute` 时重新加载，改动无需重启即生效。

## 与编排链路的握手（core KV）

心跳通过 `core.Envelope` 的 KV 与 pipeline 各 stage 协作：

| KV | 语义 |
|---|---|
| `KVHeartbeatMode` | 驱动 `LLMStage` 强制 JSON 结构化输出（决策契约） |
| `KVHeartbeatTargets` | 携带本次可发帖目标列表，供 LLM 决策选择 |
| `KVSuppressReply`（恒 true） | 心跳决策的真实发帖由 Executor 经 `ChannelPoster` 手动路由，绝不走伪频道 `"heartbeat"` 的通用 dispatcher |
| `KVSuppressReplyReason` | 压制原因：`platform_policy` / `frequency_cap` |

消息契约：`Source = core.SourceHeartbeat`、`Channel = "heartbeat"`（独立会话空间）、`UserID = "system:heartbeat"`、**`Text` 留空**（避免被 note_capture 当用户原文写入 L0 记忆），唤醒提示走 `InjectContext` 通道，并携带 `TraceID`。

## 可观测性

架构级不变量：**每一次心跳唤醒都必须落一条日志**——包括被准入关卡拒绝的 0-step turn（`Admitted=false`）——保证心跳不是黑盒，可完整复盘。日志字段语义：

- `Status`：本次唤醒做了什么（`acted`/`note`/`silent`/`suppressed`/`error`），而非健康状态。
- `Admitted`：是否真正进入编排（false = 0-step，未消耗主 LLM）。
- `Reason`：**仅** `suppressed` 时有值（`platform_policy` / `frequency_cap`）；主动静默 / 发帖成功 / 记笔记时不带，避免「bot 主动静默」被误读为「被平台拦下」。
- `Decision` / `Target`：结构化决策与选定的发帖目标（`suppressed` 时即「想发到 X 但被拦」）。
- `TraceID`：关联 trace，串起 pipeline 内部日志排查。

zap 日志（`component=heartbeat_executor`, `bot_id`）覆盖关键节点：`heartbeat admitted`（含信号描述）、`heartbeat rejected by admission guard`、`heartbeat completed`（status / decision / cost / degraded / reason / target / summary / consecutive_wakes）、决策 JSON 解析失败 Warn（含截断后的 LLM 原文，防止笔记内容彻底丢失）、发帖 / 记笔记 / 落日志失败 Warn。`Result` 字段是人类可读的行动摘要，直接供前端心跳面板展示。

## 测试

`heartbeat_test.go` 用 `fakeRunner`（把预设 JSON 注入 `env.llm.result`）覆盖：消息契约、两级闸门与频控、准入关卡（含强制放行）、决策解析失败 / 非法目标的安全降级、笔记落库（含失败不阻断、空内容不调用）、`NotifyUserActivity` 预算恢复、Store 配置与日志的 JSON 往返、`extractJSON` 畸形形态容错。

```bash
go test ./agent/heartbeat/
```
