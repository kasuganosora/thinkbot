# engagement — 主动参与决策引擎

解决"Bot 观察到时间线帖子（未被 @）时是否主动参与"的决策问题，采用三层漏斗逐级过滤避免无谓 LLM 调用。

设计参考 MaiBot Timing Gate + Houde et al. (2025) *"Controlling AI Agent Participation in Group Conversations"* 的控制分类法。

## 功能

- **Tier 0 渠道能力检查**：毫秒级判断渠道是否可写
- **Tier 1 规则引擎**：自排除（静态 ID / 动态 `SelfIDSet`）、纯转发排除、黑名单、长度、关键词、用户冷却、令牌桶限流
- **Tier 2 LLM 快判**：可选，传统 YES/NO 与评分模式（0-100 + 可配置阈值）；判定快照可经 `JudgeRecordSink` 落库，按 bot / model 归因
- **TimingGate 时序门控**：概率频率门控、突发检测（debounce）、指数退避、Wait 超时重评估、空闲补偿、per-channel 动态配置、随机噪声
- **BurstBuffer**：连发消息缓冲，突发结束后只评估最后一条
- **预设角色 Profiles**：observer/lurker/moderator/active，一键切换参与风格
- **自适应频率 AutoAdjust**：根据群组活跃度自动调整参与频率
- **对话阶段感知**：推断 divergent/convergent/idle 阶段，动态调整策略
- **自适应画像**：`BotProfileTraits` 从 SOUL.md 解析量化人格（front matter `profile:` JSON > `## Personality` 描述符 > 默认值），经 `AdaptiveEngagementSyncer` 映射为 per-channel engagement 参数；画像经 Dreaming 管线回写 `UpdateTraits`，SOUL.md 热重载时重新解析
- **一次出价熔断 OutreachBreaker**：每个情节对该人只主动出击一次。talk-past / 冷场即视为拒绝，不再补枪（冷却后再敲等于追问，因此不做）。真人 @ / 私聊 / 对 bot 发言的表态（点赞、反应）永远放行并复位。情节边界（默认 5h，可配 `engagement.unanswered_episode_boundary`）后可房间级参与，但剥掉 `reply_target` 以免点名。随机噪声不能绕过
- **判定落库观测**：`JudgeRecord`（engage/score/model/latency）经 `JudgeRecordSink` 异步落库，记录原始判定（不含阈值判断），供按 bot / model 分组评估判定质量
- `EngagementStage` Pipeline 集成（Order=40）

## 关键类型

| 类型 | 说明 |
|------|------|
| `CompositePolicy` | 三层组合策略实现 |
| `Decision` / `Tier` / `Action` | 评估结果 + 决策层级标识 + 三状态决策（continue / no_action / wait） |
| `RuleEngine` / `Rule` | Tier 1 规则引擎 + 规则接口 |
| `TimingGate` | 有状态时序门控（退避/突发/概率/等待/自适应/动态配置/随机噪声） |
| `BurstBuffer` | 消息突发缓冲器 |
| `LLMJudge` / `SimpleJudge` | Tier 2 LLM 快判（传统 + 评分模式） |
| `JudgeRecord` / `JudgeRecordSink` | 判定快照与落库接口（接口定义在本包，实现由 stats 包提供，非阻塞旁路） |
| `BotProfileTraits` / `AdaptiveEngagementSyncer` | SOUL.md 画像解析、画像 → engagement 参数动态映射（含 per-channel 覆盖） |
| `OutreachBreaker` | 按人一次出价：没接住就停；@ / 私聊 / 反应解除；情节边界后仅房间级（剥 reply_target）。导出 `OnInbound` / `ShouldSuppress` / `ShouldStripReplyTarget` / `IsDeclined` / `IsPending` / `RecordProactiveReply` / `NotifyProactiveSent` |
| `EngagementProfile` | 预设角色配置文件 |
| `ConversationPhase` | 对话阶段推断（idle/divergent/convergent） |
| `TokenBucket` / `SlidingWindow` | 限流器实现 |
| `EngagementStage` | Pipeline Stage（Order=40） |

## 论文对照（Houde et al. 2025）

| 论文维度 | 实现 | 配置项 |
|---------|------|--------|
| WHEN: 贡献价值阈值 | Tier 2 评分 0-100 + `engagementThreshold` | `engagement.engagement_threshold` |
| WHEN: 自适应速率 | `AutoAdjustFrequency` + 对话阶段推断 | `engagement.auto_adjust_frequency` |
| HOW: 角色选择 | 4 个内置 Profile，一键切换参与风格 | `engagement.profile` |
| WHEN: 外部决策逻辑 | 三层漏斗本身就是外部控制 | — |
| WHEN: 突发/退避 | TimingGate BurstBuffer | `engagement.burst_interval_seconds` |
| WHEN: 未回应则退 | `OutreachBreaker` 一次出价、没接住就停 | `engagement.unanswered_silence` / `engagement.unanswered_episode_boundary` |

## 配置项

| 配置键 | 类型 | 默认值 | 说明 |
|--------|------|--------|------|
| `engagement.enabled` | bool | false | 总开关；false 时 BuildWritableChecker 返回 DenyAll |
| `engagement.channels` | []string | — | 渠道白名单；为空且 enabled=true 时为 AllowAll（全渠道） |
| `engagement.reply_probability` | float64 | 0.15 | 参与概率 |
| `engagement.profile` | string | — | 预设角色（observer/lurker/moderator/active）。仅覆盖 4 项：ReplyProbability / EngagementThreshold / BackoffStartCount / RateLimitCapacity |
| `engagement.engagement_threshold` | int | 0 | LLM 评分阈值（0=传统YES/NO模式）。生产侧据此选择 NewScoredSimpleJudge / NewSimpleJudge |
| `engagement.auto_adjust_frequency` | bool | false | 自动频率调整 |
| `engagement.cooldown` | duration | 15m | 同一用户冷却（CooldownRule，按 UserID 记录） |
| `engagement.rate_limit_capacity` | int | 3 | 令牌桶容量 |
| `engagement.rate_limit_interval` | duration | 1h | 令牌桶补充间隔 |
| `engagement.keywords` | []string | — | 兴趣关键词（为空时 KeywordRule 不装配；同时作为 Tier 2 prompt 的 Topics of interest） |
| `engagement.llm_judge_enabled` | bool | false | Tier 2 LLM 快判开关 |
| `engagement.blocked_users` | []string | — | 黑名单用户 |
| `engagement.blocked_sources` | []string | — | 黑名单来源 |
| `engagement.min_length` | int | 0 | 最小消息长度（rune；0=不设下限） |
| `engagement.max_length` | int | 0 | 最大消息长度（rune；0=不设上限） |
| `engagement.backoff_base_seconds` | float64 | 10.0 | 退避基准 |
| `engagement.backoff_cap_seconds` | float64 | 300.0 | 退避上限 |
| `engagement.backoff_start_count` | int | 3 | 退避起始计数 |
| `engagement.burst_interval_seconds` | float64 | 5.0 | 突发检测窗口 |
| `engagement.wait_timeout_seconds` | float64 | 30.0 | Wait 超时 |
| `engagement.backoff_bypass_pending` | int | 0 | 退避绕过阈值（0=禁用；仅对群聊生效，私聊不退避） |
| `engagement.unanswered_silence` | duration | 3m | 主动回复后无人回应即视为拒绝（超时只结算，不补发） |
| `engagement.unanswered_episode_boundary` | duration | 5h | 拒绝后仍禁止点名此人；超过此时长才允许房间级参与。完全恢复需对方 @ / 私聊。未设置时用默认 5h |

`engagement.cooldown`（默认 15m）按 **UserID** 生效（`CooldownRule.lastSeen[userID]`），与 OutreachBreaker 的按 (channelKey, userID) 熔断互相独立；二者都不是第二次出价窗口——没接住就停，不会冷却后再敲。

自适应画像（`AdaptiveEngagementSyncer`）不走 `engagement.*`，配置键为 `bot.<id>.engagement.adaptive.<sub>`（`config.BotAdaptiveEngagementKey`）：`enabled`（Bot 全局）→ `channel.<type>.enabled` → `channel.<type>.<chatid>.enabled`，未显式打开的 channel 不启用。

## OutreachBreaker 状态机

```
open ──proactive Send 成功──► awaiting
awaiting ──talk-past / 冷场──► declined     // 立刻，不补枪
awaiting / declined ──对方 @、私聊、或点赞/反应──► open
declined 且超过情节边界 ──► 可房间级参与，剥掉 reply_target（禁止点名）
```

反应（点赞）是接住而非 talk-past：`core.IsReactionAck` 事件按 `MetaReactorIDs` 把该用户在**所有 channel** 上的状态复位（Misskey 反应的 channel key 与出价时不同）；该条事件本身不再升级为主动参与，且 LLMStage 对其卸载全部工具（awareness-only）。

硬抑制原因：`KVSuppressReasonUnanswered`（`unanswered_outreach`）。rhythm / 模型 `send:true` 不能覆盖（`core.IsHardSuppressReason` 硬门）。

状态按 (channelKey, userID) 存储。GC：channel 数超过 1000 时惰性回收，pending 不回收，declined 保留 90 天（继续剥 reply_target），其余 7 天无活动回收。`RecordProactiveReply` 在 declined 状态下不重置 pending——情节边界后的房间级发言不是第二次出价。

## 主链路接线

必须同时挂入站和出站，缺一即空转（历史上 `RejectionDetector` 只写了模块、生产路径未调用）。

| 位置 | 做什么 |
|------|--------|
| `api/botservice.go` | `engagement.enabled` 时 `NewOutreachBreaker`，同一实例 `WithOutreachBreaker` + `BotParams.OutreachBreaker`；Stage 进 Pipeline Order=40（`pipeline.GroupEngagement` 门控） |
| `api/botservice.go`（Tier 2） | `LLMJudgeEnabled` 时优先用 Light 模型做快判；`WithJudgeModel` + `WithJudgeSink` 注入模型标识与落库目标 |
| `api/botservice.go`（BurstBuffer / 自适应） | `BurstIntervalSeconds>0` 时建 BurstBuffer，Bot 创建后经 `Ingress().Receive` reenqueue；TimingGate 接 `SetDynamicConfig(syncer.GetTimingConfigOverride)` + `SetRandomNoiseRate(0.08)` |
| `EngagementStage.Process` | 升级 `Mentioned` 之前 `OnInbound` + `ShouldSuppress`（TimingGate 噪声不能绕过）；情节边界后剥 `reply_target` |
| `bot.New` | `ChannelReplyHandler.SetOnSent` → `NotifyProactiveSent`（仅 Send 成功） |
| `stages/llmroute.go` / `reply_stage.go` | `CopyEngagementOutboundMeta` 把 `engagement.proactive` 打进 Action |
| `outbound/channel_handler.go` | Send 成功后调 `onSent`；失败 / 只读守卫丢弃不记账 |

真人 `@` / 私聊走 Stage 早退并复位熔断，不注入「你无视我了」之类 prompt。反应事件同理早退（`engagement.reaction=true`）。心跳消息（`core.SourceHeartbeat`）跳过 engagement 门控直接放行（`engagement.heartbeat=true`），否则误判不参与会压制 bot 的自主发言。

评估未通过时不 Abort 整条 Pipeline：仍设 `core.KVSuppressReply`，消息继续流转（记忆写入等下游 Stage 照常执行），只抑制对外发送；同时退还 `RateLimitRule` 预扣的令牌。参与升级成功时补充 `reply_target`（缺失时用消息 ID），被 Strip 场景则删除该键。
