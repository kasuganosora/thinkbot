# 记忆系统

> README 里有一段通俗版的「记忆是怎么工作的」，这里是稍微深入一点的说明。实现细节（类型、接口、参数）见 [agent/memory/README.md](../agent/memory/README.md)。

## 分层

| 层 | 是什么 | 默认容量 |
|---|---|---|
| L0 工作记忆 | 近期对话里值得留意的内容 | 200 条，14 天过期 |
| L1 长期记忆 | 提炼过的事实、偏好、事件 | 500 条 |
| L3 画像 | 用户画像（这个人是谁、偏好什么）+ Bot 自我画像 | 20 条 |

L2 场景记忆是预留的扩展点（`memory.Aggregator` 接口已定义，仓库未提供实现），生产管线实际为 L0 → L1 → L3。

记忆按作用域隔离：`channel`（这个群 / 这个会话聊了什么）、`user`（这个人的画像，跨会话）、`bot`（bot 自身知识）、`global`。群聊里每个人的画像分开记，不会混在一起。channel 作用域的记忆会镜像一份到 bot 作用域（带来源平台标注），所以在 Telegram 聊过的事，换到 Web 也能接上。

所有记忆存在同一个 SQLite 数据库里（`tiered_memories`、`memory_entries` 等表），数据始终在你自己的机器上。

## 记下来

- 每轮对话后，系统自动判断哪些信息值得记（Think Filter），写入前会剥掉工具输出（搜索结果、文件内容、API JSON 等）和思考标签，并做注入 / 外泄模式扫描。
- bot 也可以主动调用 `memory` 工具（`add` / `replace` / `remove` / `search` / `recent` / `count` / `batch`）记录或修改。
- 系统推测出来的内容先标记为「待确认」，不会被当成事实注入；被新事实取代的旧说法标为「已取代」，保留可追溯但不再注入。

## 整理（巩固）

- **自动巩固**：后台定期用 LLM 把 L0 合并、去重、更新为 L1。
- **梦境（dreaming，可选，默认关闭）**：每个 bot 可在「Bot 设置 → 梦境」开启，默认每天凌晨 3 点（`0 3 * * *`）跑一次，分 Light → REM → Deep 三个阶段，像睡一觉消化白天的事：
  - 从近几天的短期记忆中找候选，按频率、新近度、反复出现程度、信息量等 6 个信号打分（`dream_score`）；
  - 只有分数过门槛的候选才在 Deep 阶段写入长期记忆，零碎、一次性的内容自然留在短期层并随过期淡出；
  - 梦境结束时顺带更新用户画像与 Bot 自我画像（置信度低于 0.4 的结果丢弃）。
- 管理 API 还提供手动清理琐碎记忆与重复记忆（`POST /api/bots/:id/memory/cleanup-trivial`、`/cleanup-duplicates`）。

## 想起来

回复前，系统按 **画像 → 长期记忆 → 原始笔记** 的顺序召回与当前话题相关的内容，用系统标注包起来放进上下文（避免被误当成用户输入），并受 token 预算控制（`memorywindow.*`）。

如果 SQLite 的 sqlite-vec 扩展可用，会额外建一份向量索引（`memory_vec`），按「意思相近」召回——几周前随口提过的爱好，在聊到相关话题时也能被想起来；不可用时退回关键词匹配。

## 忘掉

- **请它忘掉**：bot 用 `memory` 工具的 `remove` / `batch` 删除相关条目。被删除的说法会留下一个「墓碑」（只存指纹，不存原文；每个作用域最多 500 条），之后的自动巩固、梦境或历史回灌都不会把它重新学回来。
- **「别再提 X」**：消息里出现「别再提 / 不要再提 / 别提 / 不准再提 …」这类中文说法时，系统会抽出话题，之后召回时把命中该话题的记忆过滤掉（目前只识别中文句式；英文请直接让它删除相关记忆）。
- **管理员手动删**：「Bot 设置 → 记忆」可逐条查看和删除。

注意：删除记忆不会删除聊天记录本身；要连会话一起清掉，请在 Web 端删除对应会话。

## 长对话：压缩与重开

这些操作针对的是**当前会话的上下文**，不是长期记忆：

- 自动上下文压缩：会话接近模型上下文上限时，自动把较早的对话总结成摘要后继续（`compaction.*` 配置，可在「Bot 设置 → 上下文压缩」调整）。
- `/compact [N]`：手动把当前会话压到只保留最近 N 条消息（默认 3），仅管理员可用。
- `/clear`：清空当前会话上下文，从头开始，仅管理员可用。
- `/help` 列出所有命令。

## 去重（memory_dedup）运维

- 输出上限 `llm.internal_max_tokens.memory_dedup` 默认 12288（含 reasoning；GLM-5.3 把 medium 当 high 跑）。
- 每次去重 pass 结束打一行 `memory_dedup_stats`（`calls_total` / `truncated_total` / `truncated_ratio`，进程启动以来累计，含 cluster 与 repair 两阶段）；单次被输出上限截断时打 `memory_dedup_truncated`（warn）。
  `grep memory_dedup_truncated` 计数 ÷ 最后一行 `memory_dedup_stats` 的 `calls_total` 即截断率。
- 离线 dry-run（不调用 LLM、不联网）：打印去重会形成的向量近邻分组，用来对照真实 sqlite-vec 校验 0.45 阈值。
  默认先把库（含 -wal/-shm）复制到临时目录再打开，原库不被修改：

  ```sh
  THINKBOT_SQLITE_VEC=/usr/lib/sqlite-vec/vec0 \
    go run ./cmd/dedup-dryrun -db /path/to/thinkbot.db -all -reindex -min-sim 0.45
  ```

  `-all` 忽略 dedup_checked_at（存量都已回填为已检查），`-reindex` 按当前哈希向量重建 memory_vec，
  `-scope channel:xxx` 只看指定 scope，`-k` / `-max-seeds`（-1 不限）/ `-max-chars` 同生产配置。
  加载不到 sqlite-vec 时输出 `vec unavailable … fallback mode`，分组为生产环境的 recent 回退逻辑。
