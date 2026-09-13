# outreach — 对人主动开口

定时轮询到期的提醒 / 盯梢，**能不能发由规则和配额决定**，模型只在闸门全开之后走完整 pipeline 措辞（可调工具）。

与心跳的区别：心跳是 bot 作为社交主体对时间线自醒（LLM 决策 silent/post/note）；本包是对某个用户开口，且禁止模型投「要不要说」的票。

## 数据流

1. cron 每 N 分钟醒一次（默认 30）
2. 扫 `outreach_commitments` 到期 pending；没有则落 silent，零 LLM
3. 按平台开关 / 发言模式 / 静默窗 / 每日上限过滤（硬条件可绕过静默窗和日限）
4. 闸门打开 → `Engine.ProcessSync`（InjectContext + `KVOutreachForceSend`）
5. 落 `outreach_records`，承诺标 delivered

## 配置

`data/outreach/{botId}/config.json`：bot 总开关 + 间隔；`platforms.web|telegram|misskey` 各自 enabled / 配额 / 静默窗 / 硬条件绕过。

写入入口：`remind` 工具（create / list / cancel）。
