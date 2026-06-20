# engagement — 主动参与决策引擎

解决"Bot 观察到时间线帖子（未被 @）时是否主动参与"的决策问题，采用三层漏斗逐级过滤避免无谓 LLM 调用。

## 功能

- **Tier 0 渠道能力检查**：毫秒级判断渠道是否可写
- **Tier 1 规则引擎**：关键词/黑名单/长度/冷却/限流，挡掉 ~90% 噪音
- **Tier 2 LLM 快判**：可选，用小模型 YES/NO 快速判断剩余消息
- **TimingGate 时序门控**：概率频率门控、突发检测（debounce）、指数退避、空闲补偿
- **BurstBuffer**：连发消息缓冲，突发结束后只评估最后一条
- `EngagementStage` Pipeline 集成（Order=40）

## 关键类型

| 类型 | 说明 |
|------|------|
| `CompositePolicy` | 三层组合策略实现 |
| `Decision` / `Tier` | 评估结果 + 决策层级标识 |
| `RuleEngine` / `Rule` | Tier 1 规则引擎 + 规则接口 |
| `TimingGate` | 有状态时序门控（退避/突发/概率/等待） |
| `BurstBuffer` | 消息突发缓冲器 |
| `LLMJudge` | Tier 2 LLM 快判 |
| `TokenBucket` / `SlidingWindow` | 限流器实现 |
| `EngagementStage` | Pipeline Stage（Order=40） |
