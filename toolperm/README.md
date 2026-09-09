# toolperm — Bot 工具权限

`toolperm` 提供 **bot 维度的工具授权**：决定某个 bot 在某次会话里，能对当前用户放开哪些工具（`web_search`、`sandbox_exec`、`misskey_create_note`……）。数据落在 `bot_tool_permissions` 表，同一套规则同时被「工具权限」配置页与「发言模式」开关复用。

包内三个文件分工：

| 文件 | 职责 |
|---|---|
| `risk.go` | 工具风险分级（basic / sensitive / broadcast / exfil），决定「没有规则命中时」的默认值 |
| `service.go` | 规则 CRUD、缓存、评估（`EvaluateUsers`）、`ToolAccessEvaluator` 适配 |
| `outbound.go` | 渠道出站（对外发言）权限 + 三态「发言模式」（active / passive / mute） |

---

## 一、规则结构

一条规则由以下维度描述（`RuleDTO` / `RuleReq`，见 `service.go`）：

| 字段 | 含义 | 通配 |
|---|---|---|
| `botID` | 归属 bot | — |
| `tool` | 工具名 | `*` 或空 = 全部；支持 glob，如 `sandbox_*` |
| `platform` | 平台类型（`web` / `telegram` / `misskey` / …） | `*` 或空 = 全部；否则精确匹配 |
| `chatId` | 会话/群标识，如 tg 群 `-1001234567890` | `*` 或空 = 该平台全部会话；否则精确匹配 |
| `userIds` | 用户 ID 列表（JSON 数组） | 含 `*` = 全部；支持 glob（如 `luna*`） |
| `decision` | `allow` / `deny` | — |
| `enabled` | 仅 `enabled=true` 的规则参与评估 | — |
| `sort` | 遍历顺序，升序；同 sort 按 ID 稳定排序 | — |
| `auto` | 是否由「发言模式」开关自动维护（切换模式时只增删 auto 规则，不碰手动规则） | — |

规则四个维度 **AND** 组合：一条规则要命中，必须同时匹配 tool、platform、chatId、userIds。

---

## 二、判定顺序

### 1. 显式规则：首条匹配生效

按 `sort` 升序遍历启用规则，第一条同时匹配 `(tool, platform, chatID, user)` 的规则直接决定结果。管理员的显式配置永远优先于任何默认值——包括对基础工具的 deny。

### 2. 前置硬约束（evaluator.allow，先于规则评估）

- **broadcast/exfil 工具**在 `platform == ""` 或 `isSubagent` 时一律拒绝：子智能体不应继承主会话平台的发言/外发权限。
- **系统会话**（`IsSystem=true`：cron、心跳、梦境巩固）豁免一切**除对外发言之外**的工具；broadcast/exfil 必须走权限表——想给 cron 开定时发帖，配显式 allow。
- 评估失败（DB 错误）→ 保守拒绝。

### 3. 无规则命中时的默认策略（按风险分级，见 `risk.go`）

| 工具风险 | 判定 | 典型工具 |
|---|---|---|
| `basic` 基础 | **始终放行** | `calculate`、`now`、`random`、`text_*`、`memory`、只读查询（`misskey_search_notes` 等） |
| `sensitive` 敏感 | 默认禁止 | `sandbox_*`、`web_*`、`exec`、`mcp_*` 及一切未收录工具 |
| `broadcast` 对外发言 | 平台无规则 → 放行；平台有规则未命中 → 禁止 | `misskey_create_note`、`telegram_pin_message`、`misskey_/telegram_/browser__` 前缀兜底 |
| `exfil` 外发通道 | **任何情况下默认禁止，必须显式 allow** | `telegram_send_document`、`telegram_send_photo` |

即：

- **平台完全没有启用规则** → 保守默认：basic + broadcast 放行，sensitive 禁止，exfil 禁止；
- **平台已有规则但无命中** → 白名单模式（收紧）：仅 basic 放行，broadcast / sensitive / exfil 均禁止，避免「配一条 deny 反而放开其它」的提权；
- **web 平台**：`ListRules` 惰性播种一条 `tool=* platform=web allow` 基线（`SeedWebDefault`，已有 web 覆盖时不覆盖），保证网页对话开箱全开。

未收录的工具（MCP 动态工具、新 Channel 工具）一律按 **sensitive** 处理；`misskey_` / `telegram_` / `browser__` 前缀按 **broadcast** 兜底（显式只读例外除外），新工具默认最严，不会因忘记登记而悄悄获得能力。

---

## 三、三个关键语义

### 1. per-chat scope 维度（2b30ba2）

规则可下钻到单个会话：`chatId` 填具体值（如某个 Telegram 群）时仅对该会话生效，与 `platform` 叠加实现「针对某个群单独配置」。空串/`*` 匹配全部会话，存量规则（无 chat_id）保持「仅配 platform 即全平台生效」的旧行为。入站侧 `chatID` 取自 `core.Message.Channel`（telegram 下即群/私聊 chat ID），管理员可用 `/chatid` 命令直接拿到当前群/用户/平台 ID 填进表单。

配置示例（仅允许某个 tg 群里所有人用 sandbox 工具）：

```json
{ "tool": "sandbox_*", "platform": "telegram", "chatId": "-1001234567890",
  "userIds": ["*"], "decision": "allow" }
```

### 2. 多身份 OR 匹配（064cede）

`userIds` 的匹配对象不是单个 ID，而是当前用户的**候选身份集合**（OR 语义）：

- 平台数字 User ID；
- 平台账号名（来自消息元数据）；
- 已绑定的 thinkbot 内部账号名（经 `identity_mappings` + `users` 反查，未绑定则不命中）。

规则里的任意一个 `user_id` 命中候选集中的任意一个即算匹配；两侧都去掉前导 `@`，`luna` 与 `@luna` 等价。所以管理员填三种身份形式中的任何一种都能生效：

```json
{ "tool": "web_search", "platform": "misskey",
  "userIds": ["12345678", "@luna", "luna_tb_account"], "decision": "allow" }
```

### 3. 外发文件工具默认拒绝（064cede / 2b30ba2 / 2078a25）

`telegram_send_document` / `telegram_send_photo` 会把 bot 工作空间内的任意文件投递到外部会话，属于 **exfil（工作空间外泄通道）**，比 broadcast 更严：

- 即便平台**没有任何权限规则**（走保守默认分支）也**默认禁止**，必须管理员显式 allow 才放开——否则新配置的 Telegram bot 天然带着一条开放的外泄通道；
- 复用 broadcast 的硬约束：系统/心跳/子代理会话一律不得外发，防无人监督时偷偷送文件；
- 平台进入白名单模式后同样默认拒绝。

放开外发必须显式配置：

```json
{ "tool": "telegram_send_document", "platform": "telegram",
  "userIds": ["12345678"], "decision": "allow" }
```

---

## 四、双重防线与代码接入

权限在两处生效（`FilterTools`）：

1. **列表过滤**：`ResolveTools` 阶段未授权的工具直接不进 LLM 工具列表；
2. **call-time 复核**：每个放行的工具被包一层 Execute 包装器，真正执行前用同一会话上下文（botID/platform/chatID/候选身份的值快照）再评估一次，防止列表过滤被绕过或规则在解析后变更。

```go
svc := toolperm.NewService(db, logger)
tm.SetAccessEvaluator(svc.NewEvaluator()) // FilterTools + call-time 复核

// 单独判定（单身份兼容包装）
ok := svc.Evaluate(botID, "web_search", "telegram", userID)
// 多身份 OR 匹配 + 会话维度
ok = svc.EvaluateUsers(botID, "web_search", "telegram", "-1001234567890",
    []string{"12345678", "@luna", "luna_tb"})
```

规则写路径（`CreateRule` / `UpdateRule`）注意：**更新是部分更新语义**——只有显式提供的字段才修改，缺失字段保留库中原值。绝不能改回全量归一化，否则前端只回传 `{enabled:false}` 会把一条 `(sandbox_exec, telegram, deny)` 静默重置为 `(*, *, allow)`，等于「关一个开关反而放开全部工具」。规则缓存 30s，写操作即失效。

---

## 五、发言模式（outbound.go）

对外发言有三条路径：① LLM 主动调用工具发帖；② Pipeline 被动回复（`ActionReply → Channel.Send`）；③ 心跳自主发帖。「发言模式」把三者统一成一个三态开关，同样落在 `bot_tool_permissions` 表（不新表、不新接口）：

| 模式 | ① 工具主动发帖 | ② 被 @ 回复 | ③ 心跳主动发帖 |
|---|---|---|---|
| `active` | ✅ | ✅ | ✅ |
| `passive` | ❌（auto deny `misskey_create_*`） | ✅ | ❌ |
| `mute` | ❌ | ❌（`__outbound_reply` deny 拦截） | ❌ |

实现细节：保留工具名带 `__` 前缀，与真实工具名空间隔离且不会混进工具选择列表；`__outbound_reply` 采用**精确匹配**而非通配，`tool=*` 的规则不会连带禁掉回复；切换模式先删该平台全部 auto 规则再重建，绝不误删手动配置；被动回复默认放行，只有显式 `__outbound_reply` deny 才拦。
