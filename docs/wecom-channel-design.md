# 企业微信（WeCom）Channel 接入设计

> 状态：**设计阶段 + 长连接已实测打通**，未实现 channel 代码。分支 `feat/wecom-channel`。
> 调研时间 2026-10-20，来源：企业微信开发者中心「智能机器人」文档 + 社区问答 + 第三方 SDK + **真实机器人实测**（见 §9）。
> 目标：在动手前把方案、风险、未知数摆清楚（先验证再实施）。**§9 的实测结论优先于正文的推断**。

---

## 0. 结论先行

**推荐：企业微信「智能机器人」API 模式 + 长连接（WebSocket）**。

理由：
1. **它是主动出站连接** —— 不需要公网 IP、不需要回调 URL、不需要暴露任何入站端口。
   这与 thinkbot 现有三个 channel（telegram 长轮询 / misskey·mastodon WebSocket）**同构**，
   骨架可直接抄 telegram，传输层抄 mastodon 的 WS 管理。
2. **明文 JSON，无需加解密** —— 绕开 URL 回调模式那套 XML + AES-256-CBC + PKCS#7 + 签名校验。
3. **凭证只有两个**（BotID + Secret），配置面最小。

⚠️ 这条路的**代价**是：单机器人只允许一条连接、收发都挤在同一条 WS 上、限流 30 条/分钟/会话。
这三点直接决定了后面几节的设计取舍。

---

## 1. 选型对比（四种企业微信能力）

| 方案 | 收消息 | 发消息 | 公网入口 | 加解密 | 凭证 | 评价 |
|---|---|---|---|---|---|---|
| **A. 智能机器人 · 长连接** | ✅ WS 推送 | ✅ 同一条 WS 发帧 | ❌ 不需要 | ❌ 明文 JSON | BotID + Secret | **推荐** |
| B. 智能机器人 · URL 回调 | ✅ HTTP POST | ✅ HTTP/WS | ✅ 需要 | ⚠️ 可选明文或加密 | Token + EncodingAESKey | 备选；多一套回调端点 |
| C. 自建应用（CorpID/AgentID/Secret） | ✅ 回调 | ✅ 应用消息 API | ✅ 需要 | ⚠️ **XML + AES 必做** | CorpID + AgentID + Secret + Token + AESKey | 最重；且是"应用"不是"机器人" |
| D. 群机器人 webhook | ❌ **只能发不能收** | ✅ | ❌ | ❌ | webhook URL | **不合格**，做不了 channel |

排除 D（收不到消息）。排除 C（thinkbot 项目里 **零** XML/AES 现成代码，需自研 ~200 行；且项目没有任何入站 HTTP 端点基础设施）。
B 与 A 是同一个机器人的两种模式，**互斥**（切换后另一种失效）；A 更适合内网部署。

---

## 2. 长连接协议要点（已核实）

连接地址：`wss://openws.work.weixin.qq.com`

| 方向 | cmd | 用途 |
|---|---|---|
| → 企微 | `aibot_subscribe` | 认证订阅（**连接后必须发，否则收不到任何东西**） |
| → 企微 | `ping` | 心跳，**建议 30s**；连续丢失 3 次 ACK 判定连接失效 |
| → 企微 | `aibot_respond_msg` | 被动回复（**必须透传回调帧的 `headers.req_id`**） |
| → 企微 | `aibot_respond_welcome_msg` | 回复欢迎语（enter_chat 事件） |
| → 企微 | `aibot_respond_update_msg` | 更新模板卡片 |
| → 企微 | `aibot_send_msg` | **主动推送**（不依赖 req_id，指定 chatid + chat_type） |
| → 企微 | `aibot_upload_media_init/chunk/finish` | 上传临时素材（512KB base64 分片 → media_id） |
| ← 企微 | `aibot_msg_callback` | 消息推送 |
| ← 企微 | `aibot_event_callback` | 事件推送（4 种） |

统一帧格式：`{ "cmd": ..., "headers": { "req_id": ... }, "body": { ... } }`

### 2.1 订阅

```json
{ "cmd": "aibot_subscribe", "headers": { "req_id": "<REQ_ID>" },
  "body": { "bot_id": "BOTID", "secret": "SECRET" } }
```
响应 `{ "headers":{"req_id":"REQ"}, "errcode":0, "errmsg":"ok" }`。
⚠️ **订阅有频控**，成功后不要反复订阅。
⚠️ 认证失败（errcode≠0）是**致命**错误 —— 凭证错了，重连也没用，应停止重连并报错。

### 2.2 收到消息

```json
{ "cmd": "aibot_msg_callback", "headers": { "req_id": "<REQ_ID>" },
  "body": { "msgid": "MSGID", "aibotid": "AIBOTID", "chatid": "CHATID",
            "chattype": "group", "from": { "userid": "USERID" },
            "msgtype": "text", "text": { "content": "@RobotA hello robot" } } }
```

- `chattype`：`single` / `group`
- **`chatid` 仅群聊返回**；单聊为空 → 单聊只能用 `from.userid` 寻址
- `msgid` 用于**排重**（回调可能重推）
- `from.userid`：**机器人创建者是企业超级管理员时才是明文 userid，否则是加密 userid**
- 群聊里用户 @机器人 时，`text.content` 带 `@机器人名 ` 前缀 → **必须剥离**
- `compat_info` 存在时表示非普通用户消息（智能助理调用 / 频道）

消息类型：`text` / `image` / `mixed`（图文混排）/ `voice` / `file` / `video`。
**image、voice、file、video 仅单聊支持**。
多媒体带 `aeskey`：`image.url` 5 分钟内有效，AES-256-CBC、PKCS#7 填充至 32 字节倍数、IV 取 aeskey 前 16 字节。

### 2.3 发消息

```json
{ "cmd": "aibot_send_msg", "headers": { "req_id": "<REQ_ID>" },
  "body": { "chatid": "CHATID", "chat_type": 1, "msgtype": "markdown",
            "markdown": { "content": "..." } } }
```

- `chat_type`：**1=单聊（chatid 填 userid）、2=群聊（chatid 填群 chatid）、0/缺省=优先按群聊解析**
  → ⚠️ **缺省会导致单聊发不出去**，必须显式指定
- `msgtype`：`markdown` / `template_card` / `file` / `image` / `voice` / `video`
  （`markdown` 是文本回复的合理默认，比自建应用的 `text` 表达力强）
- 被动回复走 `aibot_respond_msg`，`body.msgtype` **固定为 `stream`**：
  `{"msgtype":"stream","stream":{"id":"stream_xxx","finish":true,"content":"..."}}`
  → 即使是非流式的一次性回复，也用 `stream` + `finish:true`

### 2.4 事件（`aibot_event_callback`）

| eventtype | 含义 | 响应要求 |
|---|---|---|
| `enter_chat` | 用户当天首次进入机器人单聊 | 约 5s 内回欢迎语，否则当天再进入不再推送 |
| `template_card_event` | 点了模板卡片按钮 | **必须 5s 内响应，否则连接被断开** |
| `feedback_event` | 对回复点赞/点踩 | **只能回空包**，不能发新消息 |
| `disconnected_event` | 有新连接建立，旧连接被踢 | 无需回复；**必须停止自动重连** |

### 2.5 硬限制

- **同一机器人同一时间只允许一条有效长连接**；新连接完成订阅会**踢掉旧连接**
- **限流：给某个会话发消息（回复+主动推送合计）30 条/分钟、1000 条/小时**
- **主动推送前置条件：用户必须先在该会话给机器人发过消息**，否则报错 `846607`
  ⚠️ 该错误文案是 `aibot send msg frequency limit exceeded`（**误导为频控**），
  实际原因常常是"这个会话从没交互过" —— 排查时别被文案带偏
- 智能机器人**无法加入外部群**，只能内部群

---

## 3. 与现有 channel 的关系（为什么和 Telegram 是一条路）

| | telegram | misskey/mastodon | **wecom（拟）** |
|---|---|---|---|
| 传输 | HTTP 长轮询出站 | WS 出站 | **WS 出站** |
| 需要公网入口 | 否 | 否 | **否** |
| 入站投递 | goroutine → `Ingress.Receive` | 同左 | **同左** |
| 出站 | HTTP Bot API | REST | **同一条 WS 发帧** ⚠️ |
| 心跳 | 无 | ping/看门狗 | **ping，30s** |
| 订阅 | 无 | 流名在 URL/subscribe 帧 | **aibot_subscribe 帧** |

**结论**：骨架抄 `channel/telegram`（`Start` 起 goroutine → 归一化 → `Ingress.Receive`；`Stop` cancel + `wg.Wait`），
WS 生命周期管理抄 `channel/mastodon`（重连退避、看门狗、连接计数）。
**唯一的新形态是"出站也走 WS"** —— 需要一个 `req_id → response` 的 pending 表（等 ACK），
以及"连接断了就不能发"的处理（见 §6.3）。

---

## 4. 架构映射

| thinkbot 抽象 | WeCom 实现 | 备注 |
|---|---|---|
| `Channel.Name()` | channel 实例名 | **必须等于 `Message.Source`**，否则出站 `no sender for channel` |
| `Channel.Type()` | `"wecom"` | |
| `Channel.Start(ctx, ingress)` | 连 WS → `aibot_subscribe` → 起读循环 + 心跳；**非阻塞** | 认证失败应返回错误，不进重连 |
| `Channel.Stop(ctx)` | 关 WS、停心跳、`wg.Wait()` | 必须真断开，否则下次启动被自己踢 |
| `Sender.Send(ctx, action)` | 发 `aibot_send_msg` 帧 | `action.Channel` 即会话标识，见 §6 |
| `TimelinePoster` | ⚠️ 语义不成立（没有公开时间线） | 可退化为"推到配置的默认会话"，但有 §2.5 前置条件 |
| `RecentChatLister` | ⚠️ `ChatRef.ID` 是 int64，企业微信会话是字符串 | 一期不实现 |
| `ChannelToolProvider` | 见 §7 工具清单 | |

---

## 5. 入站：`core.Message` 字段映射

| Message 字段 | 取值 | 不填的后果（已知静默失效点） |
|---|---|---|
| `ID` | `body.msgid` | 空则**无法去重**，回调重推会重复回复 |
| `BotID` | thinkbot bot id | 会话派生直接 return false |
| `Source` | channel 实例名 | 出站路由失败 |
| `Channel` | 群聊 `chatid`；单聊 `from.userid` | 会话空间标识，空则无会话历史 |
| `UserID` | `from.userid` | 记忆/画像/权限失效 |
| `ChatType` | `single`→`private`，`group`→`group` | 影响工具 Scopes 匹配 |
| `Text` | `text.content`，**剥离 `@机器人名 ` 前缀** | |
| `Mentioned` | 单聊 true；群聊看是否含 @机器人 | 不填则 passive 模式被硬抑制 |
| `Metadata["channel_type"]` | `"wecom"` | **不填 → 无会话历史、消息不落 chat_messages** |
| `Metadata["req_id"]` | 回调帧 req_id | 仅流式/被动回复需要（见 §6.3） |
| `Metadata["chat_type"]` | 1 或 2 | 出站指定 chat_type 用 |
| `Metadata["chattype"]` / `["chatid"]` / `["msgid"]` | 原样带 | 排障与后续扩展 |

---

## 6. 出站设计与限流（重点）

### 6.1 会话寻址

现有 channel 把"发往哪里"放在 `Action.Channel`（= `Message.Channel`）。
企业微信需要额外一个 `chat_type`，所以**把类型编进 Channel 值**：

| 场景 | `Message.Channel` | 出站 `chatid` | `chat_type` |
|---|---|---|---|
| 单聊 | `wecom:dm:<userid>` | `<userid>` | 1 |
| 群聊 | `wecom:chat:<chatid>` | `<chatid>` | 2 |

出站时按前缀反解即可，**不需要额外透传 metadata**，也不依赖 `req_id`。

### 6.2 被动回复 vs 主动推送（取舍）

- `aibot_respond_msg` 需要**透传回调帧的 `req_id`**。但 thinkbot 的出站链路是
  `reply_stage` → `Action{Channel, Payload, Metadata}` → `Send`，**不保证 req_id 能一路透传**。
- `aibot_send_msg` 只需 `chatid + chat_type`，与现有 `Send(action)` 语义天然匹配。

**建议一期：所有消息（含回复）统一走 `aibot_send_msg`**，把 req_id 只作为可选增强。
代价：回复不会"钉"在用户那条消息上。二期若要流式回复，再引入 req_id 透传 + `aibot_respond_msg`。

### 6.3 限流是这条链路的硬约束（先算数量级）

30 条/**分钟**/**会话**，回复与主动推送合计。

| 场景 | 每分钟条数 | 是否安全 |
|---|---|---|
| 单人单聊，一问一答 | ~1–2 | 安全 |
| 5 人同时在群里 @机器人 | ~5 | 安全 |
| 20 人活跃群聊 | ~20 | 逼近上限 |
| **长回复分片**（如 4096 字节上限切成 5 段） | 一条回复吃 5 条配额 | ⚠️ **危险** |
| **流式回复每帧算一条？** | 一次回复可能几十帧 | ⚠️ **未知，必须实测** |

→ 设计上必须：
1. **限制单条回复的分片数**（建议 ≤3，超出直接截断而不是继续切）
2. **每会话令牌桶**（30/分钟），超限排队而非丢弃
3. 出站失败要能区分「限流 846607」与「该会话从未交互」（文案一样，处置不同：前者退避重试，后者直接放弃并记 warn）

### 6.4 连接不可用时的发送

WS 断了就发不出去。**建议 `Send` 在连接不可用时返回明确错误**（不做本地队列无限堆积），
由上层重试/丢弃。可选：配置群机器人 webhook 作为**降级通道**（只能发到 webhook 绑定的群，
不能选会话），用于告警类场景 —— 二期再说。

---

## 7. 平台工具（`ChannelTools`）初拟

| 工具 | 能力 | 权限归类 |
|---|---|---|
| `wecom_send_msg` | 主动推送到指定会话 | **写** → `broadcastTools` |
| `wecom_get_user_info` | 查成员信息（需自建应用或 CLI 授权） | 只读 → `basicTools` **+** `sensitivePrefixExceptions` |
| `wecom_list_chat` | 列出机器人所在群 | 只读（同上，两处都要登记） |
| `wecom_upload_media` | 上传临时素材 | 写 |

⚠️ 只读工具必须在 `toolperm/risk.go` 的 `basicTools` **和** `sensitivePrefixExceptions`
**两处都登记**，否则会被 `sensitivePrefixes` 的 `wecom_` 前缀规则拦掉（静默失效）。

---

## 8. 接入清单（逐项，照抄现有 checklist）

| # | 位置 | 改动 |
|---|---|---|
| 1 | `channel/wecom/` | 新建：`types.go`（帧/消息结构）、`ws.go`（连接+订阅+心跳+重连）、`channel.go`（Channel/Sender/归一化）、`tools.go` |
| 2 | `api/channel_registry.go:82-154` | `supportedChannelTypes` 加一项（字段：`robot_id`、`secret`、`default_chat_id` 等，secret 用 `password` 类型）→ **前端表单自动渲染** |
| 3 | `api/botservice.go:3347-3358` + 新增 `createWecomChannel` | 工厂 switch + 配置解析 |
| 4 | `api/botservice.go:3789-3814` | `inboundSessionID` 加 `case "wecom"` + `wecomSessionTarget` |
| 5 | `api/botservice.go:1108-1119` | `normalizeChannelKind` 加 `case dao.ChannelWecom` |
| 6 | `dao/session_resolver.go:31` | 只加 `ChannelWecom="wecom"`。**不**加 `WecomDMKey`/`WecomChatKey`：企微的 external_key 就是 `msg.Channel`（还要承担出站寻址），真值在 `channel/wecom.PrefixChat`/`PrefixDM`，`wecomSessionTarget` 直接引它们。另建一份拼接函数会两处漂移 |
| 7 | `identity/code.go:88` | `knownPlatforms` 加 `"wecom"` |
| 8 | `api/handler_bot_detail.go:1280` | `botRhythmPlatforms` 加 `"wecom"` |
| 9 | `api/handler_bot_toolperm.go:142` | `outboundPlatforms` 加 `"wecom"` |
| 10 | `agent/outreach/outreach.go:14` | `KnownPlatforms` 加 `"wecom"`（**mastodon 至今没加，是历史欠账**） |
| 11 | `toolperm/risk.go:94-246` + `toolperm/outbound.go:125-137` | 工具权限三处 + `ProactivePostToolPatterns` 加 case |
| 12 | `internal/interaction/interaction.go:60-79` | `ViaWecom` + **`IsValidVia` 必须同步加**，否则 user_choice 应答被判无效 |
| 13 | `api/handler_bot_heartbeat.go:97-205` | 若要支持心跳主动发声，加 `case "wecom"`（走 `aibot_send_msg` + 默认会话） |
| 14 | 前端 `BotOutreach.vue:118/123/149/207/223`、`BotToolPerm.vue:429/437/995` | 加 `wecom` 槽位与 `.pf-wecom` 配色 |
| 15 | `util/log/redact.go` | 给 Secret 加脱敏规则（现有规则只认 telegram token / bearer / `?token=`，**不认企业微信 Secret**） |

---

## 9. 实测结论（2026-10-20，已用真实机器人跑通）

已有测试机器人（BotID + Secret），长连接**已连通并完成一轮真实收发**。

### 9.1 已验证 ✅

| # | 项 | 实测结果 |
|---|---|---|
| 1 | **连接 + 订阅认证** | `aibot_subscribe` → ACK `errcode=0`，RTT ~150ms |
| 2 | **心跳** | 应用层 `{"cmd":"ping"}` 帧，30s 一次，**11 次全 ACK**，5.5 分钟连接稳定 |
| 3 | **被动回复** | `aibot_respond_msg`（透传回调 `req_id`，`msgtype=stream` + `finish=true`）→ ACK `errcode=0` ✅ |
| 4 | **主动推送** | `aibot_send_msg`（`chat_type=1`，chatid 填 userid）→ ACK `errcode=0` ✅ |
| 5 | **单聊 `chatid` 确实是空串** | 回调里 `chatid=""` → 单聊**只能**用 `from.userid` 寻址，§6.1 设计成立 |
| 6 | **userid 是明文** | 实测 `USERID`（机器人由超管创建）→ 不需要走"加密 userid 转明文" |
| 7 | **markdown 上限 = 20480 字节** | `errcode=40058 body.markdown.content exceed max length 20480`。⚠️ 是**字节**不是字符，中文按 3 字节算 → 正文最多约 6800 个汉字 |
| 8 | **userid 稳定** | 同一用户连发 text / image / file 三条，`from.userid` 恒为 `USERID` → 可以直接当会话 key |
| 9 | **媒体解密配方成立** | 实测解出一张 PNG（928×1232，1.97MB），见 §9.2.1 |

### 9.2 实测发现的坑（文档上没有）

| 坑 | 说明 |
|---|---|
| ⚠️ **ACK 帧不带 `cmd`** | 请求响应帧只有 `{headers.req_id, errcode, errmsg}`，**没有 cmd 字段**。 → 只能靠 `req_id` 关联请求与响应，**必须建 req_id → pending 表** |
| ⚠️ **错误 ACK 不带 `req_id`** | 40058 那条回包 `req_id` 为空 → **出错时无法关联**，pending 表必须有超时兜底，错误归属只能按"最近请求"推断 |
| 📌 回调带 `response_url` | 每条 `aibot_msg_callback` 的 body 里都有 `response_url`（`https://qyapi.weixin.qq.com/cgi-bin/aibot/response?response_code=...`）→ 疑似 HTTP 被动回复通道，可作为 WS 之外的备用回复路径（**待验**） |
| 📌 事件体结构是 `body.event.eventtype` | 实测 `enter_chat`：`{"event":{"eventtype":"enter_chat"}}`，嵌套在 `event` 子对象里 |
| ⚠️ **`aeskey` 是 43 字符无 padding 的 base64** | 直接 `base64.StdEncoding.DecodeString` 会报 `illegal base64 data`，**必须先补 `=`** 再解码 → 32 字节 key |
| ⚠️ **file 消息体里没有文件名** | 只有 `file.url` + `file.aeskey`，**没有 filename / size** → 文件名要自己生成或从 URL 推断 |

#### 9.2.1 媒体解密已验证（实测）

配方：`aeskey` 补 `=` → base64 解码得 32 字节 key → **IV = key 前 16 字节** → AES-256-CBC → 去 PKCS#7 填充。

```
aeskey(43) 补 "=" → 32 字节 key: <KEY_HEX>
iv = key[:16]   : <IV_HEX>
密文 1975040 B（16 的倍数）→ 明文 1975025 B
前 8 字节 89 50 4e 47 → PNG ✅（928×1232 RGBA）
```

##### ⚠️ 9.2.1.1 PKCS#7 的填充粒度是 **32**，不是 AES 块长 16（2026-10-20 实测定位）

与企微官方加解密样例（`PKCS7Encoder`，`block_size = 32`）一致：明文填充到 **32 字节边界**，
填充字节数取值 `1..32`；AES-CBC 仍按 16 字节块运算，**只有「去填充」这一步按 32 校验**。

设明文长度 `L`，标准 PKCS#7（粒度 16）与企微（粒度 32）的填充值：

| 条件 | 企微（32） | 标准（16） | 结果 |
|---|---|---|---|
| `L % 32 >= 16` | `32-(L%32)` | `16-(L%16)` | **完全相等** → 一切正常 |
| `L % 32 < 16` | `标准值 + 16`（17..32） | 1..16 | 按 16 校验 → `invalid pkcs7 padding size N` → 媒体整条丢弃 |

→ 表现为「**约一半的媒体随机收不到**」，且失败的那些毫无规律可言。

**实测三份样本的时间线（踩坑经过）：**

| 时间 | 样本 | `L` | `L%32` | 填充值 | 按 16 校验 |
|---|---|---|---|---|---|
| 14:34 | PNG | 1975025 | 17 | 15 | ✅ 恰好重合，成功 |
| 14:35 | 文本文件 | 722420 | 20 | 12 | ✅ 恰好重合，成功 |
| 15:06 | 文件 | ? | 0 | **32** | ❌ `invalid pkcs7 padding size 32` |

前两次都落在重合区，把「粒度 16」这个错误假设掩护得很好；第三次 `padding size 32`
在标准 PKCS#7 下**根本不可能出现**（取值上限是 16），这才暴露出粒度其实是 32。

**对实现的约束：**

- `pkcs7Unpad` 的**长度**按 `aes.BlockSize`(16) 校验，**填充值**按 `wecomPadBlock`(32) 校验，
  两者不能混用（见 `channel/wecom/media.go`）。
- 填充值 > 32 仍然拒绝 —— 加上「n 个尾字节必须都等于 n」的校验，随机垃圾被误判的概率可忽略。
- 单测 `TestDecryptMediaPaddingGranularityIs32` 把 `L%32` 的 0..31 全跑了一遍。

### 9.2.2 群聊路径已验证 ✅

| 项 | 实测 |
|---|---|
| 群聊 `chatid` | `CHATID`（`wr` 前缀，32 字符，多次消息**稳定**） |
| `@` 前缀形态 | `"@mybot lllll哈…"` → 机器人名 **`mybot`** + 一个空格 |
| ⚠️ **回调里没有机器人名字段** | body 只有 `aibotid`，**没有 bot 显示名** → 剥离 @ 前缀要么在 Config 里配一个 `bot_name`，要么用通用正则剥掉开头的 `@token` |
| ⚠️ **图文混排 `mixed` 顶层没有 `text`** | `mixed.msg_item` 是数组：`[{"msgtype":"text","text":{"content":"@mybot "}}, {"msgtype":"image","image":{...}}]` → 必须逐项遍历拼文本 + 收集媒体 |
| 群聊图片走 `mixed`，不走 `image` | 与"image 仅单聊"一致，群里的图文是 `mixed` |
| ⚠️ 空正文真实存在 | 用户会发一个只有 `@mybot ` 的消息，剥离后 `Text == ""` → **必须挡掉不进 pipeline**（否则 LLM 收到空消息） |
| 单聊/群聊 `from.userid` 一致 | 同一个人在单聊和群里都是 `USERID` → 用户身份跨场景统一 |
| ⚠️ **`@` 不一定在开头** | 真实输入：`"群聊带 @ 的模式。@mybot 看看能否收到"` —— @ 在**句中**。只判 `HasPrefix` 会把它判成「没被 @」→ 群聊用例全灭（2026-10-20 线上现象）→ 必须全串搜索 |

#### 9.2.2.1 不带 @ 的群消息**收不到**（平台机制，非缺陷）

| 项 | 结论 |
|---|---|
| 实测 | 群里发不带 @ 的消息，**一条回调都没有**（日志里完全不存在） |
| 官方文档 | 回调示例的正文就是 `"@RobotA hello robot"`；频道事件只列了「频道内被 @ 事件」 |
| 第三方文档 | 「把智能机器人添加到群聊后，可在群里通过 **@ 机器人**的方式发起对话」 |
| 结论 | **智能机器人在群里只在被 @ 时被推送消息。没有配置项能改成「接收全部群消息」** |

**对实现的两个影响：**

1. 群消息即使解析不出 `@` 也应视为被 @（fail-open）。因为能收到就已经说明被 @ 了，
   解析不出多半是客户端渲染差异（全角、零宽字符等）；fail-closed 会让机器人永久哑火。
2. 想要「旁听整个群」的场景**本方案做不到**。那需要走**自建应用**（接收消息服务器配置，
   corp id + secret + 回调 URL 加密），是完全不同的接入方式，不在本 Channel 范围内。

### 9.2.3 `response_url` 一次性回复通道（已验证 ✅）

每条回调 body 都带 `response_url`（`https://qyapi.weixin.qq.com/cgi-bin/aibot/response?response_code=...`），
可直接 HTTP POST 回复，**不需要 req_id、不需要 WS**：

```
POST <response_url>
Content-Type: application/json
{"msgtype":"markdown","markdown":{"content":"..."}}   → {"errcode":0,"errmsg":"ok"}
```

| 约束 | 实测 |
|---|---|
| **一次性** | 同一个 `response_code` 用第二次 → `60140 invalid response code` |
| **不支持流式** | `msgtype=stream` → `40008 invalid message type` |
| 只认 `markdown`（`text` 返回 60140 是因为 code 已用掉，未独立验证） | 需补验 |

**设计影响**：被动回复有两条路 —— WS `aibot_respond_msg`（支持流式、依赖 req_id）与 `response_url`
（一次性、不依赖 req_id、不能流式）。**长回复要分片时第二条发不出去**，所以分片/流式只能走 WS。

### 9.2.4 「文字 + 文件」会被拆成两条回调（已实测，已治理）

用户发「文字 + 文件」时，企微推**两条完全独立的回调**：两个 msgid、间隔 0.3~2.5s，
**没有任何字段**（parent id / 序号 / 批次号）能把它们关联起来：

| 时间 | 第 1 条 | 第 2 条 | 间隔 |
|---|---|---|---|
| 14:35 | `msgtype=text`「文件渠道 测试」 | `msgtype=file` 722KB | ~0.5s |
| 15:06 | `msgtype=text`「看看能否收到这个文件」 | `msgtype=file` | 0.3s |
| 15:45 | `msgtype=text`「再看看」 | `msgtype=file` 9.4KB | 2.5s |
| 15:50 | `msgtype=text`「大文件测试」 | `msgtype=file` 6.5MB | 1.8s |

→ 直接投递的话 bot 会对「文字」和「文件」**各回一次**，用户每次发文件都收到两条回复。

对照组：发**图片**配文字时是**一条 `mixed`**（text + image 在同一回调里），所以只有**文件**会拆。
平台自身行为不一致，我们改不了。

**已有的节奏防抖救不了**：`RhythmStage.QuietWait` 只对群聊生效（单聊默认关闭节奏），
且「真人显式 @ 一律放行」——wecom 单聊我们把 `mentioned` 恒置 true，正好绕过。

#### 治理方案：caption 合并窗口（`Config.CaptionWindow`，默认 3s）

裸文本（无媒体的 `text` 消息）**先暂存一个窗口**；期间同人同会话来了媒体消息，
就把文本并进媒体消息正文、只投递合并后的那一条；窗口内没等到就照常投递。

| 约束 | 处理 |
|---|---|
| ⚠️ **绝不能吞消息** | 窗口超时必须照常投递（比重复回复严重得多），单测 `TestCaptionWindowExpiryDeliversPending` 锁死 |
| ⚠️ 只能同人同会话合并 | key = `channelID + "|" + userID`，跨用户/跨群不串 |
| 用户连发两句 | 旧的那条**立即放行**，只暂存最新的（否则只回一次） |
| 连着来两个文件 | 配文取走即清除，不会被贴两遍 |
| 停机 | `Stop` 先 `flushPending()` 再取消 runCtx，否则那句配文凭空消失 |
| 延迟投递的 ctx | 挂 channel 的 `runCtx`，**不能**挂回调 ctx（那条随回调返回就结束了） |
| `mixed` 也要参与 | 见 9.2.4.2 —— 排除它会让群聊「配文+图片」照样双回复 |

**代价**：没有媒体跟随的纯文本消息，回复被推迟整个窗口（默认 3s）。
`captionWindowMs` 可在渠道配置里调（前端表单已暴露，默认 3000）；`<0` 关闭合并（回到双回复、零延迟）。

##### ⚠️ 9.2.4.1 认领配文必须在下载媒体**之前**（2026-10-20 二次实测）

第一版把「取配文」放在 `fetchMedia`（下载+解密）**之后**，6.5MB 文件直接翻车：

```
16:12:39.0  配文回调到达 → 暂存，窗口至 16:12:42.0
16:12:42.0  文件下载+解密完成（耗时数秒）
16:12:42.0  此时才取配文 → 窗口刚过期 → 没合并上 → 又是两条回复
```

**窗口被下载耗时吃掉了**。合并是否成功只应取决于「两条回调的到达间隔」（实测最大 2.5s），
与文件大小无关 → **认领（`takePending`）提前到 `fetchMedia` 之前**。
代价：认领后下载若彻底失败，配文会留在占位文本里 —— 可接受，总比丢强。
配套加了 `wecom: caption held, waiting for possible media` 日志，下次再出问题
直接看两条日志的时间差就能定位，不用再猜。

##### ⚠️ 9.2.4.2 `mixed` **必须参与**认领（2026-10-20 代码复查发现）

第一版把 `mixed` 排除在合并之外（理由是「它自带正文，再并一次会重复」）。
但**群聊发图走的就是 `mixed`** —— 排除它等于群里「配文 + 图片」这条最常见的
组合完全没被治理，照样双回复。

正确做法是**照常认领、合并前判重**：

```go
caption = c.takePending(key)          // 一律认领，mixed 也不例外
...
if strings.Contains(msg.Text, caption) {
    // mixed 把配文内联在 msg_item 里了 → 丢弃重复的那份（认领即"已处理"）
} else {
    msg.Text = mergeCaption(caption, msg.Text)
}
```

「认领但不用」是关键：不认领的话，那句配文会在 3s 后单独投递成第二条回复。
单测 `TestCaptionMixedClaimsPendingToo` 锁死（回退旧逻辑会变成投递 2 条）。

### 9.2.5 ⚠️ 媒体下载不能卡在 WS 分发循环里（2026-10-20 代码复查）

`onMsg` 原本直接指向 `handleMsgCallback`，而它内部要下载并解密媒体
（单条最长 30s × 最多 4 项 × 最多 2 次重试）。分发循环被同步占住的后果：

| 被拖住的东西 | 后果 |
|---|---|
| ACK 帧没人 resolve | `aibot_send_msg` 在 `callTimeout`(15s) 上超时 → **bot 回不了话** |
| 后续回调没人 dispatch | 配文回调晚到 → 错过合并窗口 → 又变双回复 |
| 应用层 ping | 假失败，日志噪音 |

**治理**：`enqueueMsg` 把回调丢进 `inbox`（容量 32，满则 ERROR 级丢弃并记日志，
绝不阻塞），由 `inboxLoop` **单线程**消费 —— 顺序性保住了（caption 合并依赖
「先文本后媒体」的到达顺序），分发循环也解放了。

- worker 挂 `runCtx`：`Stop` 一取消，在途下载立刻中断，`wg.Wait()` 不会卡 30s
- 队列满时**丢弃并报错**而不是阻塞：阻塞等于把循环卡回去，正是本队列要治的病
- **不 `close(inbox)`**：`Stop` 之后分发循环仍可能并发来一帧，close 会 panic

### 9.2.6 ⚠️ 同一 key 并发 `holdPending` 会静默丢消息（2026-10-20 代码复查）

第一版在临界区**中间**放锁去投递被顶掉的旧消息：

```go
c.pendMu.Unlock()      // ← 窗口打开
c.deliver(ctx, old.msg)
c.pendMu.Lock()        // ← 回来时可能已被别人插入
c.pend[key] = p        // ← 把别人那条覆盖了
```

被覆盖的那条定时器还在，但表里已查不到自己 → `cur != p` 判重成立 → 直接 return
→ **用户那句话凭空消失**。修法：摘旧桶 + 插新桶放进**同一个临界区**，
投递挪到锁外。单测 `TestHoldPendingConcurrentSameKey` 用「慢消费者」把
Ingress 缓冲打满，让 `deliver` 真的阻塞，把窗口放大成必现。

### 9.2.7 ⚠️ 新平台默认拿不到工作空间类工具（2026-10-20 实测，非代码缺陷）

现象：wecom 会话里模型说「看不到文件工具」。根因不在 channel 代码 ——

- `toolperm` 对**没有任何已启用规则**的平台走「保守默认」分支，只放行
  `RiskBasic || RiskBroadcast`；`sandbox_*`（`read_file`/`exec`/`list_dir`）与 `web_*`
  属于 sensitive，一律拒绝。只有 `web` 有 `SeedWebDefault` 的 `*` 放行基线。
- 于是新平台（wecom）一上来自带「整个工作空间工具组离线」。

**处置是配置而非代码**：给该 bot 建一条 `sandbox_*` / `wecom` / `*` / allow 规则即可
（本次建的是 `tp-6993948e7d8b54c5083645fe`）。

另一个容易踩的名字陷阱：`sandbox` 包注册的工具名带 `sandbox_` 前缀，但
`llm.stripSandboxPrefixes` 在把工具列表交给模型**之前**剥掉前缀 ——

| 场合 | 该写的名字 |
|---|---|
| 权限规则（`toolperm`） | `sandbox_read_file`（过滤发生在剥前缀**之前**） |
| 给模型看的提示词 / 日志里的 tool 名 | `read_file` |

两处不一致是有意的，不是笔误（见 `formatSinkFile` 与 `formatFileSinkText` 的注释）。

### 9.2.8 ⚠️ 工作流续跑的出站目标必须是**完整** external_key（2026-10-20 代码复查）

`injectWorkflowContinuation` 只认 `kind == "tg"`，其余一律退回 web 兜底。
企业微信加了 `KindGroup/KindDirect` 会话之后，若不接管就会出现
「工作流跑完了但企业微信里什么都没有」—— 正是 tg 那条路径当年要治的病。

两处配套改动：

1. `resolveSessionRoute`：企业微信返回 `sess.ExternalKey`（`wecom:chat:<id>`）
   而不是 `ParseExternalKey` 拆出来的裸 id 段。
   ⚠️ **只有企微要这样**：它的前缀里编码了 `chat_type`，出站 `parseTarget`
   认的是完整串；Telegram 的 Sender 直接吃裸 chatID，不能一刀切。
2. 新增 `injectContinuationToWecom` / `buildWecomContinuationMessage`
   （照 `injectContinuationToTelegram` 的形制）：`source_channel` = 渠道名、
   `reply_target` = 完整会话标识、`channel_type=wecom`、`__history_managed=true`
   （否则续跑回复只发出、不进历史）。群聊按前缀设 `ChatType=group`。

### 9.2.9 流式回复（类 SSE）：`aibot_respond_msg` + `msgtype=stream`（2026-10-22 实测）

协议要点（官方长连接文档 + 真机探针 15/15 步全 ok）：

| 项目 | 实测结论 |
| --- | --- |
| 帧 | `aibot_respond_msg`，body `{msgtype:"stream", stream:{id, finish, content}}` |
| `req_id` | **必须透传消息回调帧的 req_id**（`headers.req_id`，不在 body 里）。同一回调的多次刷新用同一个 req_id |
| `content` | **全量内容**，不是增量。同 `stream.id` 再推一次 = 客户端原位替换 |
| `finish` | `true` 收尾。服务端从**首次推送起 10 分钟**硬上限 |
| 响应窗口 | 收到回调起 24 小时内可 respond |
| `response_url` | **不支持** stream（errcode 40008），只有长连接模式能做 |

⚠️ `req_id` 在**帧头**不在 body —— 早先 `dispatch` 只解 body、把 header 里的 req_id 丢了，
流式压根没法做。修在 `conn.go` 的 `dispatch`：`body.ReqID = f.Headers.ReqID`。

实测数据（探针连推）：

- 首推 / 反复刷新 / finish 全部 `errcode=0`
- **连推 13 次（≈5 次/秒，折合 300 次/分钟）零失败**
  → respond **不**落在「30 条/分钟/会话」那条限流上（那是 `aibot_send_msg` 的口径）
- 单次 content **27049 字节**也成功 → 流式通道**不**受 markdown 的 20480 字节上限
- 与 `aibot_send_msg` 并存不冲突（刷流的同时 bot 照常整段回复）

实现（A 档，`channel/wecom/stream.go`）：入站先登记流（`openStream`），
1.5s 后才推「正在思考…」占位（`pushPlaceholder`）—— 快回复不推，避免「思考中」闪一下就被替换；
pipeline 出结果后 `Send` 用 `finish=true` **原位替换**。任何一步失败都回退整段发送，**不会丢消息**。

**B 档（边生成边推正文）见 §9.2.11** —— 只有占位是不够的：用户全程不知道 bot 在往哪个方向写，
等最终结果出来才发现跑偏，那时白等一整轮。

几个不显然的点：

- **占位延迟是必要的**：实测 bot 单轮 17~20s，绝大多数场景都会落在占位之后；
  但短回复若也推占位，观感比干等还差。
- **「首推就是 finish=true」未实测**（15 步探针没覆盖：正常回复都要十几秒，占位早推过了）。
  代码不赌这条：检测到还没推过就先补一帧 `finish=false`，两步都落在实测过的路径上。
- **超时必须兜底**：pipeline 完全可能决定不回复（静默 / 被抑制 / 出错），
  那时没人会来 finish，消息会永远停在「正在思考…」直到服务端 10 分钟兜底。
  `reapStreams`（5min）+ `abortStreams`（停机时）负责收尾。

### 9.2.10 轮次控制：输出过程中的「补充 / 打断 / 终止」（2026-10-22）

企微**没有**中止按钮，所以「bot 正在输出时用户又发来一句，这句话归谁」只能靠状态判定。

> ⚠️ 修订：上文原写「企微没有引用关系」，**已不成立**。
> 用户引用某条历史消息时，回调里会多出 `body.quote`
> （`{"msgtype":"text","text":{"content":"..."}}`，官方文档「接收消息」节）。
> 一期漏了这个字段 → 整块被 JSON 解码静默丢弃，用户「引用一条消息再问『这个怎么改』」
> 时 bot 只看得到后一句。现在 `renderQuote` + `mergeQuote` 会把引用渲染成
> `【引用消息】\n> …` 拼在正文**前面**（与企微 UI 里引用显示在输入框上方的顺序一致）。
> 引用**只有文本能取到内容**：图片/文件/语音/视频的引用通常不带 aeskey，
> 只能降级成 `[图片]` 这类类型占位。
> 已知代价：引用在最前面时，正文里的斜杠命令不再位于串首、会失效
> （不会有人「引用一条消息再发命令」，未加特例）。
>
> 单聊回调里引用通常只有 `msgtype=text` 和正文，没有 `quote`。
> 合并转发类引用也不会把内容推给机器人，平台可能自行回一条不支持的提示，
> 那段文案不在本仓库里。群聊回调则会带 `quote`，渲染后正文以
> `【引用消息】` 开头。
>
> → **结论：群聊带、单聊不带。** 只抽样一种会话就下平台级结论会偏。
> 企微单聊与群聊是两条推送路径，行为可以不一样。

能力与 web 端同源 —— `POST /api/chat/append` 与 `/api/chat/abort` 用的就是
`BotService.AppendToMessage` / `AbortMessage`，这里只是给企微接上同一个出口
（`channel/wecom/turn.go`）。三种动作：

| 动作 | 触发条件 | 行为 |
| --- | --- | --- |
| `turnNew` | 无在跑轮次 / 僵尸轮次 | 起新一轮 |
| `turnAppend` | 有在跑轮次，**默认策略** | 注入当前轮继续跑 |
| `turnNew`（restart） | 有在跑轮次 + `interruptPolicy=restart` | 打断旧轮 + 起新一轮 |
| `turnStopped` | 有在跑轮次 + 文本终止指令 | 只打断，不起新轮 |

默认取 **append**（塞进正在跑的那一轮）。终止指令无论哪种策略都只打断。

⚠️⚠️ **append 的两个代价，选它之前必须知道**：

1. **耗时接近翻倍**。补充只在「一步生成结束后」才被编排层取走
   （`llm.drainInterruptMessages`，只在 step 边界调用），
   当前这轮会**照旧跑完**、再带着补充重跑一次。
   实测一轮 53s 的场景下，append 总耗时约 100s，restart 约 70s
   （restart 立即掐断，只浪费已跑掉的那一段）。
2. **存在「注入成功但没人消费」的窗口**。同上，最后一次取用之后
   本轮才真正结束的话，补充就静静躺在通道里被丢弃。
   实测该窗口约 0.7s（生成完成 → 出站 → 笔记落库 → `OnMessageDone`）。
   治理：`BotService.UnregisterMessageInterrupt` 注销前把**没被取走**的
   补充留进 `messageAppends`，`TakeUnconsumedAppends` 取回；
   Channel 侧 `watchAppend` 等本轮**确实结束**后再判定 ——
   取回有内容就退化成新一轮，取回为空说明已被消费、什么都不做。
   ⚠️ 本轮**仍在跑**时绝不能取（那些内容马上会被消费，
   取走再开一轮就是重复处理，用户会收到两条回复）。

**流式编排必须在每个 step 边界取用补充。**
`OrchestrateGenerate`（非流式）在循环开头每步都 drain，但 `OrchestrateStream`
曾经只在「本步没有工具调用、即将收尾」的分支里 drain 一次。长工具链轮次
里那个分支可能一次都不命中，补充会留在通道里直到本轮结束被丢弃。
`AppendToMessage` 返回 true 只表示投递成功，不表示已经被消费。

> 「投递成功」≠「被消费」，两者之间可以隔很久，也可能永远不到。
> 排查时不要只看 Channel 侧的注入日志，要看编排侧消费日志
> （`interrupt: user supplement consumed into context`）。

两处修复：

- `OrchestrateStream` 对齐 `OrchestrateGenerate`，**每一步边界**都 drain，
  且固定在 `applyPrepareStep` 之后（刚注入的消息不该被本步 reducer/compactor 压掉）。
- `watchAppend` 的等待上限不再用 `streamMaxAge`（5 分钟，那是企微流式气泡
  的寿命，不是本轮编排要跑多久）。长工具链经常更久，watcher 提前放弃会让
  补充永远没机会被取回。改为 `appendWatchMaxWait`（1 小时），超时给 WARN
  而不是静默 return。

⚠️⚠️ **「本轮还在不在跑」必须以编排侧为准**（`TurnController.IsMessageActive`），
不能靠「出站 Send 来过就摘掉」：

1. `core.Action` **不带 TraceID**，出站侧认不出自己在服务哪一輪；
   在 `Send` 里无差别摘除，会把「下一轮刚登记、本轮才刚出结果」的竞态窗口变成误摘。
2. pipeline 完全可能**决定不回复**（静默），那时根本不会有 Send 回调，
   活跃表里会留下一堆僵尸项 —— 后续消息一律被判成「打断一个已经结束的轮次」，
   白推一条「（这条已被新消息打断）」，用户的「停止」也会被吃掉。

配套的两个细节：

- **轮次在 `deliver` 里登记**，不在 `handleMsgCallback` 里：配文暂存的那 3s 里
  pipeline 还没起，那时就登记，会让随后的消息去打断一个**根本没在跑**的轮次。
- **bot 忙时不走配文合并窗口**（`holdWorthy`）：打断与「停止」必须即时生效，
  再等 3s 用户早把「停止」发了三遍了。`startsNewTurn` 同理决定要不要开流 ——
  终止指令要复用旧流（finish 时原位替换成「已停止。」），给它开新流会多冒一个气泡。

终止指令**整句完全匹配**才生效：「停一下再说」「请停止输出」都不是终止指令，
否则用户一开口带「停」字就被掐断，体验灾难。内置词表见 `defaultStopCommands`，
可用 `stopCommands` 追加。

### 9.2.11 边生成边推正文（B 档，2026-10-22）

**为什么必须做**：只有「正在思考…」时用户是**盲等**的。实测一轮 17~55s，
期间完全不知道 bot 在往哪个方向写；等最终结果出来才发现方向错了，
那时整轮已经白跑 —— 这正是「正在思考的时候我并不知道他想要说啥，
只有看到输出的时候我才知道方向错了、想重新引导」这条反馈的根因。

做法：订阅 EventBus 的 `llm.text_delta`（`agent/stages/llmroute.go` 发布），
按 2s 节流把累积正文推给同一条流（`feedStream` → `pushStreamContent`）。

| 项目 | 取值 / 结论 |
| --- | --- |
| 数据源 | `agent/outbound.MemoryEventBus` 的 `EventLLMTextDelta`，由 api 层注入（`SetDeltaSource`） |
| 内容 | **已清洗**：串过 reply-control 过滤与出站清洗（`<think>`/`<internal>` 丢弃、`<public>` 剥标签），与 Web SSE 推给前端的是同一份，**channel 侧不用再洗** |
| 节流 | `streamDeltaIntervalMs`，默认 2000；`<0` 关闭（退回纯占位） |
| 尾标 | 增量一律带「（还在写…）」—— 原位替换下没有尾标分不清「还在写」还是「就写这么短」 |
| 互斥 | 推过正文后（`st.fed`）占位刷新**必须停**，否则「正在思考… Ns」会把正文覆盖回去、内容倒退 |

⚠️⚠️ **订阅泄漏（实现时踩到，必须留着看）**：`MemoryEventBus` 的订阅**只在
`Unsubscribe` / `bus.Close` 时关闭**，轮次结束**不会**关。而 `feedStream` 阻塞在 `<-ch`
上、`stop()` 又是 `defer` 的 —— 只靠「通道关闭」退出的话两者互相等待，
**goroutine 与订阅双双永久泄漏**。订阅上限 `MaxSubscriptions=1000`，
按每条入站消息漏一个的速度，一千条消息后 `Subscribe` 开始返回已关闭订阅
→ **Web SSE 全线失效**。

治理：`feedStream` 自带存活看门狗（1s 轮询 `IsMessageActive`），轮次一结束就退订；
再加 `streamMaxAge` 硬兜底（`turnCtl` 未注入时的唯一出口）。
回归测试 `TestFeedStream_UnsubscribesWhenTurnEnds` 用**永不关闭**的通道验证：
去掉看门狗它会 2s 超时失败（已实测确认）。

⚠️ **宽限期 `deltaStartGrace`（5s）不能省**：`feedStream` 起在 `Ingress.Receive`
**之前**，那一瞬间 `IsMessageActive` 必然是 false（编排侧还没登记）。
没有宽限期的话看门狗第一轮就把自己判成「轮次已结束」退出 —— 增量一条都推不出去。

⚠️⚠️ **feedStream 必须锁定「自己那条流」（`mine`），不能每次 flush 都取当前流**：
restart 策略下旧轮被 abort 后它自己的 feedStream 还活着（`IsMessageActive` 要等
`OnMessageDone` 才转 false，可达数秒），而新轮的流这时已经开了 ——
每次取当前流的话，旧轮会把**上轮残稿**推进新轮的流，把新轮刚推的「正在思考…」直接盖掉。
修法：首次 flush 时锁定，之后只往它推；发现推不动（流被顶掉）就立即退出。
回归测试 `TestFeedStream_DoesNotClobberNewerStream`。

**工具活动行**（`streamToolActivity`，默认开）：`llm.tool_call` / `llm.tool_result`
也转成增量，气泡里以引用块显示「🔧 正在调用 X」→「✅ X 完成 / ❌ X 失败」（**就地改写**，
不是追加新行 —— 两行并存看起来像卡住了）。

⚠️ 为什么值得做：实测一轮里前 15s 是 thinking（被丢弃）、正文要到最后才流出来，
**那段空窗原本一个字都看不到**，而工具调用恰好落在里面 —— 它是空窗期唯一的状态信号。
工具执行中尾标是「（工具执行中…）」而不是「（还在写…）」：那一刻 bot 并没有在写。

⚠️⚠️ **只推工具名，绝不推参数与返回值**：`input` / `output` 里可能有绝对路径、命令行、
URL，企微是外部渠道，带上就等于把内部信息写进聊天记录。由 `toWecomDelta` 单点把关，
`TestToWecomDelta_ToolsOnlyCarryName` 盯着这条红线。企微平台是白名单模式（已有规则），
可用的是 `basicTools`（`now`/`calculate`/`memory`/`text_*` 等）与显式放行的 `sandbox_*`。

⚠️⚠️ **同一条流的推送必须串行**（`wecomStream.pushMu`）：`conn.respond` 对同一 req_id
只允许一个在途请求，撞上就报「already in flight」**直接丢帧**。而推送方不止一个 goroutine
—— 占位刷新的 ticker、推增量的 feedStream、Send 的收尾，三者会并发。
实测企微 ACK 偶尔要 5s（2026-10-22 14:55:08 真机丢过一帧刷新），这 5s 里谁撞上谁丢。

- 正文 / 收尾走 `push()`（阻塞等锁）：**收尾帧丢了会让 `Send` 回退成整段发送，用户多收到一个气泡**
- 占位刷新走 `tryPush()`（拿不到锁就放弃）：它是装饰性的，为它去阻塞共享 ticker 会连正文一起拖慢；
  跳过只记 Debug，不打 WARN —— 那是并发让路，不是故障，打 WARN 会淹掉真错误
- 回归测试 `TestPush_SerializesSameStream` 模拟了 conn 的在途检测：去掉锁它会丢 4/5 帧（已实测确认）

⚠️ **「（还在写…）」只是推送尾标，不能存进 `st.content`**：
兜底收尾（`reapStreams` / `abortStreams`）用的正是 `st.content`，
把它定格成最终内容的话，用户会看到一条**已经结束**的消息写着「还在写…」。
所以 `pushStreamContent` 分开存（干净正文）与显（正文 + 尾标）。

### 9.3 真实回调样本

```json
{ "cmd": "aibot_msg_callback", "headers": { "req_id": "<REQ_ID>" },
  "body": { "msgid": "<MSGID>",
            "aibotid": "AIBOTID",
            "chattype": "single", "chatid": "",   // ← 单聊 chatid 为空
            "from": { "userid": "USERID" },   // ← 明文
            "msgtype": "text", "text": { "content": "测试测试" } } }
```

图片消息（含 `response_url` 与解密密钥）：

```json
{ "body": { "msgid": "<MSGID>", "chattype": "single", "msgtype": "image",
            "response_url": "https://qyapi.weixin.qq.com/cgi-bin/aibot/response?response_code=...",
            "image": { "url": "https://...?sign=...", "aeskey": "<AESKEY_43>" } } }
```

文件消息（**没有文件名**）：

```json
{ "body": { "msgid": "<MSGID>", "chattype": "single", "msgtype": "file",
            "response_url": "https://qyapi.weixin.qq.com/cgi-bin/aibot/response?response_code=...",
            "file": { "url": "https://...?sign=...", "aeskey": "<AESKEY_43>" } } }
```

### 9.4 仍未验证

| # | 未知数 | 怎么验 | 状态 |
|---|---|---|---|
| B | **流式每帧是否计入 30 条/分钟** | 连发 40 帧看是否 846607 | 待验（会往会话刷 ~30 条，需维护者批准） |
| C | 30 条/分钟是**会话级**还是机器人级 | 两个会话各发 25 条 | 待验 |
| E | `response_url` 是否支持 `text` msgtype | 换个新回调再 POST 一次 | 待验（低优先，`markdown` 已够用） |

> A（群聊路径）与 D（`response_url`）**已验证**，见 §9.2.2 / §9.2.3。

---

## 10. 分期建议

| 期 | 范围 | 依赖 |
|---|---|---|
| **P0 打通** | 连上 WS + 订阅 + 心跳 + 收文本消息 → Ingress；`Send` 走 `aibot_send_msg`；会话派生；工具权限；前端槽位 | §9-1 |
| P1 稳定 | 重连退避 + `disconnected_event` 停止重连；每会话令牌桶；分片上限；Secret 脱敏；真实实例回归 | P0 |
| P2 增强 | 流式回复（req_id 透传 + `stream` 帧）；模板卡片 + `template_card_event` 5s 响应；媒体收发（aeskey 解密/上传）；`enter_chat` 欢迎语 | P1 + §9-2/4 |

---

## 11. 坑清单（预判，待实测确认）

1. **命名撞车**：thinkbot 的 `BotID` 是 bot 标识，企业微信的 `BotID` 是机器人标识。
   配置字段建议叫 `robot_id` / `aibot_id`，**不要叫 `bot_id`**，否则读代码时极易混淆。
2. **单连接互踢**：thinkbot 重启/多实例会让新连接踢掉旧连接，旧实例收到 `disconnected_event` 后
   **必须停止自动重连**，否则两个实例互相踢、谁也连不上（表现为"机器人突然不回消息"）。
3. **认证失败不可重试**：`aibot_subscribe` 返回 errcode≠0 是凭证错误，重连无意义，应直接报错停止。
4. **`chat_type` 缺省会优先按群聊解析** → 单聊静默发不出去。
5. **`846607` 文案误导**：既可能是真限流，也可能是"该会话从未交互过"。
6. **消息去重靠 `msgid`**：企业微信回调可能重推，不排重会二次回复（Misskey 2026-09-01 同款事故）。
7. **Secret 明文落库**：`BotPlatform.Config` 是明文 JSON 存 config_settings，且现有日志脱敏规则不认它。
8. **群聊必须 @机器人**才触发 → 语义上接近 Mastodon 的 mention，而非 Misskey 的 timeline 旁听。
