# engagement — 主动参与决策引擎

解决"Bot 观察到时间线帖子（未被 @）时是否主动参与"的决策问题，采用三层漏斗逐级过滤避免无谓 LLM 调用。

设计参考 MaiBot Timing Gate + Houde et al. (2025) *"Controlling AI Agent Participation in Group Conversations"* 的控制分类法。

## 功能

- **Tier 0 渠道能力检查**：毫秒级判断渠道是否可写
- **Tier 1 规则引擎**：关键词/黑名单/长度/冷却/限流，挡掉 ~90% 噪音
- **Tier 2 LLM 快判**：可选，支持传统 YES/NO 和评分模式（0-100 + 可配置阈值）
- **TimingGate 时序门控**：概率频率门控、突发检测（debounce）、指数退避、空闲补偿
- **BurstBuffer**：连发消息缓冲，突发结束后只评估最后一条
- **预设角色 Profiles**：observer/lurker/moderator/active，一键切换参与风格
- **自适应频率 AutoAdjust**：根据群组活跃度自动调整参与频率
- **对话阶段感知**：推断 divergent/convergent/idle 阶段，动态调整策略
- `EngagementStage` Pipeline 集成（Order=40）

## 关键类型

| 类型 | 说明 |
|------|------|
| `CompositePolicy` | 三层组合策略实现 |
| `Decision` / `Tier` | 评估结果 + 决策层级标识 |
| `RuleEngine` / `Rule` | Tier 1 规则引擎 + 规则接口 |
| `TimingGate` | 有状态时序门控（退避/突发/概率/等待/自适应） |
| `BurstBuffer` | 消息突发缓冲器 |
| `LLMJudge` / `SimpleJudge` | Tier 2 LLM 快判（传统 + 评分模式） |
| `EngagementProfile` | 预设角色配置文件 |
| `ConversationPhase` | 对话阶段推断（idle/divergent/convergent） |
| `TokenBucket` / `SlidingWindow` | 限流器实现 |
| `EngagementStage` | Pipeline Stage（Order=40） |

## 论文对照（Houde et al. 2025）

| 论文维度 | 实现 | 配置项 |
|---------|------|--------|
| WHEN: 贡献价值阈值 | Tier 2 评分 0-100 + `engagementThreshold` | `engagement.engagement_threshold` |
| WHEN: 自适应速率 | `AutoAdjustFrequency` + 对话阶段推断 | `engagement.auto_adjust_frequency` |
| HOW: 角色选择 | 4 个内置 Profile，一键切换全部参数 | `engagement.profile` |
| WHEN: 外部决策逻辑 | 三层漏斗本身就是外部控制 | — |
| WHEN: 突发/退避 | TimingGate BurstBuffer | `engagement.burst_interval_seconds` |

## 配置项

| 配置键 | 类型 | 默认值 | 说明 |
|--------|------|--------|------|
| `engagement.enabled` | bool | false | 总开关 |
| `engagement.channels` | []string | — | 允许参与的渠道 |
| `engagement.reply_probability` | float64 | 0.15 | 参与概率 |
| `engagement.profile` | string | — | 预设角色（observer/lurker/moderator/active） |
| `engagement.engagement_threshold` | int | 0 | LLM 评分阈值（0=传统YES/NO模式） |
| `engagement.auto_adjust_frequency` | bool | false | 自动频率调整 |
| `engagement.cooldown` | duration | 0 | 用户冷却 |
| `engagement.rate_limit_capacity` | int | 3 | 令牌桶容量 |
| `engagement.rate_limit_interval` | duration | 1h | 令牌桶补充间隔 |
| `engagement.keywords` | []string | — | 兴趣关键词 |
| `engagement.llm_judge_enabled` | bool | false | Tier 2 LLM 快判开关 |
| `engagement.blocked_users` | []string | — | 黑名单用户 |
| `engagement.blocked_sources` | []string | — | 黑名单来源 |
| `engagement.min_length` | int | 0 | 最小消息长度 |
| `engagement.max_length` | int | 0 | 最大消息长度 |
| `engagement.backoff_base_seconds` | float64 | 10.0 | 退避基准 |
| `engagement.backoff_cap_seconds` | float64 | 300.0 | 退避上限 |
| `engagement.backoff_start_count` | int | 3 | 退避起始计数 |
| `engagement.burst_interval_seconds` | float64 | 5.0 | 突发检测窗口 |
| `engagement.wait_timeout_seconds` | float64 | 30.0 | Wait 超时 |
| `engagement.backoff_bypass_pending` | int | 0 | 退避绕过阈值 |
