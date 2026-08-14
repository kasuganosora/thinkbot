# 心跳模块重设计：从 health-check 到「自主唤醒」

> 状态：设计稿（待评审）
> 提出方：露娜（架构纠偏）
> 日期：2026-08-14

---

## 1. 问题诊断：当前是 monitoring / health-check 范式

当前 `agent/heartbeat` 把「心跳」实现成了**健康监视器**，而不是「给 bot 被触发的机会」。铁证（均来自代码）：

| # | 现象 | 位置 |
|---|------|------|
| 1 | `ContextProvider` 只暴露**只读**指标：`RecentMessageCount` / `ChannelStatus` / `PendingMessageCount`，全是「看 bot 是不是活着」 | `agent/heartbeat/heartbeat.go:55-64` |
| 2 | `Executor.Execute` 调 `provider.DoGenerate` **不带任何 Tools**，bot 无法行动，只能吐一段给人看的文字 | `agent/heartbeat/heartbeat.go:159` |
| 3 | system prompt 自定位为 *"a monitor that periodically assesses the bot's runtime health"*，产出 `normal` / `ALERT` | `agent/heartbeat/heartbeat.go:216` |
| 4 | `Log.Status` 只有 `normal` / `alert` 两档，前端做成**健康/告警面板** | `agent/heartbeat/heartbeat.go:46` |
| 5 | 与 bot 真实能力（工具 / 记忆 / SOUL / 出站）**完全隔离** —— 心跳时 bot 是被观察的客体，不是行动主体 | 整包 |

而 bot 正常被 @ 触发走的是另一条路：`bot.go:49` 注释 `→ pipeline.Execute(ctx, env)`，经 `agent.Engine` + `Pipeline` + `Dispatcher`，**工具/记忆/人格全在线**。心跳本该复用这条路径，却自己另起炉灶搞了个阉割版 LLM 调用。

**后果**：心跳只能「报平安 / 报异常」，bot 既不能主动做事，也无法基于自己的记忆和待办产生任何自主行为。这违背了「bot 是一个能自己行动的主体」的设计前提。

---

## 2. 设计目标：心跳 = 周期性「自主唤醒」

> 心跳不是检查 bot 是否健康，而是**按节奏给 bot 一个被触发的机会**，让 bot 自己看看有什么事做（主动找事做，或判断没啥事做就静默结束），**由 bot 自己决定**。

系统只负责「按节奏敲门」，不替 bot 做判断、不让 bot 只是个被监控对象、不强行要求产出「健康状态」。

**架构级不变量（借鉴 deepseek-harness 的 "Model-visible means logged"）**：每一次心跳唤醒——无论 bot 最终发言、记笔记还是静默结束——都必须落一条 **durable 事件**（即一条心跳日志），内容包括「唤醒是否进入 LLM、是否 0 步结束、产生了哪些 Action」。任何进入模型的唤醒上下文都必须可复盘。这保证心跳不会变成黑盒，也便于事后审计「bot 那次醒来到底干了什么 / 为什么没干」。

---

## 3. 整体架构（数据流）

```
cron 到点
  └─→ 构造一条 system-origin 心跳消息
        (Source="heartbeat", UserID="system:heartbeat", Text=唤醒提示)
  └─→ 进入 bot 的真实编排路径  ← 关键：复用，而非独立 LLM 调用
        pipeline.Execute(ctx, env)   // 工具 / 记忆 / SOUL 全在线
  └─→ bot 自省：有啥事做？
  └─→ 两级出站闸门（见 §5）
        ① 平台策略层：allow_post=false / 渠道静默 → KVSuppressReply=true
        ② bot 自主权层：平台允许时，bot 自己决定 发言 / 记笔记 / 静默
  └─→ 记录「本次唤醒做了什么」日志
```

---

## 4. 接入点（已核实，非推测）

- `bot.Ingress().Receive(ctx, core.Message{...})` —— `agent/inbound/ingress.go:106`
  - Ingress 有**自消息过滤** `isSelfMessage(msg.UserID)`（ingress.go:114）。心跳消息 `UserID = "system:heartbeat"`，不在 `selfIDs` 中 → 不会被误丢。
- `pipeline.Execute(ctx, *core.Envelope) (*core.Envelope, error)` —— `agent/pipeline/pipeline.go:96`，完整编排（工具/记忆/SOUL）。
- `core.Message` 字段可直接用：`Source` / `Channel` / `UserID` / `Text` / `Metadata`（`agent/core/envelope.go:13`）。
- 出站语义现成，无需新造：
  - `KVSuppressReply`（envelope.go:89）：标记「本轮不对外发送，但仍正常思考+记记忆」—— 完美对应平台静默但 bot 可内部做事。
  - `ActionReply` / `ActionNote` / `ActionSilent`（envelope.go:128/137/155）：bot 决策产物，直接作为日志内容来源。

---

## 5. 两级发言闸门（露娜决策：平台策略优先，其次 bot 自主）

心跳唤醒时 bot 是否对外发言，由两级决定：

**第一级 · 平台策略（最高优先）**
- 配置 `heartbeat.allow_post`（默认 **false**）；或部署/渠道级静默策略。
- 命中静默 → pipeline 入口 enricher 设 `KVSuppressReply=true`。
- 效果：bot 仍正常走完整 pipeline（思考、调用工具、写 `ActionNote` 记记忆），但**不对外发言**。
- 理由：平台/渠道若要求静默（如公开频道防刷屏、合规），任何 bot 自主权都不能越过。

**第二级 · bot 自主权（仅平台允许时生效）**
- `allow_post=true` 时，不预设 `KVSuppressReply`。
- bot 在唤醒提示词引导下**自主决定**：
  - 输出内容 → `ActionReply`（主动发帖/回复）
  - 只记笔记 → `ActionNote`
  - 啥也不做 → `ActionSilent`
- 提示词约束：「没有重要的事就安静结束，不要为刷存在感发言」。

> 两级都「不发言」时，最终表现一致（静默），但成因不同：平台级是策略压制，bot 级是自主克制。日志需区分（`status=silent` vs 平台抑制标记）。

### 5.5 准入关卡（Admission Guard）—— 借鉴 deepseek 的 `agent/pre-step` reject

deepseek-harness 的唤醒模型里，进入模型前有一道 **`agent/pre-step` 准入瀑布**：监听器可以 `reject` 被认领的消息，或把首条 claim 重写为空；**被 reject / 空 claim 会关闭一个「耗费 0 step 的 turn」，但日志仍记录这次尝试**。

对应到心跳：bot「审视后觉得没啥事做」不该只靠 *prompt 让 LLM 憋一段空输出*（羸弱、不可审计、仍消耗一次 LLM 调用），而应有正式的**准入关卡**：

- 心跳消息进入 pipeline 后、调用 LLM 之前，由一个 Admission Guard 判定「是否有值得开的轮次」（依据：记忆/待办/关注项是否给出行动信号；或干脆把决定权交给一次极轻量的预判断）。
- 判定「无事可做」→ 直接 abort 本轮，落一条 `status=silent`（0-step）日志，**不消耗主 LLM 调用**。
- 判定「有事」→ 正常进入 LLMStage，bot 自主决定后续。

> 这是比「prompt 抑制」更健壮的设计：静默是架构层的确定性决策，不是模型的概率性克制；同时为「每次唤醒都落日志」的不变量提供了干净的落点（0-step turn 也记）。

---

## 6. 数据结构变更

### `heartbeat.Config`
```go
type Config struct {
    Enabled   bool `json:"enabled"`
    Interval  int  `json:"interval"`   // 保留，分钟
    AllowPost bool `json:"allow_post"` // 新增：平台级是否允许心跳主动发言，默认 false
}
```

### `heartbeat.Log`（语义重写）
```go
type Log struct {
    ID      string   `json:"id"`
    Status  string   `json:"status"`  // 重写：acted | silent | note | suppressed | error
    Time    string   `json:"time"`
    Cost    float64  `json:"cost"`
    Actions []string `json:"actions"` // 新增：本次唤醒产生的 Action 类型，如 ["reply"] ["note"] []
    Result  string   `json:"result"`  // 重写：行动摘要（bot 做了什么 / 为何静默），非健康描述
}
```
- 旧 `normal` / `alert` 字段：标记 deprecated，旧日志在加载时做一次性迁移（或忽略旧条目）。

---

## 7. 唤醒提示词（心跳消息 `Text` 草稿）

```
这是一次系统自主心跳唤醒（heartbeat），不是用户发来的消息。
请审视你的长期记忆、待办事项、关注的人与话题，判断此刻是否有需要你主动处理的事
（例如：跟进未完成的任务、回复重要的人、发布你一直想发的观察、检查某个状态）。
如果你判断当前没有值得主动处理的事，请安静结束，不要为了刷存在感而发言。
注意：你的任何对外输出都可能被公开发布，请自行判断其重要性。
```

> **注入方式修正（关键，借鉴 deepseek 的 `agent.inject()`）：唤醒提示词走独立「注入上下文」通道，而非拼进 `core.Message.Text`。**
> 原因：若拼进 `Text`，该自然语言心跳会被 `note_capture` 当作用户入站原文标 `speaker:user` 写入 **L0 记忆**，污染记忆层（心跳不该成为「记忆」的一部分）。独立注入通道让唤醒提示词对模型可见、但不进记忆/对话历史。pipeline 需为心跳消息提供 `InjectContext` 字段承载该提示，并使其不参与 `note_capture`。

---

## 8. 组件改造清单

| 文件 | 改动 |
|------|------|
| `agent/heartbeat/heartbeat.go` | `Executor` 不再持有 `provider`+`ctxProv` 做独立 health LLM；改为持有 bot/engine 引用或 `pipeline.Execute` 能力。`Execute` 构造心跳消息 → 进入真实编排 → 收集 `env.Actions()` 生成 `Log`。移除 `heartbeatSystemPrompt`（monitor 文案）。 |
| `agent/heartbeat/store.go` | `Log` 结构语义重写（§6）；旧日志迁移处理。 |
| `agent/bot/bot.go` | `HeartbeatScheduler` 的 executor 改为接到真实 engine/pipeline；提供心跳消息构造 helper（含 `Source="heartbeat"`、`UserID="system:heartbeat"`）。 |
| `agent/pipeline/*`（或 enricher） | 识别 `Source=="heartbeat"` / `Metadata["heartbeat"]==true`，**跳过 engagement 的「是否 @提及才处理」门控**，直接进入 bot 自主处理；按 §5 注入平台级 `KVSuppressReply`。 |
| `api/handler_bot_heartbeat.go` | 日志查询/配置接口适配新语义（新增 `allow_post` 配置项）。 |
| `web/src/api/services.js` | 心跳配置/日志类型适配。 |
| `web/src/components/bot/BotHeartbeat.vue` | 从「健康/告警面板」改为「bot 自主行动日志」展示（状态色：acted/silent/note/suppressed/error）。 |

---

## 9. 关键技术点 / 风险

### 9.1 触发方式：直接 `pipeline.Execute` 还是走 `Ingress`？
- **推荐：直接 `pipeline.Execute(ctx, env)`（同步）**。
  - 心跳是程序化内部触发，Ingress 的归一化（自消息过滤、trace 分配）对心跳不必要；走 Ingress 会变成异步投递，Executor 难以同步拿「本次做了什么」。
  - 直接调用 `pipeline.Execute` 同步返回 `*core.Envelope`，可立即读 `env.Actions()` 生成日志，最干净。
  - 需在 pipeline 内对 `Source=="heartbeat"` 做特殊放行（绕过 engagement 的 @提及 判定），而不是靠 Ingress 路径。
- 备选（方案 A）：走 `Ingress().Receive` + 回调收集 Actions。复杂度高，仅在 pipeline.Execute 非同步（如强依赖 worker 流式）时采用。**需实施时核实 `pipeline.Execute` 是否同步。**

### 9.2 自触发循环
- 心跳若让 bot 发帖，帖子经 channel 出站，会被 Ingress 自消息过滤拦下（UserID 匹配 selfIDs）→ 不会回灌成新消息触发下一轮。✅ 天然安全。
- 但仍需确保：心跳唤醒本身不被「bot 发出公开帖」这类出站动作再次触发（心跳只由 cron 触发，与 outbound 解耦，✅ 无此问题）。

### 9.3 防刷屏 / 防自激失控 —— 借鉴 deepseek 的 `maxConsecutiveWakes`

deepseek-harness 的后台任务唤醒有硬频控：`maxConsecutiveWakes`（默认 3）——一个 owner 连续被唤醒开启的轮数超过阈值后，后续通知**降级为注入（不再新开轮）**；且「自激链」是设计内生的风险（被唤醒的一轮可能启动某个任务，其完成又唤醒同一 owner）。

心跳同样存在自激风险：心跳唤醒 → bot 发言/启动某动作 → 该动作本身又可能被当成「新事件」再次激活 pipeline。因此防刷屏必须是**硬机制，不能只靠 prompt 克制**：

- **连续唤醒上限 `maxConsecutiveWakes`（新增配置，默认 3）**：同一 bot 在冷却窗内连续心跳唤醒若多次产生对外行动，超过阈值后后续心跳**降级为纯 inject（不新开 LLM 轮 / 不发言）**，直到有真实用户消息（或冷却窗过期）重置预算。
- **冷却窗 `heartbeat.cooldown_min`（新增）**：两次「产生行动的唤醒」之间的最小间隔；违反则本帧心跳跳过。
- `allow_post=false` 仍是最高优先闸门（连 inject 都不发言）。
- 本期据此把原「频控留作可选增强」**升级为必备项**（见 §11）。

### 9.4 日志量与观测
- 默认 30min 一次，日志量可控；保留滚动窗口（`MaxLogEntries`）。
- **日志必须记录 bot 实际做了什么**（Actions + 摘要），否则心跳变成黑盒 —— 符合「可观测性默认 info 级」纪律。

---

## 10. 实施步骤（分阶段）

- **阶段 0 — 数据结构**：`Config.AllowPost`、`Log` 新语义、`core.Message` 心跳元数据约定。
- **阶段 1 — 执行路径**：`Executor` 改走 `pipeline.Execute`（§9.1 推荐方案），收集 `env.Actions()` 写日志；移除 monitor 文案。
- **阶段 2 — 两级闸门 + 提示词**：pipeline enricher 注入平台级 `KVSuppressReply`；落地 §7 唤醒提示词。
- **阶段 3 — API + 前端**：配置/日志接口与 `BotHeartbeat.vue` 适配新语义。
- **阶段 4 — 测试**：
  - 构造心跳消息 → 验证 bot 能调用工具/检索记忆（不再是「吐一段描述」）。
  - `allow_post=false` → 验证不对外发言、但记忆/笔记正常写入。
  - `allow_post=true` + 无事可做 → 验证 `ActionSilent`（静默、无刷屏）。
  - `allow_post=true` + 有事做 → 验证 `ActionReply` 正常出站。

---

## 11. 已决策 / 待决策

| 项 | 结论 |
|----|------|
| 方向 | health-check → 自主唤醒（露娜拍板）✅ |
| 发言权 | 平台策略优先，其次 bot 自主（露娜拍板）✅ |
| 与正常触发同权 | 心跳走同一套 toolperm/outbound，bot 自主行动与 @触发同档 ✅ |
| 触发方式 | 推荐直接 `pipeline.Execute`（§9.1），需实施时核实同步性 ⏳ |
| 是否走 Ingress | 倾向不走（见 §9.1），除非 pipeline.Execute 非同步 ⏳ |
| 心跳发言频控 | **必备硬机制**：`maxConsecutiveWakes`（默认 3）+ 冷却窗 `cooldown_min`；超阈降级为纯 inject，不再靠纯 prompt。借鉴 deepseek ✅（已定，待实施） |

---

## 12. 对比 deepseek-harness（2026-08-14 临时 clone 对比）

> 目的：确认方向、借他山之石补强本方案。仓库 `deepseek-ai/deepseek-harness`（Cordis 插件式 agent 框架，81M / 7000+ 文件，临时 clone 于 `/tmp/deepseek-harness`）。

### 12.1 它有没有 health-check 式心跳？—— 没有。

它的 `heartbeat` 一词**仅出现在 Cordis 生命周期教程**（`docs/cordis-tutorial/02-lifecycle-and-effects.md`）里，是 `setInterval(() => console.log('tick'), 200)` 的**插件生命周期演示**，与 agent 健康监控无关。整个仓库没有任何「检查 bot 是否活着」的心跳模块。

**结论：我们的纠偏方向（心跳=自主唤醒，而非 health-check）与 deepseek 的实践一致——它根本没把心跳当监控。这进一步验证了露娜的判断。**

### 12.2 它怎么做「自主唤醒 / 让 agent 自己决定做事」？

它有完整的一套**唤醒驱动 + 准入 + 频控**机制，核心概念：

| 概念 | 出处 | 含义 | 对本方案的借鉴 |
|---|---|---|---|
| **Inbox 唤醒驱动** | `architecture.md:86` | "Input reaches the driver through one inbox. Some messages wake it immediately" —— 唤醒就是一种进 inbox 的 message | 印证「心跳=一条 message 进编排」思路 ✅ |
| **`agent/pre-step` reject / empty-claim** | `architecture.md:88`、`agent-lifecycle.md:28-32` | 准入瀑布可 `reject` 消息或将首条 claim 重写为空；**reject/空 claim 关闭一个「耗费 0 step 的 turn」，但日志仍记录这次尝试** | → 我们的 §5.5 准入关卡：bot 审视后静默应是架构层确定性决策，而非 prompt 憋空输出 |
| **`maxConsecutiveWakes`（默认 3）** | `packages/jobs/tool-jobs/README.md:25,36` | 连续唤醒开启的轮数封顶，超出降级为 inject；专治「自激链」（被唤醒轮又启动任务→完成再唤醒） | → 我们的 §9.3 硬频控，取代纯 prompt 抑制 |
| **`agent.inject()` 注入 vs 拼文本** | `architecture.md:120` | "Add model-facing context \| call `agent.inject()`; it lands in the next admitted request" —— 模型可见上下文走独立注入，不污染对话历史 | → 修正 §7：唤醒提示走独立 inject 通道，**不污染 L0 记忆**（原本拼 `Text` 会被 `note_capture` 写入记忆层，是个隐患） |
| **Session-log 不变量 "Model-visible means logged"** | `architecture.md:96` | 任何进模型的输入必须能从 session log 重建，并有运行时断言 | → 我们的 §2 不变量：每次唤醒（含 0-step 静默）都落 durable 日志 |
| **`ctx.jobs` 后台工作 + `ctx.goals` 会话级目标** | `architecture.md:116,125` | "Manage a same-session objective \| use `ctx.goals`; continue through `agent/*`" —— bot 自主找事做可由会话级目标驱动 | 可选增强：bot 自主找事做可加 goals 概念（本期不强制） |
| **`completionDelivery: quiet \| wakeup`** | `tool-jobs/README.md:35` | quiet = 通知留在 inbox 不新开轮（确定性 transcript 需要） | 映射我们的「平台静默」闸门（只 inject 不 wake） |

### 12.3 它没做、而我们独占的点

- **多 bot / 多 channel 的社交主体语义**：deepseek-harness 是单人开发 agent（一个用户 + 一个 agent），没有「bot 在多平台多渠道作为社交主体被 @ / 主动发帖」的语义。我们的两级发言闸门（平台策略优先 + bot 自主）、toolperm/outbound 社交护栏是我们独有的复杂度，deepseek 没有对应物。
- **梦境 / 分层记忆（L0-L3）**：我们的记忆子系统是独立大模块，deepseek 的 session log 是单一 append-only 流（无分层晋升）。唤醒时「bot 基于长期记忆找事做」这点我们的记忆层更纵深。
- **潜水 / 观察者模式**：我们的 lurk 模式（speak OFF + learn ON）deepseek 无对应。

### 12.4 吸收进本方案的 4 个改善点（已缝入上文）

1. **准入关卡取代「憋空输出」**（§5.5）—— 静默是架构层决策，非模型概率性克制，且为「0-step 也落日志」提供干净落点。
2. **连续唤醒硬上限 + 冷却**（§9.3）—— 防自激刷屏，取代纯 prompt 抑制。
3. **注入而非拼文本**（§7）—— 唤醒提示走独立 inject 通道，**修正原稿「拼进 Text 会污染 L0 记忆」的隐患**。
4. **唤醒落 durable 日志不变量**（§2）—— 每次唤醒（无论是否发言、是否 0-step）都可复盘。
