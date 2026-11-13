# Agent 可控交互式 CLI 实现方案 v3.7 — 已定稿（P0/P1/P2 全部收口，经六层复核修订）

> 状态：**APPROVED / 已定稿，露娜 11-11「可以开工」后进入实施**（v3.7 为六层复核后的错误修订，**主线与 9 项决策不变**）。设计内容不再变更；代码落地进度与验证结论见 **§0.1**。浏览器实机验收已在**隔离实例**跑完、真实二进制也已按部署 ldflags 重建并短暂验证（见 §0.1 末段）；当前**未 commit、未 push、未启动本机常驻服务、未部署**。
> 主线（露娜提出）：**交互式 CLI 一律 spawn 成后台会话，返回句柄；agent 自己决定何时 poll 输出、何时 send 输入**。不再尝试判断「程序是不是在等输入」。
> 约定：🔖 = 已核实的代码事实（路径:行号）；§13 的 9 个决策点**已全部定稿**，推翻任一项只回退该项。
> **v3.1 增补**：用真实 CodeBuddy Code CLI 做了隔离 PTY / 无 TTY 对照实验（§3.2）。其中一条**推翻了本方案原定「PTY 合并 stdout/stderr、`merged:true` 即可」的取舍** —— 真实 CLI 的启动期致命错误在 PTY 下被吞成「45s 零输出但进程存活」。§5 已改为「PTY 给屏幕、侧信道给诊断」。
> **v3.2 增补**：按文档契约写了最小 PoC harness（§3.3），用 `spawn/poll/send` 真实驱动 CLI 走到登录并抽出带一次性 `state` 的登录 URL。主线逐条证实，但**实测打出四条 v3.1 没覆盖的坑**（跨 chunk ANSI/UTF-8 断裂、部分重绘放大、ANSI 洪水折叠不达标、**env 全量继承导致子进程被宿主产品配置劫持**），另补上「带外交互」这一整类场景。`pending #4` 已定稿。
> **v3.3 增补**：露娜要求验证「bot 不只是知道里面干啥，还能从外面操作它」。§3.4 用已登录会话发真实指令驱动 CLI 执行工具、应答它内部弹出的权限确认屏、连续跑两轮，完整闭环。新增「嵌套交互」「确认类按键策略」「禁止脚本化 send」三条设计约束。
> **v3.4 增补**：露娜要求「继续没验证的地方确保方案真实可行，还有现在都在背景运行了怎么侦测是否卡住」。补完四类实测：① 确认屏选项 2/3 分支；② `signal(INT)` 语义被实测**推翻**（是终止会话，不是中断任务）；③ **后台会话 health / stuck detection**（§3.5）——新增 `poll.health` 契约，构造样本 8/8、真实 PoC 回放、真实 CLI 逐帧复验，并明确「输入框可见时的冻结不可区分」这条边界；④ **channel 主路径**（§3.6）——Go 探针实测同连接被 `rpc` 锁串行 `2.599s`、独立连接 `283µs` / `52ms` 并发成功。另修三条实测打出来的判据坑：静态帮助文字冒充确认屏、语义归一化洗掉 `>` 提示符、归一化数字让 `screenStableMs` 虚高。
> **v3.5 增补**：露娜补了一句「其实还需要一个 manager 来统筹控制这些后台运行的工具」，随后纠正 **「是进程」不是工具** —— 这个纠正把落点从「工具注册表」拉回**后台进程的所有权与生命周期收口**。新增 §3.7：复用仓库现成的 `procgroup_unix.go` / `channel/reaper_unix.go` 作为唯一 kill / reap 路径，补上 `DestroyBot` / `StopBot` / `CloseAll` 不关进程的真实缺口；`watch` 作为 agent 侧唯一注意力入口（纯查询、不替 agent 决策）。多会话 PoC 实测：1 个真实 CLI + 构造样本下 watch 排序、botID 隔离、per-agent cursor、closeAll 级联全部通过；**并实测证实「进程组 kill ≠ 收干净」**（`setsid()` 孙进程逃逸，注册表已空但进程仍活着）。
> **v3.6 定稿**：露娜「按你建议」→ 按文档「露娜无异议即按此执行」条款，§13 的 9 个决策点全部按建议定稿；**#9 采纳方案 ②**（容器内 idle TTL 自杀、宿主一律回 `state=lost` 让 agent 重开，不做握手认领）。同步收敛 toolperm 风险等级、§13 标题与实现默认值表。⚠️ 定稿的是**设计决策**，不是验证结论：`sandbox.backend` 未用运行中实例复核、channel 真实容器主路径未跑，这两条仍挂在 §11 上线前验收里，不因定稿而消失。
> **v3.7 六层复核修订**（露娜「整体 review 下整个方案，看看还有没遗留的点和错误的地方」）：按「方案落地前六层核查」逐层回源代码事实，跑基线测试，**挖出 3 个 P0 硬阻塞 + 一批事实/契约错误**，已就地修正；新增 §14 风险章节。三条最重的：① **`spawn` 工具名已被 `subagent/tools.go:90` 占用**，动态 provider 会被 `registry.go:151-153` 静态优先去重吞掉 → §4 定稿全名 `sandbox_session_*`；② **会话回收有洞**：abort 链完全没接，且 §3.7 把 idle TTL 定成「只在 `watch`/`spawn` 时 lazy GC」，而 §3.7 自己又承认 abort 后没人 watch → **15min TTL 在最需要它的路径上永不触发**，修订为「进程级 TTL 由已有 readLoop 顺带执行，零新增 goroutine」（§3.7 新增小节）；③ **`allowUnsafe` 是文档凭空造的参数**（全仓零匹配），且 `toolperm.Evaluate` 只吃工具名、拿不到屏幕上下文 → 「确认屏按键动态升级」机制不成立，改为工具级静态裁决（§3.4 / §9）。另外修正 6 处代码事实错误（`botworkspace.go` / `docker.go` / `services.js` / `tool_truncate.go` / `run_code` / toolperm 登记表）、补齐 `SessionView` 缺的 `Reason`/`LastSeq`、补 seq 与稳定窗口对齐、幂等性、配置化链路、新工具清单同步点与「新增字段消费方表」。⚠️ 本机 `go test -count=1 ./sandbox/...` **基线不是绿的**（8 个 darwin 环境限制失败，与本次无关），已写进 §11 / §14。
> **v3.8 增补（实现期，不改任何定稿设计）**：露娜「有个用来控制 ssh 的场景也要实现下……这个场景非常常见 通过的话一般场景都能覆盖」。用真实 `ssh -tt` 登录做完端到端验收，补上机密输入治理、凭据 / 连接失败 verdict、health 当前行判据、Ctrl+C 后发送节奏；**并纠正文档初稿里凭空造出来的 `secret=true` 参数**（仓库已有 `{{secret:name}}` 咽喉点）。详见 **§0.2** 与 §14 #16–#18。

---

## 0. 结论先行

前两版把「检测 `awaiting_input`」当成核心，对 CodeBuddy / Claude Code 这类**常驻 TUI agent** 是走偏的：它们一直在工作、一直刷屏、同时用事件循环兼听 stdin，**根本不存在「停下来等你」的时刻**。等待判定既无解，也没必要解。

正确模型是：**不要通知 agent「现在轮到你了」，而是给 agent 一个随时可看、随时可写的句柄**。LLM 读屏幕判断「这里有个 y/n 要不要按」比任何启发式都准。

**关于「怎么知道 stdin 被监听」（§3.1）：查不准，也不需要查。** 实测「在监听」与「完全不碰 stdin」的两个进程外部信号逐项相同；Linux 上只有 epoll 路径能通过 `/proc/<pid>/fdinfo` 的 `tfd: 0` 确定，poll/select 只能瞬时采样，macOS 基本不行。所以架构不设这道门禁：句柄永远可写，是否发输入由 agent 看屏幕决定。

三句话：

1. **启动即建会话**：进程在 PTY 里 `Start()` 后立刻起独立 readLoop 持续消费输出，工具**立即返回** `sessionId`（+ pid），不 `Wait()`、不等退出。
2. **daemon 持续攒 buffer**：原始字节 ring buffer + 降噪视图 + VT 屏幕网格，带单调递增 `seq` 水位线；后台进程跑多久都不阻塞任何一次工具调用。
3. **agent 自主循环**：`poll(sid, sinceSeq)` 拿增量自己判断 → 要操作就 `send(sid, text/keys)`，不需要就放着、甚至去做别的事 → 最后 `poll` 到 `exited` 取 exitCode，`close` 释放。

`llm/orchestrate.go` 仍然一行不动：每次工具调用快速返回，天然适配现有「同步 Execute → 下一轮 ToolMessage 回灌」。

**一条实测加进来的硬约束（§3.2）**：PTY 合并 stdout/stderr 会把启动期致命错误吞成「零输出但进程 alive」，而同一失败在 pipe 下是 `exit 1` + 一行 stderr。所以「屏幕没东西」既不是「在等输入」也不是「正常工作」，**绝不能当 auto 升级或任何状态判定的正面证据**；会话必须额外保留一条独立于 PTY 字节流的诊断侧信道。

**主线已被 PoC 跑通（§3.3）**：按本方案契约写的 harness 用 `spawn → poll → send(Enter) → poll → send(Enter) → poll` 真实驱动 CLI 穿过两个交互屏、走到登录并抽出带一次性 state 的登录 URL，全程没有一次 `Wait()`。`state=running` 允许 send、会话可放着不管、看门狗不误杀、BootDiag 抓到屏幕上看不到的 `401` 均获证实。同时实测打出三条 v3.1 没写透的坑（跨 chunk ANSI/UTF-8 断裂、部分重绘放大、ANSI 洪水折叠不达标），已在 §4 / §5 / §11 补齐；另发现「带外交互」这一整类场景（真正的交互在浏览器里，agent 该转交链接而非按键），已补进 `urls` 契约。

**「从外面操作」也已闭环（§3.4）**：往已登录 CLI 的输入框 `send` 一条真实指令 → 它执行工具并弹出**内部权限确认屏** → 外部 `send` 应答 → `poll` 拿到执行结果；同一会话再跑第二轮无确认屏的任务也成功。嵌套交互用同一套 `send/poll` 递归解决，无需状态机；两轮对比还证明确认屏时有时无，只能读屏决策、禁止脚本化 send。

**后台会话「是不是卡住」不能靠单一信号（§3.5）**：全部会话进后台后，「`procAlive=true` 且零输出」至少有五种含义 —— 等 agent 输入、等人做带外授权、在忙、spinner 假活、真卡死。daemon 内部可观测的 `seqRate / changedPerChunk / screenStableMs / actionable / inputReady / waiting / readLoopAlive / procAlive` 足以把它们分开：真实 PoC 回放里假活窗口 `changedPerChunk≈1.00`（单行 spinner）或 `0.00`（纯控制序列重绘），正常进展 `1.60~6.00`；构造样本 **8/8** 命中，真实 CLI 逐帧复验通过。`health` 只是 `poll` 的诊断 hint，**不自动杀进程、不改状态机**。

**一条必须写死的边界**：当屏幕上仍显示输入提示符 `>` 时，进程被冻结与健康空闲在 PTY 侧**完全同形**，health 只能给 `idle:awaiting_agent`，不能宣称「没卡住」。要区分必须引入有副作用的探针或 OS 级进程状态（Linux `/proc`、macOS 不等价），列为 P3 可选，不进首版。

**channel 主路径已用 Go 探针实测（§3.6）**：`client.go` 的 `rpc` 独占锁让同一连接在 `Exec` 期间的所有 RPC 串行 —— 同连接 `Info` 被阻塞 `2.599274s`，独立连接 `Info` `283µs`、独立连接 `Exec` `52.679ms` 且真的拿到输出，总墙钟 `3.001s` 证明两次 Exec 并行。方案里「每会话一条独立连接 + ReqID 分发」是硬性约束，不是臆测。⚠️ 本机 darwin 命中 `reaper_other.go`，channel 的 `Exec` 在 macOS 上直接返回 `process control requires unix`，故探针用受控延迟的协议对端测 client 侧锁；**真实容器 daemon 仍需线上 Linux 环境验证**，步骤写在 §11。

**统筹层是进程所有者，不是工具目录（§3.7）**：露娜指出还需要 manager，并纠正「**是进程**」。所以新增的不是「第 6 个工具的注册表」，而是**唯一持有后台进程所有权**的 `SessionRegistry`：复用 `procgroup_unix.go`（Start 前 `Setpgid`）与 `channel/reaper_unix.go`（subreaper + `Wait4` + `SignalGroup` + `Forward`），把 `DestroyBot` / `StopBot` / `CloseAll` 补上级联关闭 —— **精确说法见 §2 修订**：`CloseAll` 确实只清内存表，`DestroyBot`/`StopBot` 在 docker 持久容器后端会 `docker rm -f` / `docker stop -t 10` 连带停进程，但普通 `POST /api/bots/:id/stop` 根本不调用这两个方法，主路径仍会留孤儿。agent 侧只多一个只读 `watch`：把 N 个会话按 §3.5 的 health 收敛成注意力队列，并替 agent 记 per-agent `sinceSeq`；**manager 不替 agent 决策、不自动 send、不因 health 自动杀进程**。多会话 PoC（含 1 个真实 CLI）实测排序、隔离、cursor、级联通过，并实测证实「**进程组 kill ≠ 收干净**」：`setsid()` 孙进程在 `closeAll` 后仍活着，而 `watch.total=0`。

**v3.7 六层复核改动了什么（详见 §14）**：主线、9 项决策、所有 PoC 结论一律不变；改的是「照这份文档实现会踩空」的地方 —— 工具全名（`spawn` 撞名被吞）、会话脱离回合 ctx 之后的兜底回收（lazy GC 在 abort 后不触发）、toolperm 静态裁决的边界（`allowUnsafe` 不存在、拿不到屏幕上下文）、6 处代码事实与行号、`SessionView` 契约缺字段、seq 与稳定窗口的字节/语义对齐、幂等性测试、配置化落点，以及「新增字段谁消费、在渲染层哪里看得到」。§14 专门回答 skill 的最后一问：**照方案实现最可能错在哪**。

### 0.1 实施状态（2026-11-11，「可以开工」之后）

> 本节只记录**代码落地进度与验证事实**，不改动上面任何定稿设计。

**已落地 P0（本地 PTY 主链路）**

| 落点 | 文件 |
|---|---|
| 会话契约、状态 / reason / health / 双水位线 | `sandbox/session_types.go` |
| 屏幕与输出治理（ring / 跨 chunk ANSI / VT / deltaLog / health / env 白名单） | `sandbox/session_buffer.go` |
| **local 与 channel 共用的状态机**（`consume` / `settle` / `Poll` / `view` / `markFinished` / `markLost`） | `sandbox/session_core.go` |
| 本地 PTY 后端、进程组、常驻 readLoop、进程级 idle TTL | `sandbox/session_local.go`（+ windows / alive stub） |
| 唯一进程所有者、botID 隔离、cursor、watch、orphaned、CloseAll、lazy GC | `sandbox/session_registry.go` |
| 7 个 `sandbox_session_*` 工具 + 提示词 + SubAgent 屏蔽 | `sandbox/tools_session.go`、`sandbox/tools.go` |
| 7 处清单同步 / toolperm 分级 / tool_guard 豁免 | `api/handler_bot_toolperm.go`、`workflow/profile.go`、`toolperm/risk.go`、`llm/tool_guard.go` 等 |
| 配置化五处链路 + abort 打 orphaned + StopBot / Bot.Close 级联回收 | `config/keys.go`、`config/module.go`、`api/botservice.go`、`agent/bot/bot.go`、`sandbox/wire.go` |

**已落地 P1（channel 后端）**

| 落点 | 文件 |
|---|---|
| `MsgSession*` 帧（100-108）+ `MsgSessionTouch`(109)、`IdleTTLMs`、`session.spawn` capability | `sandbox/channel/protocol.go` |
| daemon 侧 runner：Linux PTY、非 Linux 明确 unsupported、每会话连接、touch 刷新 + **容器内 idle TTL 自杀** | `sandbox/channel/session.go`、`session_linux.go`、`session_other.go`、`server.go` |
| client 侧每会话独立连接 | `sandbox/channel/session_client.go` |
| 独立连接拨号（`DialSessionEndpoint`）、`channelDialParams` 抽成唯一解析路径 | `sandbox/channelclient/client.go`、`sandbox/botcontainer.go` |
| host 侧 channel 会话 + `routingSessionSpawner`（channel bot **绝不**回退宿主 PTY）+ 容器 env 构造 | `sandbox/session_channel.go`、`sandbox/session_buffer.go` |

**实施中打出来并已修掉的 5 个真实缺陷**（都是「机制存在 ≠ 机制生效」的又一实例）

1. `SessionClient.readLoop` 的 defer 只 `markExit`、**不回调 `OnExit`** → 连接断开后宿主永远停在 `state=running`，而 `health` 已显示 `exited`。fake daemon 单测直接复现，现改为 `markExit` 返回是否首次并对掉线发布 `CodeIO` 退出。
2. `SessionSpawner.Spawn` 原签名**没有 botID**，channel 后端无法定位 per-bot 容器 / socket / HKDF token。接口改为 `Spawn(ctx, botID, req)`，botID 由 registry 注入（模型永远传不进来）。
3. `buildSessionEnv` 会把**宿主的 `PATH`/`HOME`/`SHELL`** 写进容器子进程 env，覆盖 Linux 容器自身值。新增 `buildContainerSessionEnv`：只传显式 extra + `TERM/COLUMNS/LINES`，容器 base 由 daemon 的 `childEnv()` 提供。
4. 定稿 #9 的「容器内 idle TTL 自杀」原本缺判据：容器看不到 host 的 `poll`。新增 `MsgSessionTouch`（poll 时节流 1s 发一次，stdin/output 也会刷新 `lastActivity`），host 消失后容器自行 TERM 进程组并回 `Code=idle_ttl`。
5. **Linux 真实容器实验才打出来**：交互式 shell 忽略 SIGTERM，而 `sessionRunner.stop()` 原本只发 TERM、没有 KILL 兜底 → `close` 后会话进程仍是 `R`。此前用 `cat` / `sleep` 做样本时都响应 TERM，正好蒙混过关；换成 `exec sh` 立刻暴露。现改为 TERM 后在 500ms 内轮询 `sessionGroupAlive(pgid)`（`kill(-pgid, 0)`），仍存活即同步 KILL。**同步而不是 `time.AfterFunc`**，是为了不让延时窗口里被复用的 pgid 误杀无关进程组；`sessionGroupAlive` 只在 Linux 实现，非 Linux 恒 false。与 local 后端 `terminate()` 的 TERM→KILL 语义对齐。

**验证结果**

- `go build ./...` ✓；`GOOS=linux/windows go build ./sandbox/...` ✓（Linux PTY 分支在本机从未参与编译，已交叉编译验证）；`go vet` 关键包 ✓。
- 新增单测全绿：local 端到端 / idle TTL 无需 agent 再调用 / registry 隔离与 watch 只读（3），channel 输出+stdin+正常退出 / 掉线 `state=lost` / daemon idle TTL / spawn 失败不注册 / 路由不回退（5），buffer 幂等 / 双水位线 / health 判据顺序 / env 脱敏 / 工具名不撞车 / SubAgent 屏蔽（10+）。
- `channelclient` / `toolperm` / `workflow` / `llm` / `api` / `agent/bot` 全绿。
- **基线纪律**：`sandbox` 8 项、`sandbox/channel` 2 项在本机 darwin 仍是红的（confinement / `process control requires unix` / `process list requires linux`）；已用 `git archive HEAD` 的干净快照逐项对照，**完全一致 → 本次改动零回归**。

**Linux 实例实验（11-12，隔离环境，线上服务/容器全程未动）**

- **§11 要求的 Linux 绿灯存档**：`go test -count=1 ./sandbox` 18.2s 全绿、`./sandbox/channel/...` 与 `channelclient` 全绿 —— darwin 的 8+2 个环境性失败在 Linux 上全部消失；`go build ./...` 在 Linux / Go 1.27 通过（新依赖 `creack/pty` 正常下载）。
- **真实 Linux daemon + PTY 端到端**：独立 `channel.Listen` + `DialSessionEndpoint` 起 `sh`，`tty` 回 `/dev/pts/0`、计算标记 `MARK42=42` 证明是真 PTY 而非 pipe（标记用 `$((6*7))` 计算，避免被 PTY 的输入回显骗过）；close 后 `/proc/<pid>` 消失（Reaper 回收、无僵尸）；不发任何 poll/send 时容器侧 idle TTL 自杀并回收；daemon 关闭后 `state=lost` / `reason=conn_lost` / `lastSeq` 齐备。
- **真实 Docker 容器 daemon**：用实验代码编译 `thinkbot-channel` 作 PID 1 起**隔离容器**（独立 socket 目录 / volume / 容器名、`--network none`），宿主侧连 socket 起会话，拿到 `CTR=/dev/pts/0 C42=42`，close 后 `state=exited` / `reason=close`；复跑两次，等 2s 后 `docker top` 只剩 daemon PID 1。⚠️ 线上运行中的容器是**旧镜像**（不支持 `MsgSession*`），所以必须新编译独立容器验证，直接拿线上容器试只会得到 `CodeUnsupported`。实验容器与临时文件已清理，线上服务仍 active、线上仓库工作区干净。

**`sandbox.backend` 运行中实例复核（已完成）**

- DB `config_settings` 无 `sandbox.backend` / `sandbox.image` 显式键 → `botservice.go:sandboxConfigWithMemoryLimit` 不覆盖 `DefaultConfig()`（`Backend="auto"`、`Image="builtin"`）；`auto` 在该实例 Docker 可用 → 实际 `docker` + `PersistentContainer` + `isBuiltinImage` → **channel 主路径**。运行中 bot 的容器接口显示 `image='builtin'` / `containerStatus='running'`，对应镜像为 `thinkbot-bot:<hash>`。**文档「channel 是实际主路径」的假设已坐实**。
- ⚠️ 附带发现：`GET /api/config` 的 `GlobalMetaSpecs()` 刻意只暴露 `sandbox.cpu_limit`，不暴露 backend/image；而展示默认值与 `DefaultConfig()` 的运行时默认值存在漂移（例如 CPU 展示 `16`、代码默认 `1.0`）。不影响本次结论，但配置页会误导，列为独立后续项。本次已做**防御性对齐**：`config/module.go` 的 `DefaultMap()[KeySandboxImage]` 由 `alpine:latest` 改为 `builtin`（与 `sandbox.DefaultConfig().Image` 一致，`docs/configuration.md` / `sandbox/README.md` 本来就写 `builtin`），并修正 `api/botservice.go` 里「回落到 `alpine:latest`」的过期注释。⚠️ 依 §14 #4，`DefaultMap()` 只喂 `GET /api/config` 展示，**不改变运行时行为**，此次改动纯粹防止将来把该键加进设置页时再次误导。

**P2 后端前置契约已落地（11-12，为「另外两个问题」收口时补）**

核对前端接管链路时又打出 **3 个真实缺陷**，都已修掉并单测验证：

1. **raw 游标错位**：`SessionRegistry.Poll(mode="raw")` 的 `sinceSeq` 会被 `Poll` 夹到 `settledSeq`，而 registry 的 per-agent cursor 存的也是 `SettledSeq` —— WebSocket 每帧按 `Seq` 取增量会**重复回放未结算字节**。新增 `RawChunk` 契约 + `SessionRegistry.RawSince` / `sessionCore.RawSince`：走 `Seq` 水位线、绝不碰 agent cursor、ring buffer 溢出时回 `Resync=true` 让前端 `term.reset()` 后重放快照。
2. **resize 只改子进程、不改 daemon 网格**：`localSession.Resize` 只 `pty.Setsize`、`channelSession.Resize` 只发帧，`sessionCore.screen` 仍是 spawn 时的 `cols×rows`，`view()` 默认行数还写死 `cfg.ScreenRows` → 前端一拉窗口，agent 读到的 `Screen` 与人看到的终端**尺寸不一致**。新增 `resizeScreen()`（用 `RenderScreen(raw, cols, rows)` 幂等重建，不重置 live decoder 的 pending escape/UTF-8 状态），两个后端接线，`view()` / `View()` 改用实时 `s.rows`。
3. **`vtScreen` 数据竞争**：`consume()` 在 readLoop goroutine、`view()` 在 poll / WS goroutine，网格无锁。补 `bufMu`（锁序固定 `mu → bufMu`），并加并发回归测试。另加只读 `SessionRegistry.List`：会话列表**不能**复用 `Watch`，因为 `Watch` 会顺带 `GC()`，刚退出的会话可能在人打开回放前被清掉。

验证：`go build ./...` ✓；`GOOS=linux/windows go build ./sandbox/...` ✓；新增 7 个会话契约单测 ✓，`go test -race` 专门覆盖并发读/poll/resize ✓；`config/api/toolperm/workflow/llm/agent/bot/channelclient` 全绿 ✓；`sandbox` 8 项 + `sandbox/channel` 2 项仍是**已知的 darwin 环境性失败**，与 §11 基线一致、零回归。**逐步可执行路径见 §10.1。**

**P2 前端接管已实施（同日，「继续完善」之后）**

| 落点 | 文件 |
|---|---|
| `GET /api/bots/:id/pty-sessions`、`DELETE .../:sid`、`GET .../:sid/stream` | `api/handler_bot_pty_session.go`、`api/router.go`（`botsAdmin` 组） |
| 前端 API：`list` / `streamUrl` / `remove`，删掉未实现的 ws TODO | `web/src/api/services.js`（`botPtySessionApi`） |
| 实时 xterm 控制台（二进制帧 / reset / state / input / resize / 卸载不关会话） | `web/src/components/common/SessionConsole.vue` |
| 会话列表 + 接管 + 独立「关闭会话」 | `web/src/components/bot/BotPtySessions.vue` |
| Bot 设置新增「交互会话」tab（支持 `?tab=pty-sessions` 直达） | `web/src/views/BotSettings.vue` |
| store 15s 并行轮询 + 侧栏系统级「后台交互会话 ×N」徽标 | `web/src/stores/bot.js`、`web/src/components/SessionList.vue` |
| 端点单测 / 真 PTY 端到端（含 race） | `api/handler_bot_pty_session_test.go`、`api/handler_bot_pty_session_integration_test.go` |

⚠️ **实施时又被实测纠正 3 处**（详见 §10.1 实施记录）：① **`GET /api/bots/:id/sessions` 已被聊天会话 CRUD 占用**（`api/router.go:263`），§10.1 原稿路径会撞车 → 全部改到独立命名空间 `pty-sessions`；② `ringBuffer.Since` 的 `truncated` 是**粘性**的，直接拿它当 `Resync` 会让 xterm 在首次溢出后**每 50ms 清屏一次** → 改为按 cursor 判 gap（新增回归测试）；③ `resizeScreen` 的 cols/rows 来自浏览器，未设上限可撑爆网格 → 加 1000×500 上限。另补 REST DELETE，让列表按钮不必为了关会话先开一条 WebSocket。

**验证（P2）**：`go build ./...` ✓；`go test -race -run 'TestPtySession|TestHandleListPtySessions' ./api` ✓；`go test -count=1 ./api` 全绿；**真 PTY 端到端**：`httptest` + `NewBotWorkspaceManager(Backend=local)` + 真实 `sh` 进程 + WebSocket，输入 `PING42` 拿到 `GOT:PING42`、resize 后仍响应、显式 close 收到 `state=exited` 且注册表归零；前端 `npm run build` ✓（prebuild 模板绑定检查全过）、`npm run test:regression` 10 个脚本全过；`sandbox` 8 项 darwin 环境失败**逐项与基线一致**、零回归。

**P2 剩余两项已实施（同日，「继续把差的部分完成」之后）**

§11 P2 行里挂着的 `background:auto` 自动升级与 `watch` 事件唤醒，此前一直标「未做（独立 agent 侧能力）」。本轮补齐，**两项都没有改 `llm/orchestrate.go` 主循环、都没有绕过 toolperm**。

| 落点 | 文件 |
|---|---|
| auto 升级三分决策（退出→一次性 / 有输出→句柄 / 零输出→诊断）、PTY 字节纯文本化 | `sandbox/session_auto_upgrade.go` |
| `sandbox_exec` 的 `background=sync\|auto` 参数 + 调用期权限复核 + 提示词 | `sandbox/tools.go` |
| **复用 toolperm 同一裁决**的 `ToolManager.AllowsTool` / `ToolAccessProbe`（含 evaluator 实现） | `agent/tools/manager.go`、`toolperm/service.go` |
| watch 长轮询：广播 changeCh、只读快照、稳定窗口节流、SSE 心跳 | `sandbox/session_watch.go`、`sandbox/session_core.go`、`sandbox/session_registry.go`、`sandbox/tools_session.go` |
| 只读 `SessionRegistry.View`（不 Touch、不推进 cursor） | `sandbox/session_registry.go` |
| 3 个新配置键（五处链路：keys / module specs+DefaultMap / botservice.applySessionConfig / SessionConfig+defaults / fillSessionDefaults+wire） | `config/keys.go`、`config/module.go`、`api/botservice.go`、`sandbox/session_types.go`、`sandbox/sandbox.go` |
| 单测：fake 决策 9 项 + 真 PTY 端到端 4 例 + watch 8 项 + toolperm 一致性 3 项 | `session_auto_upgrade_test.go`、`session_auto_upgrade_integration_test.go`、`session_watch_test.go`、`toolperm/allows_tool_test.go` |

**两个设计决策（都是被 §14 六层框架逼出来的）**

1. **auto 升级不能自己判断权限**。它会创建后台 PTY 会话，等价于 `sandbox_session_spawn` 的能力。若规则「允许 exec、禁止 spawn」，从 exec 内部直接调 `reg.Spawn()` 就是一条提权后门。做法：`BotWorkspaceToolProvider` 持有 `ToolManager`，在 `Tools()` 期用 toolperm 的同一个 evaluator 裁决 spawn 是否放行；**不放行时 `background` 参数根本不出现在工具 schema 里**（不是「出现了但运行时报错」），且在 `Execute` 期再复核一次（防规则在 Resolve 之后变更）。绝不另造一套权限判断：`agent/tools.ToolAccessProbe.AllowsTool` 直接复用 `evaluator.allow()` 的 isSystem / isSubagent / broadcast 豁免；有单测断言它与 `FilterTools` 对同一 sctx 结论逐项一致。
2. **唤醒是工具内长轮询，不是回合外主动唤醒**。§11 原文写「复用 `OnMessageStart` / SSE」，但 §10.1 同时警告别把 SSE 唤醒塞进前端链路、且「不得绕过 toolperm」。真正落地为：`watch(waitMs)` 在一次**已经过 toolperm 调用期复核**的工具调用内阻塞等待；SSE 只走既有 `SendProgress` 推心跳，**不注入消息、不代调任何工具、不新起 agent 回合**；agent 醒来后自己用 poll/send/close（各自独立裁决）。「agent 不在回合里时主动拉起新回合」需要动编排层并会绕过 agent 决策，仍明确不做 —— 这是边界，不是漏做。

**实施中又打出来并已修掉的 4 个真实缺陷**

1. `WithoutCancel` 的 ctx 一开始传给了整个观察循环，abort 时长轮询不会中断，与注释声称的行为相反 → 改为**只在 `Spawn` 那一句**脱钩，观察循环继续用原 ctx。
2. 零输出诊断分支把 `Aborted=true` 传给 `execResultToToolOutput` → 复用出「被信号杀 / 超时」的误导性可靠性文案。改为只有 `state=lost` 才 `Aborted`，零输出是「主动判定为启动失败」。
3. ctx 取消路径显式 `Close` 一次、`autoOneShotResult` 内又 `Close` 一次 → 重复关闭。统一由一次性结果分支做**唯一** Close。
4. 观察循环原本把 timer 置 `nil` 后下一轮 `select` 会解引用 `nil.C` panic → 重写为「短窗口 + 诊断上限」两阶段绝对 deadline，并补 `WatchWaitMax` 零值兜底（零值 cfg 会把等待夹成 0）与稳定窗口节流（纯重绘流否则空转）。

**验证（本轮）**

- `go build ./...` ✓；改动包 `GOOS=linux GOARCH=amd64 go build` ✓；`gofmt` / `go vet` 干净；`web/npm run build`（含 prebuild 模板绑定检查）✓。
- 新增单测全绿：auto 决策 9 项、**真实 PTY 端到端 4 例**（短命令不丢输出 / 长进程升级后 `poll` 能读到已收输出 / 零输出 alive 不升级且不留孤儿 / cwd 生效）、watch 8 项、toolperm 一致性 3 项；`go test -race` 覆盖新逻辑与真 PTY ✓。
- `toolperm` / `agent/tools` / `config` / `api` / `workflow` 全绿；工具名清单、SubAgent 屏蔽、risk 分级、profile 档位相关测试全绿 → 无清单漂移。
- ⚠️ 本机 `sandbox` 8 项 + `sandbox/channel` 2 项仍是 §11 记录的 **darwin 环境性失败**（`confinement is not available on darwin` 等），逐项与基线一致、零回归；Linux 全绿存档见上文。

**浏览器实机验收（11-12，隔离实例，已完成）**

不覆盖真实二进制 / 不碰项目根真实数据库 / 不占默认服务端口：临时二进制 + `/tmp` 独立目录（`.env` 改 `api.addr` 到隔离端口 + 独立 `DB_PATH` + `static` 软链）+ `AUTH_BOOTSTRAP_ADMIN/PASSWORD` 空库自举。bot 用**本地假 provider**（`openai` 客户端对象不联网）+ `platforms=[]` + `sandbox.backend=local` 启动，日志确认只有 `web` channel、`allow_post=false`，**零外部 IM 连接、零外发消息**。

真实浏览器（系统 Chrome + `--no-sandbox`）逐条走完：

- UI 登录 → `?tab=pty-sessions` 面板渲染 → 从 UI 新建会话 → 自动 attach（「已连接」）→ xterm 显示 `UI_READY` → 浏览器输入回显两行 → 离开页面后 REST 仍 `state=running`（**关标签页不杀会话**）→ 聊天页侧栏「后台交互会话 ×1」徽标 + 「查看」跳转 → 重新「接管」全量重放 → 「关闭会话」后列表清空、pid 消失（`escapedPids: []`）。
- WS 裸协议（Go 客户端）：`101` 握手、二进制重放、`state` 帧、`input` 回声、`resize` 生效并在重连 `state` 帧带回新尺寸、显式 `close` 帧收到 `state=exited` 后服务端关 socket；同源放行 / loopback dev-proxy Origin 放行 / 外部 `Origin: http://example.com` 返回 **403**。
- 权限面：admin `canManage=true`；member `GET` 列表可见但 `canManage=false`，`POST` / `DELETE` / WS 全部 **403**；浏览器中 member 视图显示「只读：接管与关闭需要 Bot 管理权限」，无新建框、无接管 / 关闭按钮、无控制台。
- 唯一的控制台错误是既有 `/api/config/bot.<id>.vision` 缺键 404，与本方案无关。

**实机打出来的第 5 个真实缺陷：resize 反馈环（已修）**

`SessionConsole.vue` 里 `.sc-body` 高度是 auto、`.xterm{height:100%}`，`fit()` 每多算一行就把容器再撑高 → `ResizeObserver` 再触发 → rows 自增。实机日志 **4434 条 `pty_session_ws_resize`，rows 从 31 涨到 3286**，且旧洪泛在加了服务端钳制后停在 500。修复三层：

1. `.sc-body` 改**固定高度 460px**（`box-sizing:border-box`），切断「容器高度依赖终端内容」；实测计算样式 `height=460px / minHeight=auto`。
2. 客户端 `sendResize()` 只在 `term.cols/rows` 变化时发帧，连接 / 重连强制发一次（服务端按 socket 记 last-applied，不是按 session）。
3. 服务端 `sandbox.ClampWinsize`（`MaxSessionCols=1000 / MaxSessionRows=500`）在 `SessionRegistry.Spawn` 与 `Resize` 两处统一钳制，**PTY winsize 与 daemon VT 网格用同一组值**；WS 侧 `lastCols/lastRows` 去重 no-op 帧；`Resize(0,0)` 显式回 `ErrSessionBadInput`。同时修正 `resizeScreen` 里那句不成立的注释（原写「PTY 由调用方同值限制」，实际**没有任何调用方限制**——又是一次「机制存在 ≠ 机制生效」）。

复验：修复版前端 + 修复版二进制重建后，5 秒内**只有 1 条 resize 帧**（连接时那次），rows=30，输入回显与 UI 关闭仍正常。新增 `sandbox/session_resize_test.go`（4 例）与 `TestPtySessionStream_ResizeDedupeAndClamp`；`go build ./...` / `go test -race` / `web/npm run build` / 模板绑定检查全绿。

**仍未完成（不要当成已验证）**

- ✅ 真实 Linux 容器 daemon + PTY 端到端 —— 已完成（见上）。
- ✅ `sandbox.backend` 运行中实例复核 —— 已完成（见上）。
- ✅ P2 后端前置契约 + 前端接管 —— 已完成（见上）。
- ✅ `background:auto` 自动升级、`watch` 事件唤醒 —— 已完成（见上）；**回合外主动唤醒按上述边界明确不做**。
- ✅ 运行中的 Web 服务里做浏览器实机验收 —— 已完成（隔离实例，见上）；**本机常驻服务仍未启动，生产部署未做**。
- ✅ 真实编译产物验证（露娜选「只验编译产物」）：按部署 ldflags 重建项目根二进制并短暂拉起隔离实例验 `/health`、前端 `index` / 主 bundle 均 200、`pty-sessions` 未登录返回 401、日志只有 `web` channel；验完立即停掉，默认端口全程空闲、无常驻进程。旧二进制备份在 `/tmp` 未入库。
  - ⚠️ **又踩一次 ldflags 静默失效**：`BuildTime` 第一次用 `date '+%Y-%m-%dT%H:%M:%S'`（无时区），`buildinfo.resolve()` 用 `time.Parse(time.RFC3339, ...)` 解析失败 → 静默回退 `source=binary-mtime`，health 看起来正常但注入根本没生效。改用 `date -u '+%Y-%m-%dT%H:%M:%SZ'` 后 `source=ldflags`。**完整 gotcha：值不能含空格（链接器 usage 失败）、必须带时区（RFC3339 解析失败）、`-X` 必须用完整 import 路径。**
  - ⚠️ 本轮按露娜要求**暂不 commit**，所以 `gitShort` 停在 `fe7cd712cc72`（上一个 commit）—— 正是项目已知的「先 commit 再编译」顺序坑，不是编译失败；提交后重新编译才会更新。
- ✅ SSH 场景实现 + 真实 `ssh -tt` 端到端验收 —— 已完成（见 **§0.2**）；含机密输入三层防线、health 当前行判据、11 个脱敏屏幕形态黄金集、WS 跨帧遮蔽回归。
- ⚠️ 未 commit、未 push、未启动本机常驻服务、未部署；按 fix-then-push 惯例等露娜指令。

### 0.2 SSH 场景实现与真实验收（2026-11-12，v3.8）

> 露娜：「有个用来控制 ssh 的场景也要实现下，可以用服务器的 ssh 地址做测试，毕竟这个场景非常常见 通过的话一般场景都能覆盖」。SSH 一条链路同时覆盖 host-key 确认、密码 / passphrase、远端 shell、嵌套交互、Ctrl+C、连接失败、机密输入，比真实 CodeBuddy CLI 更能代表「一般场景」。**实验主机 / 端口 / 口令只存在于本机 `/tmp` 脚本与进程 env，未进任何仓库文件；实验产物只做读屏与只读命令，不改动远端。**

**真实 `ssh -tt` 登录打出来的 4 个缺口**

1. **health 整屏匹配污染**：首次 host-key 确认屏 `Are you sure...` 会长期留在 40 行 VT 网格里。修复前 8 个阶段**全部**判成 `awaiting_action`（真实日志 8/8 误判），远端 shell 空闲、命令输出后、Ctrl+C 后都认不出来。
2. **凭据提示被历史确认屏抢走**：密码提示 `...'s password:` 本该是独立 verdict，却因上一行残留的 `Are you sure` 命中 `actionable` 而被判成 `awaiting_action`；`Permission denied` / `Connection refused` / `Last login` / 远端 `user@host:~#` 全无词表。
3. **echo-on 凭据扩散**：ssh 自身密码提示 `ECHO=0`，明文不进 PTY master（实测 `contains password: false`）；但 `read -p`、sudo 回显、PIN 对话框会把输入回显进 master → 扩散到 raw ring / `Screen` / `Delta` / `URLs` / `mode=raw` / WS `seq=0` 全量重放 / 工具调用历史。核查结论：**全仓此前对 PTY raw 流、ring、`RawSince`、WS 均无 masking**，现有 vault / 日志脱敏只保护已登记值且不在这些路径上。
4. **Ctrl+C 后立刻发下一条会吞首字符**：真实日志出现 `xit: command not found`（`exit` 变 `xit`）——中断后的输入行还没吐完新提示符，第一条 send 的首字节被吃掉。

**架构纠正：`secret=true` 是错的方向（文档初稿写错了）**

初稿 hint 写「register via secret_input and send with `secret=true`」——**仓库里根本没有这个参数**。核查 `llm/orchestrate.go:1582-1598` 发现全局唯一咽喉点早已存在：

- `secret.Default().Resolve(vaultBot, tc.Input)` 在 `tool.Execute` 前把所有工具入参里的 `{{secret:name}}` 还原成真值，而 `tc.Input` **保持占位符形态**（工具调用历史 / 日志预览 / `StreamToolResultPart` 都看不到真值）；
- `secret.Default().RedactValue(output)` 在工具返回后统一遮蔽，`err.Error()` 也过 `Redact`；
- `secret/vault.go` 的 `Register` 还会同步 `util/log.RegisterSecret`，日志落盘层再兜一层。

所以 **SSH 密码不需要任何新增参数**：模型把 `{{secret:名字}}` 写进 `sandbox_session_send.text` 即可。hint 已改为引导用占位符、`secret_input action=list/request`，`watch` 的 P0 suggested 同步修正。

**新落地的三层防线**

| 层 | 落点 | 解决 |
|---|---|---|
| 入参硬闸 | `sandbox/tools_session.go` `rejectUnresolvedPlaceholders` + `secret.PlaceholderNames` / `Vault.HasValues` | 未解析的 `{{secret:name}}`（名字写错 / 属于别的 bot / 绕过编排器直调）**明确报错**，不会把占位符当密码打进去白烧一次认证机会；`spawn.command` 与 `env` 同样拦截，避免凭据进命令行被 `ps` 看到 |
| 入站遮蔽 | `sandbox/session_secret.go` `SecretStream`，接在 `sessionCore.consume` 唯一入口 | 跨 chunk 分割的凭据用 `vault.StreamHoldLen` 扣住可能的真前缀，拼齐后 `Redact` 成 `{{secret:name}}`；ring / VT 屏幕 / delta / health / WS 重放全部基于**同一份**已遮蔽字节，不会互相漂移。`flushMask` 在退出前把扣住的前缀替换为 `***`，绝不吐半截明文。空 vault 时字节级透传、零延迟；`Seq` 计的是遮蔽后的字节，须与 replay 长度一致（已加断言） |
| 出站兜底 | `redactSessionView` / `redactRawChunk` + `api/handler_bot_pty_session.go` 每连接一个 `SecretStream` | 覆盖「凭据在字节产生**之后**才登记」的场景；WS 每 50ms 一个 `RawSince` tick，跨 tick 切分由流式遮蔽器接续，普通文本零扣留。服务端生成的 `Hint` / `Reason` / `Evidence` 不参与遮蔽（不含终端字节，且 `Hint` 自带 `{{secret:name}}` 字样，短值遮蔽会把它打碎） |

**health 判据重构**（`sandbox/session_buffer.go`）

- 新增 `HealthAwaitingSecret` / `HealthConnectFailed`，进 `watchPriorities` 的 **P0**；词表补 `password:` / `passphrase` / `OTP` / `PIN?`、`Permission denied` / `Connection refused` / `Host key verification failed` 等；`inputPattern` 补远端 `user@host:~#`。
- 三窗口取代整屏匹配：`healthTailLines=8`（actionable / waiting 上下文）、`healthPromptLines=3`（连接失败，让旧的 `Permission denied` 滚出去后不再毒化后续登录）、**最后一行 = 当前提示行**。
- 裁决顺序新增两条优先级：失败 > **当前行的凭据提示** > **当前行是 shell 提示符 → `idle:awaiting_agent`** > 历史 actionable > 历史 needSecret。「当前行是 shell 提示符」用专门的 `shellPromptPattern`（比 `inputPattern` 窄，不会把 TUI 菜单 `> 1. Yes` 当成空闲 shell），从根上压住滚屏里的历史确认屏。
- `sandbox_session_send` 提示词补「Ctrl+C 后先 poll 到新提示符再发下一条」，不引入固定 sleep（与 §3.4 的写入节奏约束一致）。

**真实链路验收结论**（隔离 harness，全输出脱敏）

| 阶段 | 修复前 | 修复后 |
|---|---|---|
| host-key 确认 | `awaiting_action` | `awaiting_action` ✅ |
| 密码提示 | `awaiting_action` | `awaiting_secret` + 占位符 hint ✅ |
| 远端 shell / 命令输出 / Ctrl+C 后 | 全 `awaiting_action` | 全 `idle:awaiting_agent` ✅ |
| 嵌套 `read -p` PIN | `awaiting_action` | `awaiting_secret` ✅ |
| echo-on 凭据回显 | 明文进 ring / screen / delta / WS | `PIN?{{secret:lab.pin}}`、`GOT:{{secret:lab.pin}}`，ring 无明文、含占位符 ✅ |
| 密码（ECHO=0） | 不进 ring | 不进 ring ✅ |
| Ctrl+C → `exit` | 首字符被吞 `xit` | `state=exited reason=exit exitCode=130`，无残留错误 ✅ |

回归：`sandbox/session_secret_test.go`（跨 chunk 遮蔽 / 前缀 flush / 出站兜底 / 未解析占位符拒绝 / 工具级拒绝 / **11 个脱敏真实 SSH 屏幕形态** + 判据优先级）、`TestPtySessionStream_MasksLateRegisteredCredentialAcrossFrames`（WS 跨帧）。`go build ./...`、`go vet`、`go test -race ./sandbox ./secret`、`web/npm run build` 全绿；本机 darwin 8 个环境失败已用 `git archive HEAD` 干净基线**逐条复现一致**，确认与本次改动无关。

⚠️ **验收 harness 自己踩的坑（写进方法论）**：用「屏幕里出现 `GOT:`」当等待条件会命中**被回显的命令行** `echo GOT:$x`，于是等待立刻返回、下一条 `Ctrl+C` 把还没读到输入的 `read` 打断，看起来像「遮蔽层吞了字节」。先做本地真 PTY 复现（正常）+ 真实 SSH A/B（注册 / 不注册值都正常）排除实现问题，最后改成**按 `seq` 水位线等新字节**才拿到干净结果。又是一次「构造样本证实的假设必须再用真实数据证伪」——这次是真凶在验收脚本里，不在被测代码里。

---

## 1. 为什么「等输入判定」必须降级

### 实测（本机 PTY 对照，构造样本，仅用于验证机制）

| 样本 | 1.5s 内输出 | 最大静默 | 末尾静默 | termios | 注入后 |
|---|---|---|---|---|---|
| **F 边跑边听 stdin**（`tty.setcbreak` + `select(stdin)` + 每 100ms 刷 spinner） | 14 chunks / 224B | 0.156s | **0.025s** | `ICANON=0 ECHO=0` | 立刻 `GOT '\n'`，**程序全程在工作也全程可收输入** |
| A 停下来等输入（`read -p`） | 1 chunk / 6B | — | 1.509s | canonical 不变 | `Hi Luna` |

F 上：静默窗口阈值（500ms）**永不成立**；`/proc/<pid>/syscall` 大部分时间显示 `running`，只有极短窗口落在 `select/poll` 上，采样撞中的概率很低；CPU 有波动。也就是说上一版的 D 级确定性判据 + 静默前提，对这类程序几乎必然漏报。而它**随时可以注入输入并立即响应** —— 交互能力一直存在，不需要「等待状态」这个中间概念。

补充实测（上一轮）：`read -p`、`sleep 3`、忙循环三者的 PTY 侧信号（静默 / `ICANON=1 ECHO=1` / 输入队列 0）**完全相同**；`ECHO=0` 和 `ICANON=0` 只能分类「密码输入」「raw TUI」。→ 正则、termios、静默、CPU 曲线都不能裁决「是否在等」。

真实 CLI 再加一条反例（§3.2）：**「零输出 + 进程存活」还可能是隐藏的启动失败**，与「等输入」「正常工作」在 PTY 侧完全同形。静默信号至此被三面证伪，彻底不能用于任何判定。

### 结论：`awaiting_input` 从门禁降级为可选 hint

- 会话 `state` 只保留 `running / exited / lost`（可选 `idle`）。**`running` 不等于「不能发输入」**。
- `/proc` 探测、termios 分类、prompt 正则全部**保留但改用途**：只给 `poll` 结果附一个 `hint`（如 `screen shows a y/n confirmation`、`stdin appears monitored`），agent 可参考也可忽略；另外用于 `sandbox_exec` 看门狗的「忙 / 死锁」分流，不再误杀。
- 分类结果（`kind=secret` 关回显、`kind=keystroke` raw TUI）仍用于日志与风险提示。

---

## 2. 现况证据链（为什么现在做不到）

| # | 事实 | 证据 |
|---|---|---|
| 1 | 主 shell 工具 `sandbox_exec` 参数 `command/workdir/timeout/stuck_timeout`，**没有 stdin、没有会话句柄**；`run_code`（`tools.go:306`，`lang=bash`）同样能执行 shell，**不带 `sandbox_` 前缀**，是后续 toolperm / 清单同步的现成反例 | `sandbox/tools.go:172-210, 306`；`sandbox/sandbox.go:95-113` |
| 2 | `stdin==nil` → `/dev/null`；agent 链上所有调用都传 nil | `sandbox/docker.go:351-354`；`sandbox/tools.go:265`；`sandbox/botworkspace.go:1007` |
| 3 | **没有 PTY**，仓库多处显式否认 | `sandbox/channel/protocol.go:128-130, 346-348`；`sandbox/term_session.go:14-16` |
| 4 | `runCommandWithStreaming` 把 readLoop 与 `cmd.Wait()` 绑死，函数**阻塞到进程退出**才返回 | `sandbox/docker.go:491-534`（`waitErr := cmd.Wait(); wg.Wait()` 在 **`:533-534`**）。⚠️ **v3.7 更正**：`:381-388` 是 `StdoutPipe`/`StderrPipe`/`Start`，**不是 Wait**；v3.6 把这两段并列引用容易误读 |
| 5 | `ExecResult` 只有 `exitCode/stdout/stderr`，无「存活 / 增量」语义 | `sandbox/sandbox.go:116-133` |
| 6 | 后台常驻会话会被 stuck（5min 无输出）/ hard（15min）看门狗杀掉 | `sandbox/sandbox.go:268-320`；`sandbox/docker.go:428-461` |
| 7 | 流式 chunk 只推前端 SSE，不进模型上下文 | `sandbox/tools.go:245-257` → `llm/orchestrate.go:1530-1537` → `agent/stages/llmroute.go:2317-2319` |
| 8 | 前端 xterm 是「一行命令一次 HTTP」模拟终端；`TermHub` 不持有进程 | `XtermConsole.vue:86-129, 196-200, 280`；`sandbox/term_session.go:17-22` |
| 9 | 提示词陈旧：写 `exec`（实为 `sandbox_exec`）、超时 30s（实为 5/15min），没有「后台会话」概念 | `sandbox/tools.go:34, 63` |

补充：本地 `config_settings` 里 `sandbox.backend` 未落库（默认 `auto`，`config/module.go:2007`），`sandbox.image=builtin` → 实际大概率 **channel 容器后端**。⚠️ **未用运行中实例复核 —— 这是验证缺口，不因方案定稿而消除**，已列入 §11 P1 上线前验收（须用运行中实例 / `/api/bots` 看 running 复核）。

**v3.7 复核补两条**：

- **§3.7 的「`DestroyBot`/`StopBot`/`CloseAll` 只清内存表、不关进程」不精确，须按后端分开说**（`sandbox/botworkspace.go:411-467`）：`CloseAll`（`:412-416`）确实只 `m.workspaces = make(map...)`，且 `botWorkspace.Close()`（`:1019-1021`）是 no-op；但 `DestroyBot`（`:422-443`）在 docker + `PersistentContainer` 下走 `c.destroy()` → `botcontainer.go:666-683` **`docker rm -f`**，`StopBot`（`:449-461`）走 `botcontainer.go:686-695` **`docker stop -t 10`**，两者都会连带停掉容器内全部进程。真正成立的表述是：**「没有任何按 botID 遍历后台会话逐个 kill 的钩子」**；且普通 `POST /api/bots/:id/stop`（`api/router.go:117`）**不调用**这两个方法，只有 `container/stop`（`:179`）/ `DELETE /container`（`:184`）才触发 → 主路径上的孤儿风险依然真实，「必须补级联」结论不变。
- **基线测试不是绿的**：本机 `go test -count=1 ./sandbox/...` 跑了 15.5s，**8 个 fail，全是 darwin 环境限制**（`sandbox/local: confinement is not available on darwin` ×5、`io: channel: process control requires unix`、`io: process list requires linux`、`env: sh: No such file or directory`、以及 `TestChannelExecAndStuckWatchdog` 拿到 `Aborted:true`），`sandbox/channelclient` 包通过。与本次改动无关（工作区只有本文档未提交，`git status` 仅 `?? docs/design-agent-interactive-pty.md`），但这正好再次印证 §3.6 的 darwin 边界。→ **实施时必须在 Linux 实例上重跑基线并确认绿灯**，已写进 §11 P0 / §14。

---

## 3. 目标架构：统一 spawn 内核 + 两种返回策略

**关键认知：不需要「事后 hook 已卡住的进程」。** 只要在启动时就建好会话句柄（PTY master / stdin writer / readLoop），进程本身一直在后台跑；所谓「转向后台」只是**改变工具返回的时机**，不重启进程、不丢已经收到的 buffer。这也避开了「pipe 起的进程无法事后升级成 PTY」的死结。

```
                     ┌─ sync（默认，等价现有 exec）: 等退出 → exitCode + stdout
Start() → 建会话句柄 ─┤
   ↑ readLoop 立即开  └─ background: 立即返回 {sessionId, pid, state, seq}
                          └─ auto（可选）: 先等 N 秒，未退出且有 TUI 输出特征 → 转后台返回句柄
```

- **sync 路径完全不变**：现有构建/测试/git 命令的可靠性、超时、截断、`reliability` 信号全部保留。
- **background 路径**：同一套启动代码，只是 `Start()` 后不 `Wait()`，函数立即返回；readLoop 作为独立 goroutine 常驻消费输出。
- **auto 升级**（可选，P2）：`sandbox_exec` 加 `background:auto`，启动后等 N 秒（建议 2~3s），若进程未退出且命中「疑似长驻交互」的**输出侧**信号 → 立即返回句柄。判据只决定**返回时机**，不是状态判定，误判代价极低：最多把一个长编译提前返回，agent 后续 `poll` 一样能拿到结果。
  - **正面判据**（全部来自已持有的 PTY 输出 / 终端状态，零额外探测）：进程存活超过 N 秒且未退出；CSI 光标控制 / 清屏重绘达到密度阈值；`tcgetattr(slave)` 被改成 raw/cbreak（`ICANON=0`）；屏幕上出现结构化可交互文本（框线 / 编号选项 / `>` 选中标记）。
  - ⚠️ **alt-screen 既非必要也非充分，不得作为必选项**：真实 CodeBuddy 首屏 `alt=0` 却有 87 个 CSI 重绘（§3.2）；反过来 `vim` / `less` 一进 alt-screen 却不一定需要 agent 插话。只把它当加权项。
  - ⚠️ **负面条件（必须排除）**：`N 秒内零输出 / 仅 \x00 等控制字节` + 进程存活 → **不升级，转启动诊断**。真实 CLI 的 `EADDRINUSE` 正是这个形态（§3.2）；把它当成「疑似长驻交互」会把一个已经坏掉的进程交给 agent。
  - **刻意不检测「stdin 是否被监听」**，理由见 §3.1。
- **显式优先**：agent 直接用 `spawn(background=true)`，不依赖自动判断。提示词写清「对 TUI / REPL / 常驻 agent CLI 一律用 spawn」。

### 3.1 怎么知道 stdin 被监听：答案是「不需要，也查不准」

#### 实测：外部信号原理上分不出来

本机 PTY 三个对照样本，观察 1s 后注入输入（构造样本，仅验证机制）：

| 样本 | termios 变化 | 1s 输出 | 最大间隔 / 末尾静默 | 注入后 |
|---|---|---|---|---|
| F `setcbreak` + `select(stdin)`（常驻 TUI） | `ICANON=1 ECHO=1` → **0/0** | 9 chunks / 144B | 0.165s / 0.021s | `GOT '\n'`，在监听 |
| G 只刷屏，**完全不碰 stdin** | 1/1 → 1/1 | 9 chunks / 144B | 0.157s / 0.023s | 只有 termios 回显，程序不理 |
| H `select(stdin)` 但保持 canonical | 1/1 → 1/1 | 9 chunks / 144B | 0.161s / 0.021s | `GOT 'Luna\n'`，**也在监听** |

**G 和 H 的所有外部可观测项逐项相同**：termios、输出字节数、chunk 数、输出节奏、进程存活、fd 0 都打开且指向同一个 pts。唯一差别是程序内部有没有把 fd 0 放进 `select` —— 这在 PTY 侧、输出侧、进程状态侧都看不到。

推论：F 说明「termios 被切成 raw」是强暗示，H 说明它**不是必要条件**（canonical 下照样监听）；同时也说明「看到 prompt 才发输入」会漏（H 没有任何提示就在等一个键）。所以 termios / prompt 只能当 hint，不能当判据。

#### Linux 上唯一接近确定的窄证据

`/proc/<pid>/fdinfo/<epollfd>`（Linux 3.8+，`proc(5)`）会逐行列出被 epoll 监听的 fd：

```
pos:    0
flags:  02
mnt_id: 10
tfd:    0 events: 19 data: 74253d2500000000
```

`tfd: 0` 就是 stdin 被 `epoll_ctl(EPOLL_CTL_ADD, 0, ...)` 注册。扫 `/proc/<pid>/fdinfo/*` 即可，**只读 procfs、不需要采样时机、不需要 ptrace**（fdinfo 受 `PTRACE_MODE_READ_FSCREDS` 约束，读自己子进程可行）。这是唯一持久、确定、零副作用的证据。

但覆盖面很窄：

- 只覆盖 epoll 系（Node/libuv、Python `selectors` 等会把 stdin 注册进去）；
- **`poll/select` 的等待集合在用户态栈上，只在调用瞬间存在** → `/proc/<pid>/syscall` 只给数组指针，要 `process_vm_readv` 读出来才知道含不含 fd 0，需 ptrace 权限且必须正好采到那一瞬；常驻 TUI 的 poll 窗口极短，命中率低；
- **macOS 没有 procfs**；kqueue 的注册集合在内核 knote 链表里，`proc_pidfdinfo` 不暴露，只有内核内存取证级手段能拿到 → 生产不可用。

#### 四层可观测性

| 层 | 手段 | 能证明什么 | 副作用 | 跨平台 | 用途 |
|---|---|---|---|---|---|
| L0 我们这侧 | `tcgetattr(slave)` 变化、`TIOCGPGRP(master)`、CSI / alt-screen 输出、进程存活时长 | 程序把自己当终端用 / 起了交互式前端 | 无 | ✅ | **hint + auto 返回时机**（alt-screen 非必需，见 §3.2） |
| L1 epoll fdinfo | 扫 `/proc/<pid>/fdinfo/*` 找 `tfd: 0` | stdin 被注册进 epoll | 无 | Linux only | 可选 hint |
| L2 syscall 采样 | `/proc/<pid>/task/*/syscall` + `process_vm_readv` 读 poll 数组 | 此刻正 poll 且集合含 fd 0 | 需 ptrace | Linux only | 不做（命中率太低） |
| L3 主动探针 | 写 1 字节 + 看 `TIOCINQ(slave)` 输入队列是否被消费 | 程序确实在读 | **会注入输入** | ✅ | 只用于 `send` 后的 `accepted` 判定，**禁止预检** |

#### 所以这个条件不该存在

1. 我们持有 PTY master，**永远可写**。写进去的字节 canonical 下排队、raw 下进输入队列，不会崩；最坏是被程序误读，而误读该由 agent 看屏幕决定，不由检测器决定。
2. 「监听 stdin」是程序的**内部意图**，不是能可靠观测的外部状态；把架构押在最不可观测的一环上，等于把前两版的判定问题换个名字重新引回来。
3. 很多程序先干活、稍后才开始听 stdin；也可能像 H 一样没有任何提示就在等一个键。任何「先检测再放行」的门禁都会漏。
4. 正确做法是**能力常供、决策外移**：所有会话都给句柄 + 永远可写 + 可读 buffer，agent 依据屏幕自己判断要不要发。检测最多作为 `poll` 里的 hint，**关掉不影响任何功能**（写进 P3 验收）。

### 3.2 真实 CLI 验证：启动错误被 PTY 吞掉、首屏不是登录、alt-screen 缺席

按露娜建议「拿真实 CodeBuddy CLI 做实验，首次登录是好场景」，用 **CodeBuddy Code `2.182.0`** 做了隔离对照。安装到临时目录（`npm install --prefix <隔离目录> --no-audit --no-fund @tencent-ai/codebuddy-code`），**不全局安装、不使用真实 HOME / config / data 目录、不注入任何凭据**；`HOME` / `CODEBUDDY_CONFIG_DIR` / `XDG_CONFIG_HOME` / `XDG_DATA_HOME` 全部指向临时目录，`TERM=xterm-256color`，winsize 120×40，去掉了本机 `*_PROXY` 注入。**全程未输入凭据、未按 Enter、未打开浏览器、未认证**，只观察首屏和按键回声。

对照方式：同一命令分别跑在 ① `pty.openpty()` 的 PTY slave（stdout/stderr 合并）② 普通 pipe（stdout/stderr 分离）。

| # | 观察 | PTY（合并） | 无 TTY pipe（分离） |
|---|---|---|---|
| 1 | 启动早期（端口冲突未排除） | 15s / 30s / 45s 全部 `chunks=0 bytes=0 alive=True`；45s 才吐 1 字节 `\x00` | `bytes=110 alive=False exit=1`，直接给出 `Error: listen EADDRINUSE: address already in use <host>:<port> (use --port <number> to choose another port)` |
| 2 | 错误去哪了 | PTY **屏幕上看不到**；只出现在 CLI 自身日志：`[ProcessExit] unhandledRejection code=EADDRINUSE` | 就在 stderr 里，一眼可读 |
| 3 | 同窗口其他启动行为 | 日志里还有 `API call failed status 401 → backing off 30000ms`、`hasSession=false hasAccessToken=false`、`DefaultModeStrategy: auth initialized` | — |
| 4 | 用空闲端口 `--port <free-port>` 重跑 | `t≈4s chunks=8 bytes=3394 quiet=2.60s`，**真实首屏出现** | — |
| 5 | 首屏 termios / 序列 | 启动 `ICANON=1 ECHO=1 ISIG=1` → `ICANON=0 ECHO=0 ISIG=0`；`alt=0 csi=87`（**无 alt-screen**，纯 CSI 重绘 + Unicode 框线） | 非交互分支，不进 TUI |
| 6 | 首屏内容 | `Do you trust the files in this folder?` + `> 1. Trust folder only` / `2. Trust parent folder` | — |
| 7 | 方向键注入 | `Down` → `>` 从 1 移到 2；`Up` → 移回 1；**`send(keys)` 生效、无需等任何状态** | — |

（`<host>:<port>` / `<free-port>` 为脱敏占位；日志行只保留机制相关片段。）

**四条结论，逐条改动方案**：

1. **推翻原 `merged:true` 取舍** —— 同一个致命启动失败，在 pipe 下是「`exit 1` + 一行清晰 stderr」，在 PTY 下变成「45s 零输出但进程存活」。错误要么被改道到 CLI 自己的日志通道，要么被淹没在 ANSI 重绘里，结论一致：**PTY 屏幕不能作为「启动成功 / 失败」的判据**。→ §5 改为保留独立诊断侧信道，§4 `SessionView` 加 `StderrTail` / `BootDiag`，`pending #4` 重写。
2. **「零输出 + alive」是三义的** —— 可能是等输入、可能是在忙、也可能是已坏掉。任何「静默 → 判定 / 升级」的逻辑都不成立。→ §1 补反例、§3 auto 加负面条件、§12 明确不做。
3. **alt-screen 不能当判据** —— 真实首屏 `alt=0 csi=87`，靠「有没有 `\x1b[?1049h`」会整类漏掉。可靠组合是 `CSI 重绘密度 + termios raw + 可见结构化选项文本 + 存活`。→ §3 auto 判据重写。
4. **验证主线成立** —— 常驻 TUI 首屏是**按键选择的 modal**（不是文本登录），`state=running` 下 `send(keys=Down)` 立刻改变屏幕、没有任何「等待状态」；且 `ISIG=0` 意味着 Ctrl+C 只是被程序当普通字节读走，进程级 `signal` 工具是必需的。→ §4 / §6 的工具契约与示例换成这个真实样本。

另注：`--help` 阶段曾出现 `fetch config failed ... at setProxy`，是本机 proxy 注入带来的启动期噪声；说明**启动期诊断还要覆盖环境注入**，不能只看 PTY 字节流。

### 3.3 PoC harness 实测：用文档的方法真的把 CLI 驱动到登录

按 §3 / §4 契约写了最小可执行 PoC（临时脚本，**不进仓库、不进实现**）：双 fork 守护化 → `pty.openpty()` 120×40 → `spawn` 后立即起常驻 readLoop → 维护 raw ring buffer / `seq` / VT 网格 / 语义 diff / `urls` / BootDiag 侧信道 → Unix socket 上提供 `poll(sinceSeq)` / `send(keys,text,submit)` / `signal` / `close`。目标不是实现功能，是**检验文档里的契约能不能真的驱动一个真实 TUI**。

完整链路（全程靠 `poll` 读屏 + `send` 按键，没有一次 `Wait()`）：

| 步 | 动作 | 实测结果 |
|---|---|---|
| 1 | `spawn` | 立即返回 `sessionId` / `pid` / `state=running` / `seq=0`；跨多次工具调用复查 daemon 与 CLI 均存活 |
| 2 | `poll(sinceSeq=0)` | `8 chunks / 3394B`，VT 网格把 Unicode 框线 modal 降噪成可读纯文本：`Do you trust the files in this folder?` + 4 选项 + `> 1. Trust folder only`；`hint: -` |
| 3 | `send(keys=Enter)` @ `state=running` | 信任确认生效 → 主界面 + `Select login method` 4 选项 |
| 4 | `poll(sinceSeq=3394)` | `21 chunks / 11538B`，拿到登录菜单 |
| 5 | `send(keys=Enter)` @ `state=running` | 生成登录 URL，进入 `Waiting for auth...` |
| 6 | `poll(sinceSeq=11538)` | `62 chunks / 15903B`，从 `urls` 抽到带一次性 `state` 的登录 URL 转交人；`zeroOutputMs=12`。⚠️ 该链接属**宿主账号体系**（见下文第四条坑），但「抽取 + 转交」机制本身有效 |

**逐条对照设计点**：

| 设计点 | 结论 | 证据 |
|---|---|---|
| 常驻 TUI「一直工作 + 一直可交互」真实存在 | ✅ 证实，且是**第三种形态** | 登录等待期 spinner 每 ~0.1s 重绘，`zeroOutputMs=12`，静默窗口永不成立；它既不是「等 stdin 输入」也不是「在忙」，而是**等一个 PTY 之外的异步事件（浏览器授权）** |
| `state=running` 允许 `send` | ✅ 证实 | 两次 `send(Enter)` 都在 running 下生效，分别推进两个交互屏 |
| 会话可「放着不管」 | ✅ 证实 | 首次 `send` 距 spawn ~78s，期间无人 poll，buffer 完整保留、进程未被看门狗杀 |
| `accepted` 不能用 `seq` 推进判断 | ✅ 实证 gotcha | `send` 返回时 `seq 3394→3396`，是 **termios 回显**而非程序消费；只能由后续 `poll` 的屏幕变化确认 |
| BootDiag 侧信道必要性 | ✅ 再次证实 | 等待期侧信道抓到 `[Error] 401` + `backing off 60000ms`，**PTY 屏幕上完全没有**；§3.2 结论在「非启动期」复现 |
| `poll` 默认回「屏幕 + 语义 diff」 | ⚠️ **不达标，需修** | 62 chunks 里 30+ 行是 `✶✸✹✺✷ Waiting for auth...` 重绘，逐行 diff 全记成变化行，`delta` 被淹没 |
| `urls` 字段 | ➕ **契约里漏了，实测证明必须有** | 登录 URL 是带外交互的唯一入口，agent 需要把它直接转交人 / 工具，而不是自己瞎按键 |

**三条新增实现坑（v3.1 未覆盖，都是实测打出来的）**：

1. **ANSI 序列与 UTF-8 多字节会跨 read chunk 边界**：PoC 按 chunk 独立 `decode` + 正则剥 ANSI，屏幕里残留 `[38;5;79m` / `[22m` 等半截序列，Unicode 框线被打成替换字符。→ 必须用 `codecs.getincrementaldecoder('utf-8')` 维护解码状态 + `pendingEscape` 缓冲把跨 chunk 的 CSI/OSC 拼完整；**URL 提取必须在全量 raw 上做，禁止按 chunk**（本次按 chunk 与全量结果一致只是侥幸，长链接必断）。
2. **部分重绘会被逐行 diff 放大**：TUI 把同一行分两次写（`│ 2. Trust parent` → 补全 `│ 2. Trust parent folder (...)`），逐行比较记成两条 changed line。→ `delta` 要按**稳定窗口**（100~150ms 无新字节）聚合，同一行窗口内只取最终态、丢弃中间态。
3. **`isatty(2)` 与 stderr 侧信道冲突**：想把 stderr 单独接 pipe 就得让它不是 TTY，可能改变程序分支。→ 侧信道改为**优先用 CLI 自身结构化日志 tail**（零 fd 侵入），stderr pipe 只作兜底；见 §5 / `pending #4`。

**一条架构补充：带外交互（out-of-band action）**。登录这类流程真正的交互发生在 PTY 之外（浏览器点授权）。此时 agent 既不该 `send`、也不该干等，而应把 `urls` 里的链接**转交给人**，然后继续 `poll` 等屏幕收敛。所以 `poll` 结果要显式给出可操作外链，`hint` 可附 `external action required` —— 这是 §3.1「能力常供、决策外移」的自然延伸。

**第四条坑（最重要）：env 全量继承会换掉验证对象**。第一轮 harness 只 strip 了 `CODEBUDDY_*` 与 proxy，仍全量继承了宿主桌面端注入的 `WORKBUDDY_*` / `CLIENT_INFO_*` / `ACC_PRODUCT_CONFIG_PATH`。被测 CLI 自带的 `product.json` 明确声明 `productConfigPathEnv = ACC_PRODUCT_CONFIG_PATH`，于是宿主那份产品配置直接覆盖包内默认：

| 字段 | 包内 `product.json` 默认 | 被宿主覆盖后 |
|---|---|---|
| `productName` | `CodeBuddy` | 宿主产品名 |
| `endpoint` | 公开 CodeBuddy 站点 | 宿主产品域名 |
| `platform` / `version` | `CLI` / `2.182.0` | 宿主平台 / `5.9.3` |

表现：**同一份 CLI、同一套 PTY，只改 env**，文档链接变成宿主文档站，登录 URL 从 `...?platform=CLI&version=2.182.0` 变成 `...?platform=<宿主>&version=5.9.3`。第一轮拿到的登录链接其实是**宿主账号体系**的，不是独立 CLI 的；改成严格白名单 env（只留 `PATH/USER/SHELL/LANG/TERM` + 隔离目录）后才拿到正确的 CLI 链接。

→ 设计结论（写进 §8 / §9）：`SpawnRequest.Env` **必须白名单组装，禁止 `os.Environ()` 全量继承**。thinkbot 自身跑在带大量注入变量的宿主里，全量继承会让 agent spawn 出去的 CLI 被宿主产品配置劫持（品牌、endpoint、登录域名全变），同时把宿主 token / 内部地址泄漏进子进程。这既是功能正确性问题也是安全问题，还直接破坏 §11「用真实对象验证」的前提 —— **验证环境不干净，验证对象就不是你以为的那个**。

⚠️ 本 PoC 只验证 local PTY 路径；channel 后端在 v3.4 补了 client 侧锁的 Go 探针实测（§3.6），但**真实容器 daemon / PTY 帧仍未跑**，按 §11 落地纪律不能据此宣布主路径生效。

### 3.4 外部操作闭环：bot 不只读，还能从外面驱动它干活

露娜要求的「bot 不只是要知道里面干啥，还能从外面操作它」，前面只验证到**导航按键**（`Enter` 选菜单）。这一节验证真正的一半：**往主输入框发一条指令，让 CLI 执行任务，再应答它内部弹出的确认屏**。用已登录的干净会话，指令是「执行 `echo <marker>` 并原样报告输出」（无害、结果可机器核验），全程只有 `send` + `poll`，没有调用 CLI 任何 API。

| 步 | 动作 | 屏幕 / 结果 |
|---|---|---|
| 1 | `send(text=<指令>, submit=true)` 109B | 写进输入框，CLI 开始 `Waking… (waiting for model)` |
| 2 | `poll(sinceSeq)` | `delta` 出现 `Waking…` spinner，屏幕仍是主界面 `>` |
| 3 | 继续 `poll` | **弹出 CLI 内部的权限确认 modal**：`Are you sure you want to do this?` / `> 1. Yes` / `2. Yes, and don't ask again this session` / `3. No, and tell … what to do differently`；**没有任何「等待输入」通知** |
| 4 | `send(keys=Enter)` 选 1 | 确认屏消失，工具真正执行 |
| 5 | `poll(sinceSeq)` | `delta` 出现 `● <marker>` + `✔ Worked for 44s` + 回到 `>` |
| 6 | 再 `send(text=<第二条>, submit=true)` 86B | 这条不调工具 → **没有确认屏** → `Worked for 3s` + 结果 token |
| 7 | 后续 `poll` | `delta` 为空、`seq` 不再变，会话仍 `running`，可继续发第三条 |

**逐条结论**：

| 观察 | 结论 |
|---|---|
| 指令被接受并真的执行出结果 | ✅ 「从外面操作」成立；`send(text, submit=true)` 的 109B / 86B 一次写入 + 100ms 后 Enter 均生效（该 CLI 未表现 bracketed paste 问题） |
| 确认屏是 `poll` 读出来的，无任何通知 | ✅ 再次证明不需要 `awaiting_input`；agent 看到 `Are you sure…` 自行决策 |
| **嵌套交互**：外部驱动的 CLI 内部又要求一次输入 | ✅ 用同一套 `send/poll` **递归**解决，不需要新增状态机或工具 |
| **第 1 轮弹确认、第 2 轮不弹** | ✅ **确认屏不是必然**：固定节奏 send、预写脚本、「发完就等着」全都会错，只能读屏决策 —— 这是「能力常供、决策外移」最直接的实证 |
| 单任务 44s、chunks `1134→1582`、raw ~185KB | ✅ 会话化模型对长任务零阻塞（没有任何一次工具调用被卡住）；同时再次量化 ANSI 洪水（§5） |
| 结果 token 只出现在一次 `delta`，之后 `delta` 空 | ✅ `sinceSeq` 水位线既不漏、也不重复喂上下文 |
| 必须按 `Yes` 才放行 shell 执行 | ⚠️ §9 的「确认类按键风险」是**真实高频路径**，不是理论假设 |

**由此新增三条设计约束**：

1. **嵌套交互是一等场景**。之前把交互想成「一层」，实际是「agent 驱动 CLI → CLI 内部再要一次确认」。工具契约不用改（还是同一个 `send`），但**提示词必须写清**：发完指令后要 `poll` 看有没有确认屏，读清选项再决定，不能假设一条指令一次收工。
2. **确认类按键是真实高频风险，但只能靠「工具级静态裁决 + 提示词」兜住**（v3.7 更正，原写法有个不存在的机制）。这里的 `Yes` 等价于「批准执行 shell 命令」，实测说明它是高频路径。⚠️ **`allowUnsafe` 在仓库里零匹配，是 v3.4 凭空写的参数名**；更关键的是 `toolperm.Service.Evaluate` / `EvaluateUsers`（`toolperm/service.go:220, 225`）签名只吃 `(botID, tool, platform, chatID, candidateIDs)`，**只按工具名做静态规则匹配，拿不到工具参数、更拿不到当前屏幕内容** → 「识别到确认屏就把这次 `send` 动态升级为需批准」在现有 toolperm 体系里根本无法表达。首版做法：`sandbox_session_send` **整体**按 `RiskSensitive` 走规则裁决（不带参数级豁免），提示词写死「确认屏上的 `Enter`/`y` 等于批准执行，必须先 `poll` 读清选项」；`poll`/`watch` 虽定 `RiskBasic`，也不得据此绕过 `send` 的裁决。若要按内容动态升级，须给 toolperm 增加「工具参数级裁决」接口并单独立项（P3），**不进首版、不在文档里假装已有**。
3. **禁止脚本化 `send`**。两轮对比说明确认屏时有时无；`submit=true` 之后必须回到 `poll`，禁止连发两条、禁止按固定 sleep 猜结果。这也反向说明为什么「等待状态」这个概念既不可靠又没必要 —— 决策点是屏幕内容，不是状态机。

#### 补验：非默认分支与 `signal(INT)` 的真实语义

| 补验项 | 实测 | 结论 |
|---|---|---|
| 确认屏选项 2（`Yes, and don't ask again this session`） | `send(keys=Down)` 后 `>` 从 `1. Yes` 移到 `2. …`，`Enter` 后结果 marker 正常出现 | 方向键选非默认项可靠 |
| 确认屏选项 3（`No, and tell … what to do differently`） | 连按两次 `Down` 切到 `> 3. No…`，`Enter` 后屏幕给出 `● REPL · Run echo command` / `└ No (tell … what to do differently)`，且**执行结果 marker 不存在** | 拒绝分支可靠，shell 命令确实没跑；`Esc` 也能取消确认屏 |
| 执行中 `signal(sig=INT)` | 长任务期间发 INT → 3s 后 `state=exited`、`exitCode=0`、`seq=273969`；后续 `send` 抛 `OSError: [Errno 5] Input/output error` | ⚠️ **推翻原设计**：`signal(INT)` 是**终止会话**，不是「中断当前任务后继续用」 |

→ `signal(INT)` 统一按「终止会话」写进契约与提示词。若确实需要「中断当前任务但保留会话」，必须另设路径（`send(keys="Ctrl+C")` 在 §3.2 实测的 `ISIG=0` 下只被当普通字节；或依赖 CLI 自带取消），单独立项、单独验收，**不能复用 `signal(INT)` 的语义**。

### 3.5 后台会话 health / stuck detection：怎么知道「是不是真卡住」

#### 问题不是「有没有输出」

全部会话进后台后，agent 不再被 `Wait()` 阻塞，代价是失去了「调用没返回 = 还在跑」这个天然信号。「`procAlive=true` 且零输出」至少五义：

| 形态 | 真实来源 | 谁该动 |
|---|---|---|
| 等 agent 输入 | 主输入框 `>` 可见、CLI 空闲 | agent 发下一条指令 |
| 等人（带外） | 登录 / OAuth 链接在 `urls`，屏幕 spinner | 人点链接，agent 只 `poll` |
| 在忙 | `seq` 持续增长、屏幕行在真变 | 不用动 |
| 假活重绘 | `seq` 涨得很快但只有一行 spinner 变（`changedPerChunk≈1.00`），甚至纯控制序列（`0.00`） | 不用动，但别误判成进展 |
| 真卡死 / 隐藏启动失败 | 无输出、无输入框、无确认屏；或 §3.2 的 `EADDRINUSE` | 查 `bootDiag`，再决定 signal/close |

所以**不能只看「没输出」，也不能只看「进程活着」**；更不能沿用 §1 已三面证伪的静默窗口当判据。

#### 信号全部来自 daemon 内部（零 OS 依赖、跨平台）

| 信号 | 来源 | 用途 |
|---|---|---|
| `seqRate` | 最近 5s 原始字节数 / 5s | 有没有在产生输出 |
| `changedPerChunk` | 最近 5s `changedLines` 总和 / chunk 数（**未归一化** raw 屏幕行 diff） | 区分真进展 vs 单行重绘 |
| `screenStableMs` | 语义屏幕 hash 多久没变（数字/时长归一化后） | 辅助判 spinner 假活；⚠️ 不能单独判卡住 |
| `actionable` | **raw** 屏幕上的确认/选择文本 | 暴露决策窗口，最高优先级 |
| `inputReady` | **raw** 屏幕上的 `>` 输入提示符 | 区分「等 agent」与「卡住」 |
| `waiting` | `Waiting for / Spinning / Waking / …` | 仅在 `seqRate>0` 时区分 spinning vs no_progress |
| `readLoopAlive` | readLoop 每次循环打心跳，>2s 无心跳即故障 | 区分「子进程卡了」与「daemon 自己坏了」 |
| `procAlive` | `cmd.Process` 状态 | `exited` 判定 |

⚠️ **滑动窗口必须固定在 daemon 侧**：`poll` 是 agent 主动调的，两次调用间隔可能几十秒；若用「上次 poll 以来的增量」算速率，结果完全取决于 agent 的轮询频率。实现用固定 5s 窗口的 `(ts, bytes, changedLines)` 队列，与 poll 频率解耦。

#### 裁决顺序（实测调过三轮，顺序本身是设计）

```text
exited / procAlive=false             → exited
readLoopAlive=false                 → unhealthy:daemon
actionable                          → awaiting_action   ← 必须最优先
seqRate>20 && changedPerChunk>=1.5  → progressing
seqRate>20 && changedPerChunk<1.3   → spinning | suspected_stuck:no_progress
seqRate>20 && screenStableMs>=1500  → spinning | suspected_stuck:no_progress
seqRate>20                          → progressing
inputReady                          → idle:awaiting_agent
screenStableMs>3000                 → suspected_stuck
否则                                 → idle
```

三条顺序都是真实数据打出来的：

1. **`actionable` 必须压过 `progressing`**：真实 CLI 弹确认屏那一刻 `seqRate=3079.8`、`changedPerChunk=1.99`，按数值本应判 `progressing`；那样 agent 会继续轮询而**错过决策窗口**。
2. **`changedPerChunk` 必须压过 `screenStableMs`**：语义归一化把 `step 1/2/3` 映射成同一个 token，`screenStableMs` 在真实进展中照样涨到 2.3s；构造样本 `N_progress` 因此被误判过一次。
3. **`inputReady` 不能被 `waiting` 否决**：CLI 会在屏幕上留一行已停止的 `✷ Waking… (0s · preparing…)`；`Waking` 命中后，明明已回到 `>` 输入框却被判 `suspected_stuck`。`waiting` 只在 `seqRate>0` 时才有意义。

#### 两条判据坑：判据必须跑在 raw 屏幕上

- 语义归一化会删掉 `>` 和框线字符 → `inputReady` 在语义屏幕上**恒为 False**。
- `Press ESC` 曾是确认屏关键词，但主界面静态 Tips 里就有 `Press Esc twice to reset the input box.` → 空闲态被误判 `awaiting_action`。`actionable` 正则只保留 `Are you sure / Do you trust / [y/n] / 1. Yes / Select login method / …`，且作用在 **raw** 屏幕。

#### 实测：构造样本 8/8 + 真实回放 + 真实 CLI 逐帧

构造样本混淆矩阵（用 daemon 里同一份 `Health` 实现，不是另写一份逻辑）：

| 样本 | 期望 | 实测 | `seqRate` | `ch/chunk` |
|---|---|---|---|---|
| `N_progress` 逐行输出 | `progressing` | ✅ | 203.8 | 12.67 |
| `F_fakelive` 单行 spinner | `spinning` | ✅ | 695.6 | 1.00 |
| `I_wait_input` y/n 提示 | `awaiting_action` | ✅ | 6.6 | 2.00 |
| `H_real_stuck` SIGSTOP 无输入框 | `suspected_stuck` | ✅ | 4.8 | 1.00 |
| `E_idle_prompt` 打印 `>` 后空闲 | `idle:awaiting_agent` | ✅ | 0.4 | 1.00 |
| `W_static_waiting` 残留 `Waking…` + `>` | `idle:awaiting_agent` | ✅ | 6.6 | 2.00 |
| `J_frozen_idle` 输入框可见时被冻结 | `idle:awaiting_agent` | ✅（**设计上承认不可区分**） | 0.4 | 1.00 |
| `D_daemon_fault` readLoop 心跳停、进程 alive | `unhealthy:daemon` | ✅ | — | — |

真实 PoC 事件回放（1944 chunks / 干净会话）用来证伪「构造样本自嗨」：

| 窗口 | chunks/5s | bytes/5s | `ch/chunk` | 判读 |
|---|---|---|---|---|
| 登录/授权等待（80s~160s，连续 17 个窗口） | ~62 | ~5952 | **0.98~1.00** | 假活：单行 spinner 重绘 |
| 另一段纯控制序列重绘（445s~470s） | ~25 | ~875 | **0.00** | 假活：语义零变化 |
| 正常执行（0s / 60s / 165s / 475s…） | 11~80 | 2.8K~24K | **1.60~6.00** | 真进展 |

真实 CLI 最终复验（`poll` 每 1s 一次，同一已登录会话）：

```text
idle        → idle:awaiting_agent  (inputReady=true,  actionable=false)
t+1..t+4    → progressing          (seqRate 1858→2858, ch/chunk 3.69→1.75)
t+5 确认屏  → awaiting_action      (seqRate 3079.8, ch/chunk 1.99, actionable=true)
```

#### 边界与动作策略（必须写进提示词）

- `health` 是 `poll` 的**诊断字段**，不是状态机：不改 `state`、不自动 `signal`、不自动 `close`。`suspected_stuck` 只让 agent 去做 `bootDiag` 核查或显式求助，处置权仍在 agent / 人。
- **屏幕信号能区分的是「有没有在产生有意义的变化」，不是「进程有没有被 OS 冻结」**。`J_frozen_idle` 证明输入框可见时冻结与健康空闲同形。要覆盖需 ① 有副作用的 `send` 探针（违反 §3.1 禁止预检），或 ② OS 级进程状态（Linux `/proc/<pid>/stat` 的 `T`；macOS 无等价物）→ 列 P3 可选 hint，跨平台不承诺。
- `readLoopAlive` 最 cheap 也最不能省：**daemon 自己挂了**时 PTY 不再被消费，子进程阻塞在 write 上，外观与「程序卡死」一模一样（§5）。

### 3.6 channel 后端实测：`rpc` 锁串行、独立连接可并发

§8 写「会话用独立连接、不要碰 `client.go:31/239` 的 `rpc` 独占锁」，此前只有源码证据。补一次 Go 探针实测（临时脚本，不进仓库）：

- 起一个受控延迟的 channel 协议对端（`WriteFrame` / `ReadFrame` 均为包内导出 API），对 `MsgCommandExec` 故意延迟 3s 再回 `MsgCommandExit`；
- 三条连接 `a / b / c` 完成 Hello；`a.Exec("sleep 3")` 持有 `a.rpc` 期间，并发发 `a.Info`、`b.Info`、`c.Exec("echo …")`。

| 调用 | 实测延迟 | 说明 |
|---|---|---|
| baseline `a.Info`（空闲） | `170µs` | 基准 |
| `a.Info`（与 Exec 同连接） | **`2.599274s`** | 被 `rpc` 锁串行到 Exec 结束 |
| `b.Info`（独立连接） | `283µs` | 不受影响 |
| `c.Exec`（独立连接） | `52.679ms`，且拿到 `stdout="CONCURRENT_OK"` | Exec 期间仍能并发执行 |
| `a.Exec` 自身 / 总墙钟 | `3.001113s` / `3.001484s` | 两次 Exec 并行，未叠加成 6s |

同连接 / 独立连接 `Info` 比值 **≈9190x**。结论：`client.go` 注释「`A second connection is required to list or signal processes while an exec is streaming`」不只是注释，是硬性约束；每会话一条独立连接 + 连接内 reader 按 ReqID 分发 + writer mutex 的方案成立，现有 RPC 零改动。

⚠️ **本机验证边界**：darwin 编译命中 `reaper_other.go`（`//go:build !linux`），`Reaper.Start` 直接返回 `process control requires unix`，所以**真实 channel server 在本机跑不了 Exec**，探针用的是协议对端（被测对象是 client 侧锁，与 server 实现无关）。真实容器 daemon / PTY 帧仍需线上 Linux 实例验证，步骤列在 §11 P1；在此之前不得宣布 channel 主路径「已生效」（本项目踩过「机制存在 ≠ 机制生效」4 次）。

### 3.7 进程所有者：SessionManager（统筹的是**进程**，不是工具）

露娜补了一句「还需要一个 manager 来统筹控制这些后台运行的工具」，随后纠正：**是进程**。这个纠正把落点从「工具集合的注册表」拉回「后台进程的所有权与生命周期收口」，也正好补上前面各章都绕开的一层。

#### 为什么必须有唯一所有者

现在 `spawn/poll/send/signal/close` 各自拿 `sessionId`，**谁都不持有进程所有权**，会出四类事故：

| 事故 | 现有代码证据 |
|---|---|
| bot 停用 / 删除时后台 CLI 变孤儿 | `sandbox/botworkspace.go:411-467` 的 `CloseAll` / `DestroyBot` / `StopBot` **只清 `workspaces` 内存表，不关任何进程** |
| 只杀直接子进程，孙子进程逃逸 | `sandbox/docker_exec_cancel_integration_test.go:148-160` 专门测 `echo escaped > orphan.txt`；`procgroup_unix.go:19-25` 才用 `Kill(-pgid)` |
| 双 fork 孙进程变僵尸 | `sandbox/channel/reaper_unix.go:27-31`：Go 的 `os/exec.Wait` 与全局 `Wait4` 竞争，daemon 不能自己 `Wait()` |
| 按 pid 二次操作打到复用后的别的进程 | §7 已定 `sessionId` 主键，pid 只作日志/排查 |

#### 定位：manager 是进程原语的**唯一收口点**，不是新写一套进程管理

仓库里进程原语已经齐了，manager 只负责登记 + 收口 + 级联：

| 原语 | 位置 | manager 怎么用 |
|---|---|---|
| `setProcessGroup`（Start 前 `Setpgid=true`） | `sandbox/procgroup_unix.go:11-17`、`docker.go:355` 注释「必须在 Start() 之前」 | spawn 的唯一入口；禁止工具各自 `exec.Command` |
| `killProcessTree` / `SignalGroup`（`Kill(-pgid)`，失败回退 `kill(pid)`） | `procgroup_unix.go:19-25`、`channel/reaper_unix.go:132-140` | `signal/close/closeAll` 的**唯一** kill 路径 |
| `Reaper`（`PR_SET_CHILD_SUBREAPER` + `Wait4` + `exited` 缓存 + `Forward(sig)` 广播） | `channel/reaper_unix.go:33-59,94-107,143-153,155-193` | channel 后端直接复用；退出状态写回 `state=exited`；服务关闭时 `Forward(SIGTERM)` |
| `processAlive`（`kill(pid,0)`） | `procgroup_unix.go:27-32` | `health.procAlive` + pid 复用校验 |

⚠️ 不要在 manager 里再写一遍 `cmd.Wait()` / `p.Kill()`：会与 Reaper 的全局 `Wait4` 抢，且只杀得到直接子进程。

#### 分层：宿主登记 / 容器内持有进程

```text
宿主 BotWorkspaceManager（已有；tools.go:1114 注入，tools.go:92-102 闭包捕获 botID）
  └─ SessionRegistry: sessionId -> {botID, backend, pgid(仅日志), startedAt, lastTouch, cursor[agent]}
       │  唯一所有者：spawn / signal / close / closeAll / DestroyBot 级联
       ├─ local PTY  → ProcessOwner 持 master fd + cmd + readLoop（复用 procgroup_unix.go）
       └─ channel    → 容器内 ProcessOwner 持 pgid + Reaper；每会话独立连接按 ReqID 分发（§3.6）
```

- **必须分两层**：容器内 pid 在宿主 namespace 里没有意义，宿主 `SessionRegistry` 只持 `sessionId + 连接`；进程身份、回收、逃逸检测归容器内 daemon。
- 复用现成注入路径：`RegisterBotWorkspaceTools(toolMgr, mgr)`（`tools.go:1114`）已把 `BotWorkspaceManager` 闭包进 `BotWorkspaceToolProvider`，`Tools()`（`tools.go:92-102`）又从 `sctx.BotID` 捕获 botID → **botID 硬隔离零新增机制**，PoC 实测 `watch(bot-c) total=0`、错误 bot 的 `poll` 返回 `not found or wrong bot`。
- daemon 重启 / 连接断开 → `state=lost`，禁止静默重建（§9）。**#9 已定稿：采纳「容器内 idle TTL 自杀 + 宿主一律回 `lost` 让 agent 重开」**，不做握手认领 —— 避免 pid 复用风险，代价是丢掉已投入的长任务上下文；实现须在 `lost` 返回里附 `reason` 与 `lastSeq`，让 agent 知道要重开而不是继续 poll。

#### 生命周期事件（全部由 manager 收口）

| 事件 | 动作 |
|---|---|
| `spawn` | 登记 `(sessionId, botID, pgid)` → 起 ProcessOwner → 起常驻 readLoop → 立即返回句柄 |
| 进程退出 | Reaper / Wait 拿到 exit status → `state=exited` → buffer 保留 60s → 释放 fd |
| idle 超时 | **lazy GC**：`watch` / `spawn` 时顺带扫（对齐 `TermHub.dropExpiredLocked` 的懒清理），**不新增 ticker goroutine**（`SandboxManager.cleanupLoop` 那种常驻定时器不进本方案） |
| `close(sessionId)` | `SignalGroup(pgid, TERM)` → 宽限 → `KILL` → 释放 fd / buffer / 连接 |
| `closeAll(botID)` / `DestroyBot` / `StopBot` / 服务 `Close` | 级联关闭该 bot 全部会话 + `Reaper.Forward(SIGTERM)`；⚠️ 现有三个方法只清内存表，**必须补** |
| 确认类操作 | manager 只收口「谁能关谁」，**不自动批准执行**（§9 toolperm） |

#### ⚠️ v3.7 补齐：会话脱离回合 ctx 之后，谁来回收

六层复核挖出的**最重的遗留点**：上表把 idle 超时定成「`watch`/`spawn` 时 lazy GC，不新增 ticker goroutine」，但本节的 `watch` 又明确「纯 pull，agent 不在回合里时后台进程继续跑但没人 watch」—— **两条合起来意味着：agent 回合结束后再没人调 `watch`/`spawn`，lazy GC 永不执行，15min idle TTL 在最需要它的路径上完全失效**。

先把现有中止链摆清楚（全部为已核实代码事实）：

| 环节 | 证据 | 行为 |
|---|---|---|
| 前端「停止」 | `web/src/stores/bot.js:1306-1307` → `POST /api/chat/abort` | 传 `sessionId` / `traceId` / `all` 三粒度 |
| 后端分发 | `api/handler_chat.go:171-202` → `api/botservice.go:917-933`（`AbortMessage`）、`:1061-1075`（`AbortSession`）、`:1114-1126`（`AbortBot`） | **只调 `cancel()` + 删表项，零资源清理**，不遍历任何 registry |
| cancel 来源 | `api/botservice.go:3395-3403` 注入 `OnMessageStart` → `agent/bot/bot.go:783-789` `context.WithCancel` | `msgCtx` 贯穿整条工具链 |
| 现有 exec 的落点 | `sandbox/botworkspace.go:906, 992` → `sandbox/docker.go:378-380` `cmd.Cancel = stopCommand` → `:372` `killProcessTree` | ctx cancel → `exec.CommandContext` → 进程组 kill |
| **回合正常结束也会 cancel** | `agent/bot/bot.go:764-773` `finishMessageLifecycle`，被 `:813/:833/:861/:867` 覆盖正常与异常出口 | 不只是「用户点停止」，**每一轮结束都会 cancel** |

由此得到两条结论：

1. **后台会话必须完全脱离 message ctx —— 方向正确，且比原先写的更硬**。若复用 `msgCtx`，不等用户点停止、agent 正常回完一句话就会把后台 CLI 杀掉，直接违背 §0「放着不管」主线。现有 `sandbox_exec` 严格绑 message ctx，正是不能复用 `runCommandWithStreaming` 的第三个理由（前两个是 `Wait()` 绑定、无 PTY）。
2. **但「脱离 ctx」≠「脱离所有生命周期」**。abort 后 `SessionRegistry` 无人引用、无人 `watch`、lazy GC 不触发；local 后端下 `StopBot` 是 no-op、`CloseAll` 不 kill、`botWorkspace.Close()` 也是 no-op → 孤儿进程只能等进程自己退出或服务重启（重启后内存表丢，真孤儿）。容器后端好一些，但只有 `container/stop` 或 `DELETE /container` 才 `docker stop -t 10`。

**修订（保持 #7「不新增常驻定时器」的意图，不推翻定稿）**：把回收拆成两层，各司其职 ——

| 层 | 负责什么 | 触发条件 | 是否依赖 agent |
|---|---|---|---|
| 注册表内存 GC | 清 `SessionRegistry` 里的过期条目（对齐 `TermHub.dropExpiredLocked`） | `watch`/`spawn` 时 lazy 扫 | 依赖，但**不再负责杀进程**，失效也无害 |
| **进程级 idle TTL** | **TERM → 宽限 → KILL 进程组，置 `state=exited` + `reason=idle_ttl`** | **由本就常驻的 readLoop 每轮顺带比对 `lastTouch`，零新增 goroutine、零 ticker** | **完全不依赖** |

readLoop 本来就在持续消费 PTY 字节，顺手判一次 `now-lastTouch > TTL` 是纯 Go、无外部定时器、无新 goroutine，且与 #9「容器内 idle TTL 自杀」天然同构 —— #9 定的是「连接断开/daemon 重启时宿主回 `lost`」，这里是「会话没人碰时 ProcessOwner 自己收摊」，两者用同一个 TTL 常量即可，不必引入第二套计时。

**abort 的语义明确写死**：`/api/chat/abort` **不自动 kill 后台会话**（理由见结论 1，否则又变回「回合结束就杀」），但必须做到：① `AbortSession` / `AbortBot` 把命中维度的会话标为 `orphaned` 并 `Infow`；② 用户可见性走 §10 的现成「运行中」徽标，把后台会话与 trace 任务**区分展示**，不能让人以为点停止就全停了；③ `BotService.StopBot`（`api/botservice.go:3617-3694`）/ `Bot.Close`（`agent/bot/bot.go:546-602`）**同步调用** `SessionRegistry.closeAll(botID)`，这是不依赖 TTL 的确定性收口 —— 现在 `StopBot` 全程不调 `workspaceMgr.DestroyBot/StopBot/CloseAll`，`Bot.Close` 只走到 `BotWorkspaceManager.Close()` → `CloseAll()` 清表。④ 服务 `Close` 走 `Reaper.Forward(SIGTERM)`。

> 这条修订同时回答了「为什么不能只靠 lazy GC」：**任何把进程存活绑定到「agent 还会不会再来调一次工具」的设计，都在 agent 消失的那一刻失效**。进程所有权收口（§3.7 的定位）必须包含一条不依赖调用方的兜底。

#### ⚠️ 实测：进程组 kill ≠ 收干净

多会话 PoC（1 个真实 CLI + 构造样本，双 fork 孙进程写 heartbeat）跑出：

```text
fork   孙进程（留在 pgid 内）      closeAll 后 alive=False, heartbeat 停止   ✅ 被进程组带走
escape 孙进程（os.setsid() 逃逸）  closeAll 后 alive=True,  heartbeat 继续   ❌ 逃逸
watch(bot-p) total=0                                    ← 注册表已空，但进程还活着
```

→ **`watch total=0` 不代表没有残留进程**。manager 必须：① 复用 `Reaper` 的 subreaper 接管孤儿并回收；② `close` 时对 `setsid` 逃逸者记录 `escapedPids` 并 `Infow` 告警，不能假装收干净；③ 逃逸进程不归任何 `sessionId`，后续 `signal/close` 不得误伤。宿主侧无 `/proc` 等价物时只报不杀。

#### `watch`：agent 侧唯一入口（查询，不是调度器）

manager **不替 agent 决策**（否则把 v2 状态机从后门请回来）。它只把 N 个会话收敛成一条按「需不需要一次动作」排序的队列，排序直接复用 §3.5 的 `health.verdict`，不另造一套：

| 优先级 | verdict | suggested |
|---|---|---|
| P0 | `awaiting_action` / `suspected_stuck` / `unhealthy:daemon` | `poll then send` / `bootDiag` / `restart session` |
| P1 | `exited` / `idle:awaiting_agent` | `poll then close` / `send` |
| P2（折叠） | `progressing` / `spinning` / `idle` | `none` |

```text
watch(botID 由 ToolProvider 隐式注入, agent?, limit=8, includeQuiet=false)
→ { total, attention:[{sessionId,pid,pgid,alive,state,verdict,priority,reason,
                       suggested,seq,sinceSeq,urls}], quiet:N|[], reclaimed:N }
```

- `sinceSeq` 是 **manager 替 agent 记的 per-agent 水位线**：agent 不用自己记 N 个 seq。PoC 实测 `cursor 0 → poll(seq=29854, delta=19557B) → cursor 29854 → 再 poll delta=0`，不漏不重。
- `snapshot()` 必须**零副作用**：不消费 delta、不推进 cursor、不触发任何 send/signal；`watch` 只读会话缓存的 health 摘要，O(N)。
- ⚠️ **纯 pull，不改编排层**：`llm/orchestrate.go` 的 `runTool` 同步返回后才 append `ToolMessage`，所以 agent 不在回合里时后台进程继续跑但没人 watch。首版接受这个边界；P2 唤醒复用 `OnMessageStart` / SSE，单独立项。

#### PoC 实测（多会话，含 1 个真实 CodeBuddy CLI）

| 检查 | 结果 |
|---|---|
| watch 把确认屏顶到 P0、spinner 折叠进 quiet | ✅ `total=3 attention=1 quiet=2` |
| 从外部驱动真实 CLI 弹出内部确认屏 | ✅ t+6s → `watch` 变 `attention=2`，两个 P0 都是 `awaiting_action` |
| botID 隔离 | ✅ `watch(bot-c) total=0`；错误 bot `poll` 被拒 |
| per-agent cursor | ✅ 第二次 `poll` delta=0 |
| closeAll 级联 / 注册表清空 | ✅ `closed=3`、`closed=2`、`watch total=0` |
| 进程组级联 | ⚠️ 见上：组内被带走，`setsid` 逃逸者仍活着 |

---

## 4. 工具契约（6 个 + `close_all`）

> ⚠️ **v3.7 定稿注册全名**：v3.6 全篇用裸名，其中 **`spawn` 是硬阻塞** —— `subagent/tools.go:90` 已静态注册 `Name: "spawn"`，而 `agent/tools/registry.go:151-153` 在合并动态 provider 时「同名静态优先」，新增的动态 `spawn` **永远不会出现在 agent 工具列表里**（`api/handler_bot_toolperm.go:182, 291` 也已登记该名字）。统一改用 `sandbox_session_*` 前缀：既避开撞名，又让 `sandbox_` 前缀天然命中 `toolperm/risk.go:241` 的敏感护栏。**下文示例（§6）与正文为可读性仍写短名，注册名一律以下表全名为准。**

| 注册全名 | 短名 | 关键参数 | 返回 |
|---|---|---|---|
| `sandbox_session_spawn` | `spawn` | `command`, `cwd`, `background=true\|auto`, `cols=120`, `rows=40`, `env`, `term=xterm-256color` | `{sessionId, pid, pgid, state, seq, screen, urls, bootDiag, backend}` |
| `sandbox_session_poll` | `poll` | `sessionId`, `sinceSeq?`, `mode=screen\|tail\|raw`, `lines=40`, `waitMs=0` | `{state, delta, screen, urls, seq, settledSeq, exitCode?, hint?, stderrTail?, bootDiag?, reason?, lastSeq?, health, truncated}` |
| `sandbox_session_send` | `send` | `sessionId`, `text?`, `keys?`, `submit=false` | `{state, seq, accepted?, warning?}` |
| `sandbox_session_signal` | `signal` | `sessionId`, `sig=INT\|TERM\|KILL` | `{state}` ⚠️ 语义见下方 |
| `sandbox_session_close` | `close` | `sessionId` | 释放句柄 / 杀进程组 |
| `sandbox_session_watch` | `watch` | `limit=8`, `includeQuiet=false`（**`botID` 与 `agent` 都由 `ToolSessionContext` 隐式注入，一律不接受模型传参**） | `{total, attention[], quiet, reclaimed}`；见 §3.7 |

**新增工具必须同步的 7 处清单**（v3.7 补；`sandbox/tools.go:128-141` 的注释专门警告过「两份清单漂移是**静默**的」）：① `botWorkspaceToolDefs()`（`sandbox/tools.go:153-166`，生产唯一真源，动态 provider `Tools()` 在 `:92-103` 直接返回它）；② `WorkspaceToolNames()`（`:142-149`，工具名唯一真源，供外部校验）；③ `botWorkspaceToolPromptSection`（`:22-79`，提示词里现写裸名 `exec`、超时 30s，本身已陈旧）；④ `api/handler_bot_toolperm.go:279-316` 的 `fallbackToolList()` 与 `:187-195` 中文名映射（**现已漏 `run_code`，是现成漂移**）；⑤ `workflow/profile.go:68-77` 档位常量 + `workflow/profile_test.go:129-152`；⑥ `sandbox/tools_test.go:285-305` 的 **`len(defs) != 10` 硬断言（加工具会立刻让测试挂）**；⑦ `toolperm/risk.go`（见 §9）与 `llm/tool_guard.go:66`。**漏任何一处，现象都是「工具静默不出现」或「权限页选不到」，不会报错。**

⚠️ **SubAgent 边界**：`BotWorkspaceToolProvider.Tools()`（`sandbox/tools.go:92-103`）不读 `sctx.IsSubagent`、不设 Scopes，`subagent/manager.go:192-233` 让子 Agent 继承主 Agent 的 ToolManager 并取交集 → **子 Agent 会拿到全部新增会话工具**，包括能 `send` 批准执行、能 `close` 别人的会话。§3.7 只写了 botID 隔离，不够：首版须显式决定「是否对 `IsSubagent=true` 屏蔽 `sandbox_session_*` 全组」（建议屏蔽，最小改动是在 provider 内按 `sctx.IsSubagent` 返回不含会话工具的清单），并写进 §11 验收。

- `poll` 默认**非阻塞**；需要短暂等待可用 `waitMs`（上限 8s，不长期占住工具调用）。`sinceSeq` 只回增量。
- `hint` 是可选的启发式观察，**不参与任何状态机**，agent 可以忽略。
- `send` 硬规矩：literal 写入、默认不附加 `\n`；`submit=true` 拆成「写文本 → 等 ~100ms → 发 Enter」（TUI 会把紧连的 text+Enter 当粘贴）；全屏 TUI 一次只发一个动作，发完 `poll` 确认。
- `state=running` **允许 send** —— 这正是与 v2 最大的区别，§3.3 已用真实 CLI 两次 `send(Enter)` 证实。
- `poll` 在 `state=running` 且**启动后前 N 秒零输出**时，除 `screen` 外必带 `stderrTail` / `bootDiag`（§3.2）——否则 agent 会把「已经坏掉」误读成「还在工作」。
- `ISIG=0` 的 raw TUI（§3.2 实测）里 Ctrl+C 只被当普通字节读走，`send(keys="\x03")` 不保证中断。
- ⚠️ **`signal(sig=INT)` = 终止会话，不是「中断当前任务后继续用」（§3.4 实测推翻原设计）**：raw TUI 上实测 `state=exited`、后续 `send` 抛 `EIO`。提示词必须写清「发 INT 后会话不可再用，要重开」。任务级中断另立项、单独验收。
- **`poll` 必返 `health`（§3.5）**：daemon 内部固定 5s 滑动窗口算出的诊断对象；`hint` 继续只做可选观察，`health` 不参与状态机、不触发自动处置。
- **`urls` 必返**（§3.3 实测）：从**全量 raw** 抽出的可操作外链，是登录 / OAuth / 设备码 / 回调这类**带外交互**的唯一入口。agent 看见它应把链接转交人 / 工具，而不是自己按键或干等。
- **`send` 返回的 `written` 不代表被消费**：termios 回显会让 `seq` 凭空推进（§3.3 实测 `3394→3396`），所以禁止用「`seq` 变了」当 `accepted`；消费确认只能靠后续 `poll` 的屏幕变化。
- **`send` 之后必须回到 `poll`（§3.4）**：一条指令可能触发 CLI 内部的权限确认屏，也可能不触发；禁止连发两条、禁止按固定 sleep 猜结果。提示词要写清「发完读屏，读清选项再决定下一步」，嵌套交互用同一个 `send` 递归处理。
- **`watch` 是只读查询（§3.7）**：不消费 delta、不推进 `sinceSeq`、不自动 send/signal/close；`attention[].suggested` 只是排序建议，不是待执行指令。决策权仍在 agent。
- ⚠️ **`agent` 与 `botID` 同等敏感，都不准模型传参**（v3.7 补）：v3.6 把 `agent?` 列成模型可传参数，但 per-agent `sinceSeq` 是水位线 —— 模型只要换个 agent 名就能拿到 `sinceSeq=0`、**把整个历史 raw 重放一遍**，等价于绕过 `sinceSeq` 且不消耗任何权限。`agent` 必须像 `botID` 一样由 `ToolSessionContext` 隐式注入（`tools.go:92-102` 已捕获 botID，同一闭包补 agent 即可）。首版若不做多 agent，直接固定 `agent="default"`。
- **所有进程操作只经 manager**：工具不得自己 `exec.Command` / `p.Kill()`；`botID` 只能来自 ToolProvider 闭包，禁止模型传参（否则可跨 bot 关掉别人的会话）。`signal/close/closeAll` 统一走 `SignalGroup(pgid, …)`。
- ⚠️ **`poll` 返回 `settledSeq`（v3.7 补，详见 §5）**：`seq` 是原始字节水位线，而 `delta` 是稳定窗口结算出来的语义增量，两者不对齐时 `sinceSeq` 会漏内容。agent 下一轮 `sinceSeq` **必须取 `settledSeq`，不能取 `seq`**；`watch.attention[].sinceSeq` 同理按 `settledSeq` 推。

`PTYSnapshot` 简化为：

```go
type SessionState string // running | exited | lost（idle 只是 health verdict，不进 state；§1）
type SessionView struct {
    SessionID  string
    PID        int      // 仅供人工排查 / 日志关联，不作主键
    State      SessionState
    Seq        uint64   // 原始字节水位线：只用于 raw 回放 / 排障
    SettledSeq uint64   // v3.7：语义增量已结算到的水位线；下一轮 sinceSeq 必须取它（§5）
    Delta      string   // 语义增量：稳定窗口聚合 + 重绘去重后（见 §5）
    Screen     string   // 当前可见屏幕（VT 网格降噪后）
    URLs       []string // 全量 raw 抽出的可操作外链（带外交互入口），见 §3.3
    ExitCode   *int
    Hint       string   // 可选，启发式观察；带外交互时可附 "external action required"
    Truncated  bool
    Merged     bool     // PTY 下 stdout/stderr 合并（≠ 屏幕可信，见 §3.2）
    StderrTail string   // 独立侧信道：不经 PTY 的 stderr / 结构化日志尾部，可空
    BootDiag   string   // 诊断摘要（前 N 秒、零输出或侧信道报错时填）
    Reason     string   // v3.7：lost/exited 原因码 daemon_gone | conn_lost | idle_ttl |
                        //       signal | close | exit；#9 要求 lost 必带
    LastSeq    uint64   // v3.7：lost 时最后已知水位线，配合 Reason 让 agent 判断"重开"而非继续 poll
    Health     SessionHealth // 后台健康诊断（§3.5），必返，不参与状态机
}

// 判定顺序与边界见 §3.5；所有信号来自 daemon 内部，零 OS 依赖。
type SessionHealth struct {
    Verdict         string  // progressing | spinning | awaiting_action |
                            // suspected_stuck | suspected_stuck:no_progress |
                            // idle:awaiting_agent | idle |
                            // unhealthy:daemon | exited
    SeqRate         float64 // 5s 窗口 bytes/s
    ChangedPerChunk float64 // 5s 窗口 raw 行变化数 / chunk 数
    ScreenStableMs  int64   // 语义屏幕 hash 稳定时长（数字归一化会使其虚高）
    Actionable      bool    // raw 屏幕上有确认/选择文本
    InputReady      bool    // raw 屏幕上有 ">" 输入提示符
    Waiting         bool    // 仅在 SeqRate>0 时用于 spinning 分流
    ReadLoopAlive   bool    // readLoop 心跳，区分 daemon 故障
    ProcAlive       bool
    Evidence        string  // 人 / LLM 可读的判据说明
}
```

> ⚠️ **v3.7 补：`Reason` / `LastSeq` 正是 §13 #9 定稿要求、但 v3.6 契约里根本没定义的两个字段**。#9 白纸黑字写「`lost` 返回须带 `reason` + `lastSeq`」，原 `SessionView` 里两个都没有 —— 照原样实现，agent 拿到 `state=lost` 无法区分「daemon 挂了」和「idle 自杀」，也不知道该重开还是继续 poll。`SettledSeq` 则修 §5 的字节/语义水位线错位。

#### 新增字段消费方表（六层核查第 6 层：谁读它、在渲染层哪里看得到）

| 字段 | 至少一个消费方 | 首版渲染层 |
|---|---|---|
| `Delta` / `Screen` / `Hint` | LLM（`ToolMessage` 回灌）、agent 读屏决策 | ❌ 不进 DOM |
| `URLs` | agent 转交人 / 带外工具（§3.3） | ❌ 由 agent 复述到聊天消息，**不是结构化 UI** |
| `Health` | LLM（`poll` 结果）、`watch` 优先级排序（§3.7） | ❌ 不进 DOM；`watch.attention[]` 只回 LLM |
| `StderrTail` / `BootDiag` | LLM 诊断、`Infow` 日志（脱敏后）、§11 P0 验收比对 | ❌ 不进 DOM |
| `Reason` / `LastSeq` | agent 判断「重开 vs 继续 poll」、`Infow` | ❌ 不进 DOM |
| `SettledSeq` | agent 下一轮 `sinceSeq`、`watch` 的 per-agent cursor | ❌ 不进 DOM |
| `Truncated` / `Merged` | LLM 判断输出是否可信、`Infow` | ❌ 不进 DOM |
| `escapedPids` | `Infow` 告警、运维 | ❌ 不进 DOM |
| `sessionId` / `state` / `verdict` / `orphaned` | **`SessionList.vue` 运行中徽标 + 停止按钮（P2）** | ✅ **P2**：复用 `web/src/components/SessionList.vue:29-33, 87-92, 258-282` 与 `stores/bot.js:1849-1860` 的 15s 轮询，把数据源从 traceID 扩到 sessionId（详见 §10） |

⚠️ **首版刻意「用户不可见」是取舍，不是遗漏**：本方案的服务对象是 agent，用户可见性靠 agent 复述与 `Infow`。但按六层核查标准，**必须说清「指不出渲染层位置」是有意为之** —— P0/P1 验收不得声称「用户能看到会话状态」；P2 之前，前端唯一相关的是聊天流里 agent 自己说的话。§10 已把 P2 的现成组件和数据源写清楚。

---

## 5. Buffer 与输出治理（能否「舒服」的关键）

**readLoop 必须持续消费**：PTY/pipe 缓冲区写满会让子进程阻塞在 write 上，看起来又像「卡住」。项目已有同类记录（`sandbox/docker.go:355-358` 的管道后代持写端问题）。所以 `Start()` 与 readLoop 必须同生，且 readLoop 不能挂在 `Wait()` 后面。

三层存储：

| 层 | 内容 | 用途 |
|---|---|---|
| raw ring buffer | 原始字节，固定上限 256KB~1MB，环形覆盖 + `truncated` 标记 | 排障、前端 xterm 回放 |
| 降噪 tail | 丢 ANSI/SGR、处理 `\r` 覆盖、CRLF、去空行 | 行式 CLI / REPL |
| VT 屏幕网格 | `cols×rows` cell buffer、光标移动/擦除/alt-screen/wrap、200 行 scrollback | 全屏 TUI 的「当前画面」 |

**ANSI 洪水**：实测 F 每 100ms 重绘一次 spinner，1.5s 就 14 个 chunk；真实 CLI 首屏 8 个 chunk 就带 87 个 CSI（§3.2），登录等待期更是 62 chunks / 30+ 行 spinner 重绘（§3.3）。若 `poll` 直接回原始增量，agent 每轮都会被无意义重绘淹没。所以：

- `poll` 默认返回 **当前屏幕 + 语义 diff**（新出现的文本行 / 变化的行），不是字节流。
- `raw` 模式只在排障和前端接管时用。
- `seq` 按原始字节递增；出口仍过 `TruncateOutput`（500 行 / 50KB）。⚠️ **v3.7 更正调用事实**：`llm/tool_truncate.go:62-70` 是 `DefaultToolOutputConfig()`，`TruncateOutput` 的签名在 **`:113-119`**；且 **`sandbox/` 内没有任何调用**，由 `llm/orchestrate.go:1654` 编排层对**所有工具统一截断**。sandbox 另有一层独立字节截断 `Config.MaxOutput = 1MB`（`sandbox/sandbox.go:197-198, 279` → `docker.go:588-599` `truncateString`），即**两层截断**：sandbox 1MB 按字节 → 编排层 500 行/50KB 按 head+tail。实现别在工具里再调一次。
- ⚠️ **v3.7 补：`seq`（字节）与 `delta`（语义）必须双水位线，否则会静默丢内容**。稳定窗口是 120ms「无新字节才结算」，而 `seq` 每收到一个字节就涨。若 agent 恰好在窗口未结算时 `poll(sinceSeq=X)`，daemon 已收下 `[settled, X)` 这段字节但还没产出对应 `delta`，下一轮 `poll(sinceSeq=X)` 就**永远拿不到这段语义内容**（§3.3 实测「结果 token 只出现在一次 `delta`」正是这类边界的高危形态）。修法：`SessionView` 同时返回 `Seq`（原始字节，仅供 raw 回放/排障）与 **`SettledSeq`（已结算到的字节位置）**；`poll` 的 `sinceSeq` 一律按 `SettledSeq` 推进，agent 下一轮取 `settledSeq` 而非 `seq`；`watch` 的 per-agent cursor 同理。`raw` 模式才用 `Seq`。这条要写进 §11 验收：**窗口内连续 poll 不得丢任一 marker**。
- ⚠️ **逐行 diff 不够**（§3.3 实测 62 chunks 里 30+ 行 spinner 被全记成变化行）：必须加 ① **稳定窗口** 100~150ms 无新字节才结算，窗口内同一行只取最终态、丢弃部分重绘的中间态；② **高频单行折叠**：同一行位置在窗口内变化超过 K 次（spinner / 进度条）时只回最终一行并标 `collapsed:N`；③ `delta` 行数设上限，超出按 `TruncateOutput` 截。
- ⚠️ **解析必须跨 chunk 有状态**（§3.3 实测残留 `[38;5;79m`、`[22m` 与被打断的 Unicode 框线）：用增量 UTF-8 decoder + `pendingEscape` 缓冲把跨 chunk 的 CSI/OSC 拼完整；`urls` 一律在**全量 raw** 上抽，禁止按 chunk 抽。

**stderr 取舍（v3.1 修订，原结论被 §3.2 实测推翻）**：原方案写「PTY 下 stdout/stderr 合并，`merged:true` 即可，要分离 stderr 就走 sync `exec`」。真实 CLI 证明这是**错的**：`EADDRINUSE` 在 pipe 下是 `exit 1` + 一行 stderr，在 PTY 下变成「45s 零输出但 alive」，错误被改道到日志 / 淹没进 ANSI 重绘 —— agent 拿到的 `screen` 是空的，却会以为会话正常跑着。合并不是「少一点信息」，而是**把唯一的失败证据弄丢**。

修订为「**PTY 给屏幕，侧信道给诊断**」两条独立通道：

| 通道 | 内容 | 实现 |
|---|---|---|
| PTY master（合并） | stdout+stderr 的**屏幕字节流** → `raw ring buffer` / `screen` / `delta` | 不变，`merged:true` 只描述这条流 |
| 诊断侧信道（不进 PTY） | 独立 stderr fd、CLI 结构化日志尾部、退出码、启动 preflight 结果 | 汇总进 `SessionView.StderrTail` / `BootDiag`，`poll` 在**启动后前 N 秒或零输出**时强制返回 |

- 具体做法（`pending #4` 已定稿，见 §13）：**首选 CLI 自身结构化日志 tail** —— 按会话 `pid` + `[Error]/[Warning]` 级别过滤，上限 20 行 / 4KB，零 fd 侵入、`isatty(2)` 不变，§3.3 PoC 已用这条路径实测抓到 `401` + backoff。local / docker 可额外接独立 stderr pipe 作兜底，`docker exec -t` 用 Engine API 独立 attach 流或容器内 tee；channel 加 `MsgSessionDiag` 帧（§8）。⚠️ **把 stderr 改成 pipe 会让 `isatty(2)=false`**，可能改变程序分支，必须 A/B 对照后才启用（§3.3 坑 3）。
- `poll` 的硬规矩：`state=running && Seq==0 && 已启动 > N 秒` → 必须附 `BootDiag`，并把 `hint` 置为 `no output; possible startup failure, check diagnostics`，**禁止**输出「程序在等待输入」这类诱导性判断。
- 「零输出但 alive」不得触发 auto 升级（§3）、不得计入「疑似正常长驻」（§9 看门狗）、不得当成 `awaiting_input` 的 hint（§1）。
- 需要严格分离 stdout/stderr 做机器解析的命令，仍走 sync `exec`；这条不变。

**健康信号是 buffer 治理的副产品**：`readLoop` 每消费一个 chunk 就往固定 5s 窗口写一条 `(ts, bytes, changedLines)`，并维护语义屏幕 hash、raw 屏幕行与 readLoop 心跳 —— 这些正是 §3.5 `health` 的全部输入，零额外探测。⚠️ `changedLines` 必须用**未归一化 raw 行**算：语义归一化会把 `step 1/2/3` 折叠成同一个 token，让真实进展看起来像「屏幕冻结」。

#### ⚠️ 幂等性（v3.7 补，六层核查第 4 层）

这里会被**反复应用**的变换比看上去多，而且输入恰恰是「真正会被变换的内容」：增量 UTF-8 解码 + `pendingEscape` 拼接、ANSI/SGR 剥离、`\r` 同行覆盖、语义归一化（数字/时长折叠）、稳定窗口折叠 + `collapsed:N`、`urls` 抽取。它们会在三处被重放：① 前端接管时从 raw ring buffer 重建屏幕（§10）；② daemon 重建 / 会话 `lost` 后重开时的回放；③ 同一份 raw 被 `mode=screen|tail|raw` 各渲染一次。

- **必须从「同一段 raw 字节序列」出发做纯函数式重建**：`render(raw) → (screen, delta)`，`render(render_input)` 的**输入**始终是 raw，绝不能拿上一次的 `screen`/`delta` 当输入。稳定窗口折叠与 `collapsed:N` 只对**输出**生效，不得回写 raw。
- **必写幂等性测试**（用真实 ANSI 洪水内容，不用「不会被处理的纯 ASCII」当输入，否则测试退化成无操作）：
  ```go
  raw := goldenANSIFlood   // §3.2/§3.3 的真实 62-chunk 屏幕流，含跨 chunk 半截 CSI / Unicode 框线
  once  := render(raw)
  twice := render(raw)
  if !reflect.DeepEqual(once, twice) { t.Fatal("render is not deterministic / idempotent") }
  // 另测：分块喂入 (1B / 7B / 任意边界) 与整块喂入结果一致 —— 验证 incremental decoder 的状态正确
  ```
- **归一化/折叠只用于视图，不用于 `urls` 与 `BootDiag`**（§3.3 已定「URL 从全量 raw 抽」）。`StripChannelContextMarkers` 那类正文清洗在本方案里**不做**：PTY 屏幕是给 agent 读的原始画面，做清洗会改坏框线与选项文本（第 3 层「照抄外部方案」的反例）。
- `sinceSeq`/`SettledSeq` 水位线本身也要幂等：同一 `sinceSeq` 重复 `poll` 必须返回**相同** `delta`（或明确 `delta=""`），不得因重复调用推进状态 —— `watch` 零副作用（§3.7）依赖这一点。

进程退出后 buffer 保留 60s 供 `poll` 取 `exitCode` 与最终输出，再回收。

---

## 6. agent 的自主循环（示例，改用 §3.2 真实首屏）

```
spawn("codebuddy", background=true)
  → {sessionId:"s-7f2", pid:1234, state:"running", seq:0, bootDiag:"(empty)"}

poll(s-7f2, sinceSeq=0, mode=screen)     # 无 alt-screen、纯 CSI 重绘，VT 网格降噪后
  → screen: "╭──────────────────────────────────────╮
             │ Do you trust the files in this folder? │
             │  > 1. Trust folder only               │
             │    2. Trust parent folder             │
             ╰──────────────────────────────────────╯"
  → LLM 读屏幕判断「这是确认屏，选 1」→ send(s-7f2, keys="Enter")
    （改选：send(keys="Down") → poll 看到 ">" 移到第 2 项，实测有效）

poll(s-7f2, sinceSeq=...)
  → 屏幕持续刷执行日志；state 仍是 running，但 send 随时可用
  → 不需要操作 → 去做别的任务（或同时管另一个会话）

# 嵌套交互（§3.4 实测）：从外部发指令，CLI 内部又弹一次确认
send(s-7f2, text="Run: echo <marker> and report the output", submit=true)
poll(s-7f2, sinceSeq=...)
  → screen 出现 "Are you sure you want to do this?" / "> 1. Yes" /
    "2. Yes, and don't ask again this session" / "3. No, and tell ... what to do differently"
  → 没有任何等待通知，agent 读屏自行决策（`send` 整体走 toolperm 规则裁决；确认屏上的 Enter = 批准执行，见 §3.4 / §9）
  → send(s-7f2, keys="Enter")
poll(s-7f2, sinceSeq=...)
  → delta 出现 "● <marker>" + "✔ Worked for 44s"，回到 ">"，会话仍 running
send(s-7f2, text="Reply with only: <token>", submit=true)
poll(s-7f2, sinceSeq=...)
  → 这次没有确认屏、3s 直接出结果
  → 证明确认屏不是必然，禁止脚本化 send / 固定节奏

# 异常分支（§3.2）：若 spawn 后 45s 仍 seq≈0
poll(s-7f2)
  → bootDiag:"stderr: listen EADDRINUSE ...", hint:"no output; possible startup failure"
  → agent 据此 signal/close 重跑，而不是干等或盲目 send

# 带外交互分支（§3.3 实测）：屏幕出现 "Browser didn't open? Use the url below to sign in"
poll(s-7f2) → urls:["<login-url>?state=<one-time>"], hint:"external action required"
  → agent 把链接转交人，不 send、不干等；期间 spinner 每 ~0.1s 重绘，
    delta 经稳定窗口折叠后只剩一行 "Waiting for auth..."
  → 人点完授权后继续 poll，等屏幕收敛到登录成功态

poll(s-7f2, sinceSeq=...) → state:"exited", exitCode:0
close(s-7f2)
```

# 后台健康诊断（§3.5）：poll 顺带回 health，agent 据此决定「等 / 插话 / 查诊断」
poll(s-7f2, sinceSeq=...)
  → health.verdict:"idle:awaiting_agent"  → 可以发下一条指令
poll(s-7f2, sinceSeq=...)
  → health.verdict:"spinning", changedPerChunk:1.00  → 假活重绘；别当进展，也别当卡死
poll(s-7f2, sinceSeq=...)
  → health.verdict:"suspected_stuck", readLoopAlive:true  → 查 bootDiag；health 不会自动杀进程

三个 v2 做不到的能力：**不轮询也行**（放着不管）、**多会话并发**（按 sessionId 分别 poll）、**任意时刻插话**（不必等「等待状态」）。

---

## 7. 「通过 pid 拿 buffer」的一个修正

你的模型完全成立，但 **pid 不适合当句柄主键**：

- pid 会复用；进程退出后 pid 失效，而 buffer 还要保留 60s。
- **channel 后端**（实际主路径）里 pid 是容器内 pid namespace 的编号，宿主/agent 侧看不到、也无法直接用。
- 跨实例、跨重启不可定位；服务重启后旧 pid 可能指向完全无关的进程。

建议：对外给 **`sessionId`（不透明句柄）**，内部映射 `{pid, pgid, masterFd, stdinWriter, ringBuffer, backend, idleAt}`；`pid` 一并返回，但只用于人工排查、`ps`/`kill` 关联和日志。所有工具一律收 `sessionId`。语义和你的想法一致，只是把主键换稳。

→ **v3.5 补一层**：这份「内部映射」的**唯一持有者就是 §3.7 的 manager**。`pgid` 是唯一的 kill 单位，任何工具不得自己按 pid 操作进程；pid 复用校验（`(pid, startedAt)` 双条件）也只在 manager 里做。否则「pid 不作主键」只是句口号，实际仍会被工具拿去 kill。

---

## 8. 后端实现

新增 `sandbox/session.go`（`SessionRegistry` + `ProcessOwner` + readLoop + writeLoop），新增可选接口，**不动 `Workspace` 主接口、不改 `sandbox_exec` 默认行为、不动编排层**。宿主挂载点复用现成的 `BotWorkspaceManager`（`tools.go:1114` 注入、`tools.go:92-102` 闭包捕获 botID），进程原语复用 `procgroup_unix.go` / `channel/reaper_unix.go`，不新造又一个 Manager，详见 §3.7。

```go
type SpawnRequest struct {
    Command string; Cwd string; Env []string
    Cols, Rows uint16; Term string
    Background bool
}
type SessionHandle interface {
    Poll(sinceSeq uint64, mode string) (SessionView, error)
    Send(data []byte) error
    Resize(cols, rows uint16) error
    Signal(sig string) error
    Close() error                 // 关 master + kill 进程组
}
type SessionSpawner interface {
    Spawn(ctx context.Context, req SpawnRequest) (SessionHandle, error)
}
```

- **核心改动**：`runCommandWithStreaming`（`sandbox/docker.go:351` 为函数签名与 stdin 段）的 `cmd.Wait()` + `wg.Wait()` 结构（**`:533-534`**）不能复用；新路径 `Start()` 后立即返回，readLoop 独立常驻。第三个不能复用的理由是 v3.7 补的：现有路径把进程挂在 message ctx 上（`botworkspace.go:906, 992` → `docker.go:378-380` `cmd.Cancel`），而 `agent/bot/bot.go:764-773` 在**每轮正常结束**都 cancel 该 ctx（§3.7）。
- **看门狗**：后台会话**完全不走** `docker.go:428-489` 的 stuck/hard；改 idle TTL（默认 15min 无 poll/send）+ 显式 close + 进程组 kill（复用 `sandbox/procgroup_unix.go:19-25` 的 `kill(-pid, SIGKILL)`），先 TERM 后 KILL。

| 后端 | 改动 | 坑（已核实） |
|---|---|---|
| **local** | 新增 `pty_unix.go` + `pty_windows.go`（**必须成对**，参考 `procgroup_unix.go/windows.go`）：`pty.Start(exec.Command("sh","-c",cmd))`，`cmd.Dir/Env` 沿用 `local.go:156-159`，`SysProcAttr{Setsid:true, Setctty:true}`，`Stdin/Stdout/Stderr` 全指 slave，注入 `TERM` | Windows 首版 `CodeUnsupported`，不做 ConPTY |
| **docker exec** | **旁路** `docker_exec.go:17-58` 的 `setsid`+fd3+`/proc` 扫 fd+`kill -KILL -pid` 脚本（与 PTY 语义冲突）；单独走 `docker exec -it -w <dir> -e TERM=xterm-256color <ctr> sh -c ...` | `-t` 合并 stdout/stderr；本地 stdin 非 TTY 时 `-it` 会报错，需 Docker Engine API `Tty:true` 或容器内 `script -qec` 兜底 |
| **channel（实际主路径，改动最重）** | `channel/protocol.go:100-105` 加帧常量（`MsgSessionSpawn/Stdin/Output/Exit/Signal/Resize/Diag`），`:128-144` `Capabilities` 加 `session.spawn`，`:349` 附近加请求结构；`channel/exec.go` 新增 `runSession`；`server.go:226-250` 加会话表、`:273-331` 加 dispatch case；`client.go` 新增方法 | **不要碰 `client.go:31/239` 的 `rpc` 独占锁**：会话用**独立连接**（每会话一条），帧的 `ReqID` 当 sessionId，连接上一个 reader goroutine 按 ReqID 分发 + writer mutex，现有 RPC 零改动。§3.6 Go 探针实测：同连接 `Info` 被阻塞 `2.599s`，独立连接 `283µs` / `52ms` 并发成功 |

- **诊断侧信道是各后端都必须带的**（§3.2 / §5）：**首选 CLI 结构化日志 tail**（按 pid + 级别过滤，§3.3 实测已验证可行）；local / docker 可额外接独立 stderr pipe 兜底，或 Engine API 独立 attach 流 / 容器内 tee；channel 新增 `MsgSessionDiag` 帧，与 `MsgSessionOutput` 分开。只有 PTY 字节流、没有侧信道的后端，会话状态一律视为不可信 —— 这条是真实实验换来的硬性验收（§11 P0）。
- **`SpawnRequest.Env` 必须白名单组装，禁止 `os.Environ()` 全量继承**（§3.3 第四条坑，实测把子进程的品牌 / endpoint / 登录域名整个换掉）：默认只给 `PATH / HOME / USER / SHELL / LANG / TERM` + 会话显式注入的 `TERM / COLUMNS / LINES` + agent 指定的变量；宿主注入的产品配置变量（`*_PRODUCT_CONFIG*` / `CLIENT_INFO_*` / 宿主品牌前缀 / token 类）一律剔除。容器后端同理，不要用 `docker exec -e` 透传宿主全量 env；channel 帧里只传白名单后的 env，不传宿主 env。

⚠️ **本机验证环境的两条 channel 边界**（只影响本地调试，不改变容器内 Linux 主路径）：① `reaper_other.go`（`//go:build !linux`）让 `Reaper.Start` 在 macOS 直接返回 `process control requires unix`，本机跑不了真实 channel `Exec`；② `Config` 未显式给 `SocketPath` 时 `stateDir()` 回落到 `DefaultSock` 的 `/run/...`，只读文件系统下 `Listen` 直接失败 —— 本地起 channel server 必须显式把 `SocketPath` 指向临时目录。§3.6 的探针因此用受控延迟的协议对端测 client 侧锁。

#### 配置化落点（v3.7 补，六层核查第 5 层）

§13 定稿了一堆默认值（idle TTL 15min、并发 4/32、health 窗口 5s、readLoop 心跳 2s、稳定窗口 120ms、折叠阈值 3、raw 1MB、`SeqRate>20`/`changedPerChunk>=1.5`…），v3.6 却全当硬编码写，**`sandbox.Config`（`sandbox/sandbox.go:160-266`）里没有对应字段**。误报时无法一键止损，也换不了别的 CLI 的阈值。按项目惯例（以 `sandbox.stuck_timeout` 为完整模板）补 5 处链路：

| 层 | 现成模板 | 本方案要补的键 |
|---|---|---|
| ① 键常量 | `config/keys.go:163-167` | `sandbox.session_idle_ttl` / `sandbox.session_max_per_bot` / `sandbox.session_max_total` / `sandbox.session_health_window` / `sandbox.session_stable_window` / `sandbox.session_raw_buffer` |
| ② 元数据 | `config/module.go:1183-1196` `WorkspaceMetaSpecs()` | 同上，带 Category / Description |
| ③ 默认值 | `config/module.go:1949-2011` `DefaultMap()` | 与 §13 表一致 |
| ④ 读取 + 转换 | `api/botservice.go:550-574`（`sandbox_exec` 装配处）、`:3195-3198`（另一条构造路径） | `GetInt` / `GetDuration`，两条构造路径都要读 |
| ⑤ 落到结构体 | `sandbox/sandbox.go:160-266` `Config` + `:269-284` `DefaultConfig()` + `wire.go:124-151` `fillDefaults()` | 新增字段 + 三处默认值兜底 |

⚠️ **`DefaultMap()` 的值并不自动成为运行时值**：它唯一的生产消费者是 `api/handler_config.go:39-50`，只用于 `GET /api/config` 给前端填展示空值；`config/store.go:20-35` 明确默认值由**调用方提供**，`NewStore`（`:48-57`）没有 seed 循环。现成反例：`sandbox.image` 在 `config/module.go:2008` 是 `alpine:latest`，而 `sandbox/sandbox.go:272` `DefaultConfig()` 是 `builtin`；`sandbox.require_docker` 同样两边不一致。**只改 ③ 不改 ④⑤ = 参数完全不生效**，这条要进 §11 验收。纯内部阈值（折叠次数、判据系数）可只走 ①④⑤ 不挂 `GlobalMetaSpecs()`（`sandbox.backend` 就被 `module_specs_test.go:20` 刻意排除在通用设置页外，照此办理）。

---

## 9. 生命周期 / 安全 / 可观测性

- `SessionRegistry` / manager（§3.7）：`map[sessionID]*ProcessOwner`，绑定 `botID` 隔离；上限每 bot 4 / 全局 32，超限 `CodeBusy`；服务启动/退出全量清理；`state=lost` 明确告知，禁止静默重建，`lost` 必带 `Reason` + `LastSeq`（§4 / #9）。⚠️ **`DestroyBot` / `StopBot` / `CloseAll`（`botworkspace.go:411-467`）必须补级联 `closeAll(botID)` + `Reaper.Forward(SIGTERM)`**。v3.7 精确化：真正缺的是「按 botID 遍历后台会话逐个 kill 的钩子」；`DestroyBot`/`StopBot` 在 docker 持久容器下会 `docker rm -f` / `docker stop -t 10` 连带停进程，但普通 `POST /api/bots/:id/stop` 不调用它们，`CloseAll` 与 `botWorkspace.Close()` 则确实只清表/no-op（§2）。⚠️ **`BotService.StopBot`（`api/botservice.go:3617-3694`）与 `Bot.Close`（`agent/bot/bot.go:546-602`）必须同步调 `closeAll(botID)`** —— 前者全程不碰 workspaceMgr 的这三个方法，后者只走到 `BotWorkspaceManager.Close()` → `CloseAll()` 清表，这是 v3.7 新发现的确定性收口缺口（§3.7）。⚠️ `/api/chat/abort` **不自动 kill 后台会话**（会话必须脱离 message ctx），但 `AbortSession`/`AbortBot` 要在注册表打 `orphaned` 标记并 `Infow`，回收交给 readLoop 的进程级 TTL（§3.7）。⚠️ `close` 后若检测到 `setsid` 逃逸进程，`escapedPids` 必须 `Infow` 告警，不得只报「注册表已空」（§3.7 实测：注册表空了但进程还活着）。
- **toolperm**（`toolperm/risk.go`，**v3.7 按实际机制重写**）：⚠️ 原写「显式登记 `RiskSensitive`」是错的 —— toolperm **没有 RiskSensitive 登记表**，`ToolRisk()`（`:270-283`）对「未收录 / 非 basic / 非 broadcast / 非 exfil」一律 `return RiskSensitive`（`:282`），**不登记即敏感**。真正需要显式登记的是 basic、broadcast、exfil 与前缀例外。按 §4 定稿的 `sandbox_session_*` 全名：`spawn/send/signal/close/close_all` 命中 `sensitivePrefixes` 的 `"sandbox_"`（`:241`）→ 天然 `RiskSensitive`，**零登记**；`poll/watch` 定稿 `RiskBasic` 则必须**改两处**（缺一不可）：① `sensitivePrefixExceptions`（`:255-268`）加两项绕过前缀护栏；② `basicTools`（`:59-131`）加两项让 `IsBasicTool`（`:314-336`）查表命中。现成先例就是 `sandbox_health`（`:114` basic + `:256` 例外）。另需同步 `api/handler_bot_toolperm.go:187-195, 279-316` 的 UI 清单，否则权限页选不到。回归测试照 `toolperm/risk_test.go:376-389` 的 `TestIsBasicTool_PrefixGuardWins` 补。
- `close_all` 是**跨会话批量**操作，必须校验 `botID` 取自 ToolProvider 闭包而非模型参数；`watch` 的 `agent` 同理（§4 v3.7 补）。
- **确认类按键只能工具级静态裁决**（§3.4 v3.7 更正）：~~要求显式 `allowUnsafe=true`~~ —— 该参数**全仓零匹配**；`toolperm.Service.Evaluate/EvaluateUsers`（`:220, :225`）签名只吃 `(botID, tool, platform, chatID, candidateIDs)`，**拿不到工具参数与屏幕内容**，无法按「当前是不是确认屏」动态升级。首版：`sandbox_session_send` 整体 `RiskSensitive` 走规则裁决，提示词写死「确认屏上的 `Enter`/`y` = 批准执行，必须先 `poll` 读清选项」；`poll`/`watch` 的 `RiskBasic` 不得被用来绕过 `send`。⚠️ **§3.4 实测：这类确认屏是高频路径而非理论假设** —— 从外部发一条执行 shell 的指令，CLI 立刻弹 `Are you sure you want to do this?`，按 `Yes` 等于批准执行。参数级裁决接口列 P3 单独立项，**不进首版**。
- echo-off 输入明文不进日志、不进工具结果、不进上下文，只记字节数。
- **env 脱敏**（§3.3）：spawn 日志只记 env 的 **key 列表与来源**（白名单 / agent 指定），绝不记 value；被剔除的宿主变量只记「已剔除 N 个」计数，不记名字里可能带的内网地址或凭据。`bootDiag` / `urls` 回上下文前同样过滤。
- **Infow**（硬性要求）：每条带 `sessionId, pid, pgid, botID, backend, command(脱敏), state, seq, settledSeq, lastSeq, reason, orphaned, bytesRaw, truncated, sendBytes, bootDiag, stderrTailBytes, zeroOutputMs, exitCode, elapsedMs, healthVerdict, seqRate, changedPerChunk, readLoopAlive, escapedPids, closeReason, ttlReason`（**v3.7 补 6 个字段**：双水位线、`lost` 原因、abort 标记、TTL 触发原因）；`state` 迁移、**`health.verdict` 迁移**、每次 `send`、`truncated` 覆盖、**启动后持续零输出超过 N 秒**、idle 回收逐条落日志。出问题时能回答「当时屏幕上是什么、agent 发没发、发出去之后屏幕变没变、**以及屏幕空着时侧信道里到底报了什么错**」。⚠️ 脱敏：`bootDiag` 可能带主机名 / 端口 / 路径，落日志与回上下文前必须过滤（遵守仓库信息边界）。

---

## 10. 前端接管（P2）

复用 `api/handler_bot_runtime.go:306-361` 的 `desktopUpgrader`（`:306-318` 是带同源校验的 `websocket.Upgrader`，`:331-361` 是 `serveBotDesktop`）+ `sandbox/desktop.go:148-214` 的 `RelayDesktop` 全双工模板，新增 `GET /api/bots/:id/sessions/:sessionId`；`XtermConsole.vue` 的 `term.onData → ws.send`、`onmessage → term.write`、`ResizeObserver → resize 帧`，Ctrl+C 不再只本地回显。人能实时看 agent 在操控的每个会话，也能直接接手输入。旧 HTTP 逐行 exec 保留作降级。

**v3.7 按六层核查第 6 层把链路追到底，修正三处**：

1. **`services.js:2376-2379` 不是「预留的 PTY 路径」**。原文是设想性 TODO 注释：`真实后端建议走 WebSocket：ws(s)://<host>/api/bots/:botId/terminal` + `下方 mock 仅做本地命令回显，供前端联调 UI`。该 ws 地址**根本没有实现**（`api/router.go:205-206` 是 REST GET/POST，WebSocket 只有 `:207` `/:id/desktop`）。表述改为「前端早有 WebSocket 设想注释，但需本方案 P2 新建接口」。
2. **`XtermConsole.vue` 的真实数据源既不是轮询也不是 SSE**，而是 props 注入的 `connect` / `exec`（`:33-36`），`runCommand`（`:86-129`）一次 HTTP 拿整段输出，父组件 `BotTerminal.vue:20-21` → `services.js:2454, 2461` → `api/handler_bot_runtime.go:238-304` `handleBotTerminalExec` 同步 `ws.Exec`。输入与 Ctrl+C 全是本地模拟（`:139-215, 196-200`），头部状态是本地 ref。§2 事实 8 的「一行命令一次 HTTP 模拟终端」准确。
3. **`RelayDesktop` 只是「全双工框架」可复用，不能直接当 PTY 用**：① 它中继的是 **RFB / X display 字节流，不是 PTY**（`desktop.go:161-204`）；② ctx 是 `c.Request.Context()`（`handler_bot_runtime.go:360`），**不接 message ctx / abort 链**；③ 收尾只 `cmd.Process.Kill()` 直接子进程（`desktop.go:164-170`），**没有进程组 / Reaper 收口** —— 正好是 §3.7 明令禁止的写法。PTY、resize 帧、会话注册、进程组级联全要补。

**可零改动复用的前端组件（v3.7 补，比原方案具体）**：`web/src/components/SessionList.vue:29-33` 系统级「运行中 ×N」徽标、`:87-92` 每会话「运行中 {elapsed}」+ 就地停止按钮、`:258-282` `onStop` → `abortSession`；`web/src/stores/bot.js:92, 101, 136, 1860` 的 `runningTasks` / `refreshRunningTasks` / `runningBySession` 聚合与 **15s `setInterval` 轮询**；后端 `GET /api/chat/active`（`api/handler_chat.go:286-306`）已返回 `tasks/traceIds/unattributed`。P2 只需把数据源从 traceID 扩到 `sessionId`，**UI 骨架不用新造**。⚠️ 但必须区分两类「运行中」：点停止会 cancel 的 trace 任务 vs **不会随 abort 停止的后台会话**（§3.7）；后者要单独标 `orphaned` / 给独立的关闭入口，否则用户会以为点停止就全停了。

### 10.1 P2 可执行实施路径（11-12 复核后补齐，按此顺序即可开工）

> 前提：§0.1 的 P2 后端前置契约（`RawSince` / `resizeScreen` / `bufMu` / `List`）已落地并单测验证。下面每一步都标了**已核实的代码落点**，不依赖任何设想接口。

**Step 1 — 会话列表 REST：`GET /api/bots/:id/pty-sessions`**

> ⚠️ **实施时实测纠正**：原稿写的是 `/api/bots/:id/sessions`，但 `api/router.go:263-264` 早就被**聊天会话 CRUD**（`handleListSessions` / `handleCreateSession`）占用，前端 `sessionApi.list()` 也走它。直接注册会撞车或吞掉旧接口。全部改到独立命名空间 `pty-sessions`；前端对象同理命名 `botPtySessionApi`，避开已有的 `sessionApi` / `sessionToolApi`。

- 注册到 `api/router.go:110-111` 的 `botsAdmin` 组（已有 `requirePermission(auth.PermBotManage)` + 登录 cookie），与 `:205-207` 的 terminal/desktop 同级。另注册 `DELETE /:id/pty-sessions/:sid`：列表里的「关闭会话」按钮是显式人工动作，**不该为了关会话先开一条 WebSocket**。
- 取进程所有者**只能**走 `s.botSvc.RunningBotWorkspaceMgr(botID)`（`api/botservice.go:280-292`）→ `mgr.Sessions().List(botID)`。⚠️ **不要用 `WorkspaceManagerForBot(botID)`**（`:390-`）：bot 未运行时它会临时构造一个**新的** `BotWorkspaceManager`，其 `SessionRegistry` 是另一个实例，页面会显示空列表且不报错。bot 未运行直接回 409。
- `List` 已把 `Delta` / `Screen` 清空，只回 `sessionId/pid/backend/state/health/reason/orphaned/urls/seq`，供列表徽标用；详细画面走 Step 2。

**Step 2 — 单会话 WebSocket：`GET /api/bots/:id/pty-sessions/:sid/stream`**

- 实施落地为 `ptySessionUpgrader`（保留同源校验，额外**只在两端都是 loopback 时**放行 vite dev proxy 的场景：代理 `changeOrigin:true` 把 Host 改成后端端口、Origin 仍是 dev 端口，否则本地联调必定 403）。复用 `api/handler_bot_runtime.go:350-360` 的 Upgrade / `defer conn.Close()` 写法；**只借框架，不借 `RelayDesktop`**（§10 修正 3：RFB 字节流、ctx 绑 HTTP request、收尾只 Kill 直接子进程）。
- `Upgrade` **之前**先 `reg.RawSince(botID, sid, 0)`，一次性完成「存在性 + botID 归属 + 初始快照」校验，失败回 HTTP 错误而不是先升级再关。
- **写循环**（服务端独立 goroutine，~50ms ticker 或 `RawSince` 长轮询）：
  - 二进制帧 = `RawChunk.Data` 原始 PTY 字节；游标用返回的 `chunk.Seq`（**`Seq` 不是 `SettledSeq`**）。
  - `chunk.Resync=true` → 先发 JSON `{"type":"reset"}` 让前端 `term.reset()`，再发整窗快照；ring buffer 溢出后绝不能假装流是连续的。
  - `state` 从 `running` 迁移时发 JSON `{"type":"state","state":...,"reason":...,"exitCode":...}`，随后停写循环但**保留 socket 直到前端断开**，让人还能看完最后一屏。
  - 帧要做大小上限与慢客户端处理（写超时即断开），别让一个卡住的标签页拖住 goroutine；raw buffer 本身已是定长 ring。
- **读循环**（`conn.ReadMessage`）：JSON `{"type":"input","data":"<base64>"}` → `reg.Send(botID, sid, bytes)`；`{"type":"resize","cols":n,"rows":n}` → `reg.Resize(botID, sid, n, r)`（后端已同步 daemon 网格）；`{"type":"close"}` → 显式 `reg.Close`。⚠️ **WS 断开只 `conn.Close()`，绝不自动 `reg.Close`** —— 会话存活与浏览器标签页解耦，这是 §3.7 所有权原则的直接推论。
- ⚠️ **Ctrl+C 走字节、不走 signal**：xterm `onData` 送来 `\x03`，由 PTY 行规转成前台进程组 SIGINT；**不要**映射成 `reg.Signal("INT")` —— §3.4 实测后者在 raw TUI 里直接把会话打退出。
- ⚠️ **权限语义要写进 UI**：toolperm 只裁决 LLM 的工具调用，人工 WS 输入天然绕过它。所以端点必须留在 `botsAdmin`（BotManage 权限），UI 上明确标「人工接管」并留审计 `Infow`（`sessionId/botID/bytes/who`），不能做成匿名可见的实时终端。
- ⚠️ `RawSince` 会 `Touch()`：标签页开着即视为活动，idle TTL 不触发。这是「人在接管」的预期语义，但要接受「忘了关标签页 → 会话不自动回收」；若要收紧，按 `document.visibilityState` 决定是否继续发心跳，别在后端猜。

**Step 3 — 前端 API 层**：`web/src/api/services.js` 新增就地命名导出 `botPtySessionApi`（本文件无聚合 export），含 `list(botId)` / `streamUrl(botId, sid)`（由 `location.protocol/host` 推 `ws(s)`）/ `remove(botId, sid)`。删掉原 `:2376-2379` 那段「建议走 `ws(s)://<host>/api/bots/:botId/terminal`」的 TODO —— §10 已核实它**从未实现**，留着会把后来人指向不存在的新端点。旧 `botTerminalApi` 原样保留作降级。

**Step 4 — 新建 `web/src/components/common/SessionConsole.vue`，不要改造 `XtermConsole.vue`**

- 理由（已核实）：`XtermConsole.vue:74-77` 自造提示符、`:139-215` 是完整本地行编辑器（`inputBuf` / `prompt()` / `runCommand`、`:214` 本地回显、`:203-211` 拦下方向键、`:196-200` 假 Ctrl+C）。接到真 PTY 上会**双回显**、吞掉 `\x1b[A` 等序列、把 `exec` 语义塞进流模式。改造等于删掉 ~80 行既有行为，还会伤到现有终端页。
- 新组件只做四件事：`Terminal` + `FitAddon`（主题块照抄 `:264-273`）；`term.onData(d => ws.send(input 帧))`；`ws.onmessage` 二进制 → `term.write(new Uint8Array(ev.data))`、JSON `reset` → `term.reset()` 后重放、`state` → 更新头部；`ResizeObserver` → `fit()` 后发 resize 帧（带 `term.cols/term.rows`，别只 `fit()`）。`onBeforeUnmount` 只 `ws.close()`，**不发 close 帧**。
- 组件加卡片后必须跑 `web/` 下 `npm run build`：prebuild 的 `web/scripts/check-template-bindings.mjs` 会查未定义模板绑定，`vite build` 本身不查（项目已踩：渲染中断、按钮不进 DOM）。

**Step 5 — 接进会话面板（实施落点）**：主面板是 `web/src/components/bot/BotPtySessions.vue`（左侧列表 + 右侧 `SessionConsole`），挂在 `web/src/views/BotSettings.vue` 新增的「交互会话」tab（左导航 + `v-else-if` 链 + import 三处，`activeKey` 支持 `?tab=pty-sessions` 直达）。`web/src/stores/bot.js` 新增 `interactiveSessions` / `interactiveRunningCount` / `refreshInteractiveSessions()`，接进 `watch(activeBotId)` 的**同一个 15s `setInterval`** 并在 return 中显式导出（setup store 不自动暴露）；`web/src/components/SessionList.vue` 只加一条**系统级**「后台交互会话 ×N」徽标 + 「查看」跳转，**不动**聊天会话行里的运行中徽标与停止按钮。⚠️ 两类「运行中」严格分列：trace 任务用现有 `abortSession` → cancel；后台会话用**独立的「关闭会话」**按钮（REST DELETE）并额外显示「已脱离回合」（`orphaned`），绝不复用 `abortSession`——abort 只打标记、不杀进程（§3.7 / v3.7 修订）。

**验收用例（P2 完成标准）**

1. `sandbox_session_spawn` 起 `cat`/真实 TUI → 列表立刻出现该 `sessionId` 且 `state=running`；bot 停用时列表 409 而不是空列表。
2. 打开控制台能拿到**此前已产生**的输出（验证 `RawSince(0)` 全量重放），之后新输出逐帧跟随，无重复行、无半截 ANSI / Unicode。
3. 人在 xterm 输入 → agent 侧 `sandbox_session_poll` 的 `screen`/`delta` 能看到；agent `send` → 人侧也能看到（双向闭环）。
4. 拉窗口 → `tput cols` 随之变化，且 `poll` 的 `Screen` 宽度与真实终端一致（回归 §10.1 前置缺陷 2）。
5. 关标签页 → 会话仍 `running`，agent 继续可 `poll/send`；重开标签页能接着看。
6. Ctrl+C 只中断当前程序、**不**把会话打退出（区分 `\x03` 字节与 `signal(INT)`）。
7. `orphaned` 会话显示独立徽标；点「停止」(trace abort) 不会杀掉它，点「关闭会话」才会。
8. 1MB ring 溢出后收到 `reset` 帧，xterm 重置而非显示错乱拼接的流；`go test -race ./sandbox ./api` 无数据竞争。
9. `npm run build` 通过（模板绑定检查），旧 `BotTerminal.vue` 逐行 exec 行为零回归。

**实施记录（11-12，Step 1–5 已落地）**

| 验收 | 状态 | 证据 |
|---|---|---|
| 列表 / 409 / botID 隔离 | ✅ | `TestHandleListPtySessions_RequiresRunningBot`、`TestPtySessionListAndDelete_AreScopedToBot` |
| 全量重放 + 二进制帧 + state 帧 | ✅ | `TestPtySessionStream_ReplayInputResizeAndExplicitClose`（fresh attach 先收 binary `hello`，再收 `state=running`） |
| 输入 / resize / 显式 close | ✅ | 同上；`TestPtySessionStream_RealPTYRoundTrip` 用真实 `sh` 拿到 `GOT:PING42` / `GOT:AFTER-RESIZE` |
| 关标签页不杀会话 | ✅ | `TestPtySessionStream_DisconnectDoesNotCloseSession` |
| 跨 bot 拒绝 | ✅ | `TestPtySessionStream_RejectsCrossBotBeforeUpgrade`（Upgrade **之前**回 403） |
| `go test -race ./api` | ✅ | 全部 PTY 测试 race 通过 |
| `npm run build` / 回归脚本 | ✅ | 模板绑定检查全过；`npm run test:regression` 10 个脚本全过 |
| 浏览器实机手工验收 | ✅ 已做 | 隔离实例（`:18080` + 独立 DB + 临时二进制）跑完上面 9 条，另加 member 只读视图与 Origin 拒绝；详见 §0.1 末段 |

**实施时又被真实代码纠正的 3 处**（都是「文档看着对、落地就踩」）：

1. **路由撞车**：`/api/bots/:id/sessions` 是聊天会话 CRUD（`api/router.go:263`），不是空闲路径。改 `pty-sessions` 命名空间，前后端对象名同步避开 `sessionApi`。
2. **`Resync` 不能等于 sticky `truncated`**：`ringBuffer.Since` 原返回 `r.truncated || seq < oldest`，而 `r.truncated` 一旦溢出**永远为 true**。若照原稿直接用，第一次 ring 溢出后 WebSocket 每 50ms 都会发 `reset`，xterm 被反复清屏。现改为按 cursor 判 gap（`seq < oldest`），并补回归测试断言「溢出后 `Since(currentSeq)` 不得报 gap」「未来游标 clamp 而非 gap」。
3. **resize 帧是不可信输入**：cols/rows 来自浏览器，`RenderScreen` 会按它分配 `[][]vtCell`。`resizeScreen` 加 1000×500 上限；WS 侧另有 1MiB 帧上限、输入长度上限。⚠️ **实机验收后又补了一层**：最初只有网格被钳制，`pty.Setsize` 仍收到原始值，注释却写着「PTY 由调用方同值限制」（没有任何调用方限制）。现由 `sandbox.ClampWinsize` 在 `SessionRegistry.Spawn` / `Resize` 两处统一钳制，WS 侧按 last-applied 去重 no-op 帧；前端容器固定高度 + 变化去重，杜绝 `ResizeObserver` ↔ `fit()` 反馈环（实测 4434 帧 → 1 帧）。

另补一个协议细节：`close` 帧会先置 `closeRequested` 再调 `reg.Close`，因为 `Close` 会**立刻从 registry 删除条目**；否则 writer 下一拍 `RawSince` 会拿到 `ErrSessionNotFound`，用户看到的是「会话不可用」而不是干净的 `state=exited`。

⚠️ **P2 表里另两项不属于本路径**：`background:auto` 自动升级与 `watch` 事件唤醒是**独立**的 agent 侧能力，前端接管不依赖它们；不要为了「顺手」把 SSE 唤醒塞进这条链路，否则会绕过 toolperm（§11 P2 验收明确要求不得绕过）。

---

## 11. 分阶段落地

| 阶段 | 内容 | 验收用例 |
|---|---|---|
| **P0 — spawn 内核**（不做任何等待判定） | `SessionRegistry` / manager（§3.7）+ ring buffer + readLoop 解耦 + local 后端 + **按 §4 全名表注册**的 6 个 `sandbox_session_*` 工具（⚠️ 禁止裸 `spawn`）+ 7 处清单同步 + 配置化五处链路 + 提示词（修 `tools.go:34/63`）+ toolperm 登记 + `Infow` + **诊断侧信道** | ① F 类常驻 TUI（边刷屏边听 stdin）：spawn 立即返回句柄，poll 拿得到持续输出，任意时刻 send 能生效，进程不被看门狗杀；② A 类 `read -p` 问答可控；③ `npm init` 多轮问答；④ sync `exec` 行为零回归（`go build`、`git`、超时/截断）；⑤ readLoop 停止消费时不会把子进程写阻塞；⑥ 服务重启无孤儿进程；⑦ **启动失败反例（§3.2）**：PTY 下 45s 零输出 alive，`bootDiag` 能拿到 `EADDRINUSE`，`hint` 不给「等待输入」诱导；⑧ 同一命令 pipe 对照 `exit 1`，两者可自动比对；⑨ 侧信道缺失时 `poll` 明确回 `diagUnavailable`，不伪装成正常空屏；⑩ **真实 CLI 完整链路**（§3.3）：`spawn→poll→send(Enter)→poll→send(Enter)→poll` 能走到登录并抽出带一次性 state 的带外 URL，跨多次调用会话不丢；⑪ `send` 后 `seq` 因回显推进**不得**被误判为 `accepted`；⑫ **env 白名单**：同一 CLI 分别在「全量继承宿主 env」与「白名单 env」下 spawn，两者 `urls` 里的登录域名 / `platform` / `version` 必须不同，且白名单侧与包内 `product.json` 默认一致 —— 用可观测输出证明验证对象是真实 CLI 而非被宿主产品配置劫持的副本；⑬ **外部操作闭环（§3.4）**：`send(text, submit=true)` 一条指令 → `poll` 读到 CLI 内部确认屏 → `send(keys=Enter)` 应答 → `poll` 拿到可机器核验的执行结果 marker；⑭ **多轮 + 无确认屏分支**：同一会话第二条不调工具的指令不得再弹确认屏、仍能拿到结果，且结果只出现在一次 `delta` 中（验证 `sinceSeq` 不漏不重）；⑮ **长任务**：44s 任务期间任一 `poll` 均快速返回、会话不被看门狗杀；⑯ **`signal(INT)` 语义**：发 INT 后 `state=exited`、后续 `send` 明确失败，提示词不得写「中断后继续用」；⑰ **确认屏非默认分支**：选项 2 / 3 / `Esc` 三条路径可用，且选 3 时执行结果 marker 不出现（证明命令未跑）；⑱ **`poll.health` 必返**：daemon 内部信号齐全，`health` 不触发任何自动 signal/close；⑲ **进程所有权（§3.7）**：`DestroyBot` / `StopBot` / `CloseAll` **以及 v3.7 补的 `BotService.StopBot` / `Bot.Close`** 后该 bot 全部后台进程（含进程组内孙进程）均退出，`escapedPids` 有告警；⑳ **botID 隔离**：`watch(otherBot)` 看不到本 bot 会话，错误 bot 的 `poll/send` 被拒；㉑ **`watch` 零副作用**：调用前后 `seq` / `sinceSeq` / delta 均不变，且不产生任何 send/signal/close；**㉒ 工具全名可见（v3.7 硬阻塞）**：`ResolveTools` 返回列表里 6 个 `sandbox_session_*` 全部出现，且**不存在**被 `subagent` 的静态 `spawn` 吞掉的同名工具；`WorkspaceToolNames()`、`fallbackToolList()`、`profile.go` 档位、`tools_test.go` 计数四处清单一致；**㉓ toolperm 分级**：`spawn/send/signal/close/close_all` = `RiskSensitive`、`poll/watch` = `RiskBasic`（`sensitivePrefixExceptions` + `basicTools` 两处都改到，照 `risk_test.go:376-389` 回归）；**㉔ SubAgent 边界**：`IsSubagent=true` 时按 §4 定稿结果决定屏蔽或放行，且必须有断言；**㉕ `agent` 不可伪造**：模型传 `agent` 参数被忽略/报错，换名字拿不到 `sinceSeq=0` 的历史重放；**㉖ 脱离 message ctx**：agent 正常回完一轮（`finishMessageLifecycle` cancel）与用户点「停止」（`/api/chat/abort`）后，后台会话仍 `running`、**不自动 kill**，注册表打 `orphaned` 并 `Infow`；**㉗ 进程级 TTL 不依赖 agent**：abort 后全程不再调 `watch`/`spawn`，`lastTouch` 超过 TTL 时 readLoop 自行 TERM→KILL，进程 `kill(pid,0)` 返回 ESRCH，`state=exited` + `reason=idle_ttl`（**这一条专门验 v3.7 修的 lazy GC 洞**）；**㉘ 确定性收口**：`BotService.StopBot` / `Bot.Close` 后该 bot 全部会话进程退出；**㉙ 双水位线不丢内容**：稳定窗口 120ms 内连续高频 `poll`，每个执行结果 marker 恰好出现在一次 `delta` 中，agent 用 `settledSeq` 推进不漏不重；**㉚ 幂等性**：§5 的 `render(raw)` 重复调用与任意分块喂入结果一致；**㉛ `lost` 契约**：`poll` 在 `state=lost` 时必返 `Reason` + `LastSeq`；**㉜ 配置化生效**：改 `sandbox.session_idle_ttl` 后运行中实例行为确实变化（防「只改 `DefaultMap()` 不生效」）；**㉝ 基线**：在 **Linux 实例**上 `go test -count=1 ./sandbox/...` 全绿（本机 darwin 基线是红的，见 §2） |
| **P1 — PTY + 视图治理 + 主路径** | PTY / VT 网格 / alt-screen / resize / ANSI 洪水折叠 + channel 后端帧协议与独立连接 + docker exec `-t` | `vim`、`htop`、`git add -p`；`screen` 与真实画面一致；1 分钟刷屏后 `poll` 返回的 diff 不含重复重绘；`tput cols` 随 resize 变化；容器内 Ctrl+C → SIGINT；**真实 CLI 首屏（无 alt-screen、`csi=87`、Unicode 框线）能被降噪成可读纯文本**；`send(keys=Down/Up)` 后 `>` 选中标记确实移动；`ISIG=0` 下 `signal(INT)` 能中断；**spinner 折叠率**：62 chunks 的 `delta` 里 `Waiting for auth...` 重绘行 ≤1 条且带 `collapsed:N`；**跨 chunk 完整性**：screen 不出现半截 `[38;5;79m` / `[22m` 或被打断的 Unicode 框线；`urls` 从全量 raw 抽，长链接不被 chunk 边界切断；**health 混淆矩阵 8/8**（§3.5）；**真实回放假活识别**：`ch/chunk≈1.00` / `0.00` 窗口判 `spinning`，`1.60~6.00` 判 `progressing`；**确认屏优先**：`actionable=true` 时即使 `seqRate>3000`、`ch/chunk≈2.0` 也必须回 `awaiting_action`；**channel 独立连接**（§3.6）：Linux 实例上同连接 `Info` 在 `Exec` 期间被串行、独立连接毫秒级返回且能并发 `Exec`，帧 `ReqID` 分发不串流；**manager 级联（§3.7）**：`closeAll` 后组内孙进程 `kill(pid,0)` 返回 ESRCH，`setsid()` 逃逸者被记录并告警，`watch.total=0` 时不得宣称无残留进程；**per-agent cursor**：`watch.sinceSeq → poll(delta=N) → watch.sinceSeq=seq → 再 poll(delta=0)`；**注意力排序**：确认屏 P0、spinner 折叠进 quiet，真实 CLI 弹出内部确认屏后 attention 从 1 变 2 |
| **P2 — 自动升级 + 接管** | `background:auto`（只用输出侧判据，不检测 stdin）、多会话并发 + **`watch` 事件唤醒**（首版纯 pull，不在回合里没人 watch）、前端 WebSocket 接管、会话列表 UI | 一条 `sandbox_exec` 长跑命令在 2~3s 内自动转后台且**不丢已收输出**；同时管 4 个会话互不串扰；前端实时跟随；**「零输出 alive」不得被升级为后台会话**（§3 负面条件）；唤醒路径复用 `OnMessageStart` / SSE，且不得绕过 toolperm |
| **P3 — 可选 hint** | `/proc` 旁证、termios 分类、prompt 正则仅作为 `hint` 附在 `poll` 结果；看门狗忙/死锁分流；OS 级进程冻结检测（Linux only，macOS 不等价） | hint 关掉后功能完全不受影响（证明它不是门禁）；构造样本 + **真实 CLI** 混淆矩阵（含 §3.2 的零输出失败样本）；**health 边界**：`J_frozen_idle` 允许回 `idle:awaiting_agent`（不承诺检测输入框可见时的冻结）；`readLoopAlive=false` 必须回 `unhealthy:daemon` 而非 `suspected_stuck` |

> **P2 状态（11-12，已全部收口）**：上表 P2 行的四项**均已实施** —— 前端 WebSocket 接管 / 会话列表 UI 按 §10.1 落地并用真 PTY 端到端验证；`background:auto` 自动升级与 `watch` 长轮询事件唤醒按 §0.1 落地，含真 PTY 端到端与 toolperm 一致性单测。
> ⚠️ 两处**契约微调已写进 §0.1**：① auto 升级必须在工具定义期复用 toolperm 对 `sandbox_session_spawn` 的裁决，否则构成提权后门；② 「复用 `OnMessageStart` / SSE 唤醒」落地为**工具内长轮询 + 既有 `SendProgress` 心跳**，不做回合外主动拉起新回合（会改编排层并绕过 agent 决策）。原表「复用 OnMessageStart」按此理解，勿再按字面另起一条注入链路。
> 浏览器实机验收已于 11-12 在隔离实例完成（§0.1 末段），并修掉实机打出来的 resize 反馈环。**仍缺：commit / 重启本机常驻服务 / 生产部署**，以及 P3 可选 hint（按设计明确不做）。

⚠️ **落地顺序**：判定逻辑已不是核心，P0/P1 可以在 local 先跑通；但 channel 是实际主路径，P1 必须在其上做真实验证（本项目踩过「机制存在 ≠ 机制生效」4 次）。验证纪律：构造样本只证明机制可行，**必须再用真实 CLI + 真实日志复核频次**。真实 CLI 复核已完成一轮（§3.2），结果又应验了一次 —— 合并 PTY 让启动失败在屏幕上完全不可见，纯靠「机制看起来有」会把这条漏到线上。

⚠️ **基线纪律（v3.7）**：六层核查要求「改动前先跑一遍基线测试并确认是绿的」，本机**不满足** —— `go test -count=1 ./sandbox/...` 15.5s 内 8 个 fail，全是 darwin 环境限制（`confinement is not available on darwin` ×5、`process control requires unix`、`process list requires linux`、`env: sh: No such file or directory`、以及一个 `Aborted:true`），`sandbox/channelclient` 通过。这些失败与本方案无关（工作区仅 `?? docs/design-agent-interactive-pty.md`，实现代码零改动），但**不能在红基线上判回归**：实施第一步须在 Linux 实例重跑全绿并存档输出，之后每次改动才用同一环境对比。另注：项目长期记忆里「真实 LLM 集成测试会 flaky」，并发跑多个包时尤其容易挂，判回归前单独复跑。

**channel 主路径上线前必须补的一步**（§3.6 只做到 client 侧锁实测）：在线上 Linux 容器实例里，① 起两条独立连接，一条 `Exec("sleep 3")`、另一条同窗口 `Info` + 短 `Exec`，核对后者不被串行；② 用新增的 `MsgSessionSpawn` 起一个 PTY 会话，核对 `MsgSessionOutput` / `MsgSessionDiag` 两路帧不串流、`ReqID` 分发正确；③ 跑一次 §3.2 的启动失败反例，确认容器内 `BootDiag` 能拿到错误原文。三项都通过才把 P1 标为完成。

---

## 12. 明确不做

- **不把 `awaiting_input` 当门禁或状态机核心**（实测 F 类程序永远不触发）；最多作为可选 hint。
- **不把「零输出 / 静默 / 进程存活」当成等待、正常或可升级的证据**（§3.2：真实 CLI 隐藏启动失败正是这个形态）；静默一律先查诊断侧信道。
- **不检测「stdin 是否被监听」**（§3.1）：外部观测原理上分不出来，epoll 之外的路径都不可靠，主动探针又有副作用。句柄永远可写，不设这道门。
- **不事后 hook 已阻塞的进程**：启动即建句柄，只切换返回时机，不重启、不丢 buffer。
- **不引入 tmux / screen / expect 二进制**（纯 Go，反对外部启动器）。
- **不把 PTY 原始输出流直接注入 LLM 上下文**、不改 `llm/orchestrate.go:1198-1205` 的消息回灌。
- **不用 `health` 自动杀进程 / 自动中断**：`suspected_stuck` 只是 hint，处置权在 agent / 人（§3.5）。
- **不承诺检测「输入框可见时的进程冻结」**：PTY 侧与健康空闲同形，需有副作用探针或 OS 级状态，列 P3 可选且不跨平台（§3.5）。
- **manager 不替 agent 决策**：不自动 send、不自动应答确认屏、不因 health 自动杀进程（§3.5 / §3.7 一致）；`watch` 只排序，不执行。
- **不新造又一个 Manager、不新起独立 ticker goroutine**：宿主复用 `BotWorkspaceManager`，进程原语复用 `procgroup_unix.go` / `Reaper`。⚠️ **v3.7 修订**：lazy GC **只负责清注册表内存条目，不再负责杀进程**（否则 abort 后无人 `watch`，TTL 永不触发）；**进程级 idle TTL 由本就常驻的 readLoop 每轮顺带比对 `lastTouch` 后 TERM→KILL**，零新增 goroutine、零外部定时器，仍符合「纯 Go、不要启动器」的偏好（§3.7）。
- **不宣称 `closeAll` 收干净所有进程**：`setsid()` 逃逸者由容器内 subreaper 接管 + `escapedPids` 告警；宿主侧只报不杀。
- **不自动应答确认/密码**；**Windows ConPTY** 首版 `CodeUnsupported`；**不改 `sandbox_exec` 默认同步语义**。
- **`/api/chat/abort` 不自动 kill 后台会话**（v3.7 明确）：会话必须脱离 message ctx，否则每轮正常结束就会被 `finishMessageLifecycle` 杀掉；abort 只打 `orphaned` 标记 + `Infow`，回收交给 readLoop 的进程级 TTL，确定性收口走 `StopBot` / `Bot.Close` 的同步 `closeAll(botID)`（§3.7）。⚠️ 这条**必须与前端「运行中」徽标的语义区分**，否则用户会误以为点停止就全停了（§10）。
- **不按「确认屏内容」动态升级 toolperm**（v3.7 明确）：`Evaluate` 只吃工具名，参数级裁决列 P3；首版靠「`send` 整体敏感 + 提示词」兜（§3.4 / §9）。

---

## 13. 决策记录（全部已定稿）

> ✅ **全部 9 项已定稿**：#4 于 v3.2 定稿，其余 8 项于 v3.6 按「露娜无异议即按此执行」条款采纳建议值（露娜 11-11「按你建议」）。
> 定稿的是**设计取舍**，不是验证结论；未验证项仍按 §11 验收。推翻任一项只回退该项，其余不受影响。
> **v3.7 六层复核后 9 项决策值一项未变**；#7 / #9 追加的是「执行位置 / 契约字段」的澄清（原文档把要求写在了决策里却没落到结构体），#7 的「不新增独立 ticker」意图保留、只是把进程级 TTL 挪到已有 readLoop。新增的工具全名、清单同步点、配置化链路、双水位线、SubAgent 边界、基线纪律属于**实现约束**，不占决策编号。

1. ✅ **返回策略 = 默认 sync 不变 + 显式 `background=true`**；`auto` 留到 P2。理由：风险最低、不动现有命令可靠性，三者可渐进。（未采纳：默认全后台 / 直接上 `auto`。）
2. ✅ **句柄 = 对外 `sessionId` 主键，`pid` 仅附加**。理由见 §7（pid 复用 / 容器 pid namespace / 退出后 buffer 保留 60s）。
3. ✅ **auto 判据 = 只用输出侧信号**（`CSI 重绘密度 / termios raw / 可见结构化选项 / 存活`），**alt-screen 降为加权项**（§3.2 实测 `alt=0 csi=87`），**不引入 stdin 监听检测**，「N 秒零输出 alive」作为**排除项**转启动诊断而非升级；P2 才做，P0 只靠 agent 显式选 spawn。
4. ✅ **启动诊断侧信道 = 结构化日志 tail 优先 + stderr pipe 兜底**（v3.2 定稿）。原 `merged:true` 被 §3.2 推翻（PTY 合并把 `EADDRINUSE` 吞成「零输出 alive」）；§3.3 实测日志 tail 零 fd 侵入即可抓 `401` + backoff，而 stderr 接 pipe 会让 `isatty(2)=false`。按会话 `pid` + `[Error]/[Warning]` tail CLI 日志，上限 20 行 / 4KB，汇总进 `StderrTail` / `BootDiag`，落日志与回上下文前脱敏；**仅当后端拿不到 CLI 日志时才启用 stderr pipe**，启用前做 A/B 对照。未采纳：无 TTY preflight、仅零输出告警。
5. ✅ **buffer = raw 1MB 环形 + `truncated`**；**VT 网格自研 ~300 行最小集**（CSI 移动 / 擦除 / alt-screen / wrap + scrollback），不引 `hinshun/vt10x`；必须带增量 UTF-8 decoder + `pendingEscape`（§3.3 实测）。
6. ✅ **依赖 = 新增 `github.com/creack/pty`**（纯 Go、无 cgo），不用 `x/sys/unix` 自写 `openpty`。
7. ✅ **idle TTL 15min / 并发每 bot 4、全局 32**；**前端 WebSocket 接管不进首版（P2）**；**会话不持久化**（内存 + `Infow`，不新增表）。**v3.7 澄清（不改决策值）**：TTL 由 **readLoop 顺带执行**（进程级 TERM→KILL），lazy GC 只清注册表；abort 后不自动 kill 会话，但 `BotService.StopBot` / `Bot.Close` 必须同步 `closeAll(botID)`。全部阈值须走 §8 的配置化链路，不得硬编码。
8. ✅ **`health` 首版只做诊断、零自动处置**：`suspected_stuck` 只提示 agent 查 `bootDiag`；不做「连续 N 次自动 `close`」、不做「`unhealthy:daemon` 自动重建会话」。理由：重建涉及 `state=lost` 与 §9「禁止静默重建」。
9. ✅ **`state=lost` 认领策略 = 方案 ②：容器内 idle TTL 自杀，宿主一律回 `lost` 让 agent 重开**（露娜「按你建议」直接确认）。不做握手重新认领 —— 避免 pid 复用风险；代价是丢掉已投入的长任务上下文。`lost` 返回须带 `reason` + `lastSeq`，提示 agent 重开而非继续 poll。**v3.7 补齐契约**：`SessionView` 原有字段里**根本没有 `Reason` / `LastSeq`**（定稿要求与结构体脱节），已在 §4 补上；`Reason` 枚举 `daemon_gone | conn_lost | idle_ttl | signal | close | exit`，让 agent 能区分「重开」与「继续 poll」。

### 已确认的实现默认值（v3.6 定稿，直接按此实施）

| 项 | 默认值 | 依据 |
|---|---|---|
| 返回策略 | 默认 sync 不变 + 显式 `background=true`；`auto` 留到 P2 | 风险最低，不动现有命令可靠性（#1） |
| 句柄 | 对外 `sessionId` 主键，`pid` 仅附加 | §7（pid 复用 / 容器 pid namespace / 退出后 buffer 保留） |
| PTY 依赖 | `github.com/creack/pty`（纯 Go、无 cgo） | 比自写 `openpty` 稳（#6） |
| VT 网格 | 先自研最小集 ~300 行（CSI 移动 / 擦除 / alt-screen / wrap + scrollback） | #5；§3.3 实测还须带增量 UTF-8 decoder + `pendingEscape` |
| buffer | raw 1MB 环形 + `truncated` 标记；退出后保留 60s | §5 |
| 稳定窗口 / 折叠阈值 | 窗口 120ms；同行窗口内变化 >3 次折叠为 1 行并标 `collapsed:N` | §3.3 实测 62 chunks / 30+ 行 spinner 重绘 |
| idle TTL / 并发 | 15min / 每 bot 4、全局 32 | §9 |
| 前端 WebSocket 接管 | 不进首版（P2），旧 HTTP 逐行 exec 保留作降级 | #7 |
| 会话持久化 | 内存 + `Infow`，不新增表 | #7 |
| health 窗口 / 动作 | 固定 5s 滑动窗口；readLoop 心跳 >2s 判 daemon 故障；只诊断、零自动处置 | §3.5 实测 8/8 + 真实回放 |
| manager 挂载 / GC | 挂 `BotWorkspaceManager`；lazy GC on watch/spawn，不新增 ticker；kill 统一 `SignalGroup(pgid)` | §3.7 PoC 实测 |
| watch 注意力 | P0/P1 展开、P2 折叠；纯 pull、零副作用、不自动处置；botID 只取闭包 | §3.7 PoC 实测 |
| lost 认领 | 容器内 idle TTL 自杀；宿主回 `lost` + `reason` + `lastSeq`，agent 重开 | #9 / §3.7 |
| toolperm | `spawn/send/signal/close/close_all` = `RiskSensitive`（`sandbox_` 前缀兜底，**零登记**）；`poll/watch` = `RiskBasic`（**`sensitivePrefixExceptions` + `basicTools` 两处都要改**） | §9 / `toolperm/risk.go:241,255-268,59-131` |
| **工具注册全名**（v3.7） | `sandbox_session_{spawn,poll,send,signal,close,watch}` + `sandbox_session_close_all`；**禁止裸 `spawn`**（撞 `subagent/tools.go:90` 被静态去重吞掉） | §4 / `agent/tools/registry.go:151-153` |
| **清单同步点**（v3.7） | 新增工具须同步 7 处（含 `tools_test.go:285` 计数硬断言、`WorkspaceToolNames()`、`fallbackToolList()`、`profile.go` 档位） | §4 |
| **配置化**（v3.7） | 全部阈值走 `keys.go → module.go(元数据/DefaultMap) → botservice.go(两条构造路径) → sandbox.Config/DefaultConfig/fillDefaults` 五处链路；⚠️ 只改 `DefaultMap()` 不生效 | §8 / `config/store.go:20-35` |
| **TTL 执行位置**（v3.7） | 进程级 TTL 由 readLoop 顺带比对 `lastTouch`；lazy GC 只清注册表；abort **不**自动 kill；`StopBot`/`Bot.Close` 同步 `closeAll(botID)` | §3.7 / §12 / #7 |
| **双水位线**（v3.7） | `Seq` = 原始字节（仅 raw 回放）；`SettledSeq` = 语义已结算位置，`sinceSeq` / watch cursor 一律取它 | §5 / §4 |
| **SubAgent 边界**（v3.7） | 首版建议对 `IsSubagent=true` 屏蔽全组会话工具（动态 provider 默认会继承），须显式断言 | §4 |
| **基线**（v3.7） | Linux 实例上 `go test -count=1 ./sandbox/...` 全绿后才开工；本机 darwin 基线 8 fail | §2 / §11 |

⚠️ 全部默认值只影响实现细节，不改主线（启动即建会话 / 能力常供 / 决策外移 / 进程唯一所有权）；如需推翻任一项只回退该项，其余不受影响。

⚠️ **定稿 ≠ 已验证**：以下四项仍是硬缺口，实施时不得跳过 —— ① `sandbox.backend` 未用运行中实例复核（§2）；② channel 真实容器 daemon / PTY 帧未跑（§3.6 / §11 上线前三步）；③ 本机 darwin 的 `reaper_other.go` 让 channel `Exec` 不可用，进程级联与 subreaper 行为**只能在 Linux 实例上验收**（§3.7 的 PoC 是 local PTY 对照组）；④ **本机 `go test -count=1 ./sandbox/...` 基线是红的**（8 个 darwin 环境限制失败，§2 / §11），改动前须在 Linux 实例拿到绿灯存档。

---

## 14. 风险章节：照这份方案实现，最可能错在哪（v3.7 六层复核）

> skill 的最后一问。按「出错概率 × 静默程度」排序；**前 4 条都是不会报错、只会「功能莫名不生效」的类型**。

| # | 最可能出错的地方 | 现象（为什么难查） | 防线 |
|---|---|---|---|
| 1 | **工具注册名撞车 / 清单漏同步** | 用了裸 `spawn` → `registry.go:151-153` 静态优先去重，工具**静默不出现在列表里**；漏改 `WorkspaceToolNames()` / `fallbackToolList()` / `profile.go` → 节点档位或权限页里选不到，日志毫无线索（`tools.go:128-141` 的注释本身就是这个坑的记录，而 `fallbackToolList()` 现在已漏 `run_code`） | §4 全名表 + 7 处清单；§11 ㉒ 断言 `ResolveTools` 真的返回 6 个全名 |
| 2 | **把后台会话挂到 message ctx** | 照抄 `runCommandWithStreaming` 的 `exec.CommandContext` 写法 → agent **正常回完一句话**就把 CLI 杀了（`bot.go:764-773` 每轮结束都 cancel），表现为「会话活不过一轮」 | §3.7 / §8 写明第三个不能复用的理由；§11 ㉖ 正常收尾与 abort 后都要断言仍 `running` |
| 3 | **回收只靠 lazy GC** | abort 后无人 `watch`/`spawn` → 15min TTL 永不触发，local 后端留孤儿；因为「没报错、进程还活着」，极易到线上才发现 | §3.7 进程级 TTL 挪到 readLoop；§11 ㉗ 专门验「全程不再调 watch 也能回收」 |
| 4 | **配置只改 `DefaultMap()`** | 面板上值变了、运行时纹丝不动 —— `config/store.go:20-35` 明确默认值由调用方提供，`DefaultMap()` 只喂 `GET /api/config` 展示；`sandbox.image` 展示/运行时两处默认值就长期不一致（11-12 已把展示侧对齐为 `builtin`，**链路风险本身不变**） | §8 五处链路；§11 ㉜ 改配置后行为必须真的变 |
| 5 | **`seq` 与 `settledSeq` 混用** | 稳定窗口未结算时 `poll(sinceSeq=seq)` **丢一段语义输出**，且不会重复出现，agent 表现为「偶尔没看到结果」 | §4 / §5 双水位线；§11 ㉙ 窗口内高频 poll 逐 marker 核对 |
| 6 | **有状态解析不幂等** | 增量 UTF-8 decoder / `pendingEscape` 在前端接管重放 raw 时产生与首次不同的屏幕；幂等测试若用纯 ASCII 会退化成无操作、测不出来 | §5 用真实 62-chunk ANSI 洪水做 `render(raw)` 确定性与任意分块一致性测试 |
| 7 | **health 阈值当通用真理** | `seqRate>20` / `changedPerChunk>=1.5` / `screenStableMs>3000` 全部从**一个**真实 CLI（CodeBuddy）的 8 个构造样本 + 一轮回放调出来；换 `vim` / `htop` / `git add -p` 大概率漂移。构造样本 8/8 只证明「机制在这套样本上自洽」，不证明泛化 | 阈值全部配置化（§8）；每个新 CLI 跑**黄金集回归**（固定 case 全量重放比通过率），不做小样本统计检验（规模不同类，第 3 层）；`health` 只诊断、零自动处置（#8），误判代价可控 |
| 8 | **`send` 的 `submit=true` 经验值** | 「写文本 → 100ms → Enter」只在真实 CLI 上验过一次（§3.4 已注明「该 CLI 未表现 bracketed paste 问题」）；对开启 bracketed paste 的 TUI，紧连输入会被吞或整段当粘贴 | 提示词强制「发完必须回到 `poll` 读屏」；bracketed paste 敏感 CLI 单独验收，必要时改发 `\r` 或显式 paste 序列；§3.4 的「禁止固定 sleep 猜结果」针对的是**等结果**，与这里的**写入节奏**不是一回事，别混 |
| 9 | **误以为 `RiskBasic` 的 `poll`/`watch` 可以绕过 `send` 裁决** | toolperm 只按工具名静态裁决，拿不到屏幕内容；若有人按 v3.6 原文去找 `allowUnsafe` 开关，会找不到（全仓零匹配） | §3.4 / §9 已重写；确认屏风险靠「`send` 整体敏感 + 提示词」兜，参数级裁决列 P3 |
| 10 | **在红基线上判回归** | 本机 darwin 8 个 fail 与方案无关，若拿它当「改动前基线」，之后任何测试失败都会被误归因 | §2 / §11 基线纪律：Linux 实例绿灯存档后再开工；真实 LLM 集成测试单独复跑（项目已踩 flaky） |
| 11 | **Web 控制台拿 `poll(mode=raw)` 当字节流** | `Poll` 会把 `sinceSeq` 夹到 `settledSeq`、registry cursor 也存 `SettledSeq` → 每帧重复回放未结算字节，表现为「屏幕内容自己长出来」；同时人类的 `poll` 会**吃掉 agent 的语义 cursor**，两边互相干扰 | 11-12 新增独立 `RawSince`（`Seq` 水位线 + `Resync`），§10.1 Step 2 明令只走它；单测断言 agent delta 不被消费、游标不推进 |
| 12 | **resize 只发给了子进程** | `pty.Setsize` / channel resize 帧改的是孩子看到的窗口，daemon 的 VT 网格仍是 spawn 尺寸 → 人一拉窗口，agent `poll` 的 `Screen` 与真实终端换行位置不一致，且 `view()` 默认行数写死配置值，越看越短 | 11-12 新增 `resizeScreen()` 幂等重建 + 两个后端接线 + `view()` 用实时 `s.rows`；§10.1 验收 4 用 `tput cols` 对照 |
| 13 | **会话列表复用 `watch` / WS 断开当作关闭** | `Watch` 顺带跑 `GC()`，刚退出的会话可能在人打开回放前被清掉；WS 用 `c.Request.Context()` 若被接到 message ctx 或 abort 链，关标签页甚至「agent 回完一句话」都会杀会话 | 11-12 新增纯只读 `List`（不 GC）；§10.1 Step 2 规定 WS 只 attach/detach、断开绝不 `reg.Close`，关闭必须显式帧 |
| 14 | **新端点路径撞已有接口 / `Resync` 用了 sticky 标志** | `/api/bots/:id/sessions` 早已是聊天会话 CRUD，照文档原稿注册会吞掉旧接口；`ringBuffer.truncated` 一旦溢出恒为 true，拿它当 `Resync` 会让控制台每 50ms 清屏一次，**两种都是不报错、只功能莫名不对** | 实施时用 Grep 复核路由 → 全程改 `pty-sessions` 命名空间；`Since` 改按 cursor 判 gap 并补回归测试；§10.1 实施记录留档 |
| 15 | **浏览器 resize 反馈环 +「注释里的防线」并不存在** | 实机验收日志 4434 条 `pty_session_ws_resize`、rows 31→3286：`.sc-body` 高度 auto + `.xterm{height:100%}` 让 `fit()` 撑高容器 → `ResizeObserver` 再触发。更隐蔽的是 `resizeScreen` 只钳了 daemon 网格，注释却写「PTY 由调用方同值限制」，而**没有任何调用方限制** → `pty.Setsize` 收到 3286 行 | 前端固定容器高度 + 变化去重；服务端 `ClampWinsize` 统一钳制 spawn/resize、WS 去重 no-op 帧；补 4 个 sandbox 单测 + 1 个 WS 去重/钳制单测；重建后实机复验 4434 → 1 帧。**教训同 #1：注释声称的机制必须找到真实调用点才算数** |
| 16 | **health 拿整屏判提示，历史确认屏永久污染 verdict** | 真实 ssh 登录 8 个阶段全部判成 `awaiting_action`：首屏 `Are you sure...` 留在 40 行网格里，之后密码提示、远端 shell、命令输出、Ctrl+C 后全被它压住。不报错、只是 `watch` 永远报 P0 假警 | 三窗口（8 行上下文 / 3 行失败窗口 / **最后一行 = 当前提示**）+ `shellPromptPattern` 让「当前行是 shell 提示符」压过历史 actionable；11 个脱敏真实屏幕形态做黄金集回归（§0.2） |
| 17 | **`secret=true` 是凭空造的参数；echo-on 凭据沿 PTY 四处扩散** | 文档初稿的 hint 指向一个仓库里不存在的参数；真正的风险是 `read -p` / sudo 回显 / PIN 对话框把输入回显进 master，明文进 ring → `Screen`/`Delta`/`URLs`/`mode=raw`/WS 全量重放/工具调用历史，而全仓对这些路径**零 masking** | 复用 `llm/orchestrate.go:1582-1598` 已有的 `{{secret:name}}` 咽喉点，**不新增参数**；入参拒绝未解析占位符 + `SecretStream` 入站跨 chunk 遮蔽 + `redactSessionView`/`redactRawChunk`/WS 每连接流式遮蔽出站兜底（§0.2）。⚠️ 值长度 < `MinMaskableSecretLen`(6) 时 vault 明确 WARN 且遮蔽层覆盖不到，这是既有边界 |
| 18 | **验收脚本用文本 needle 等结果** | 「屏幕出现 `GOT:`」会命中被回显的**命令行** `echo GOT:$x` → 等待立即返回、紧接着的 Ctrl+C 打断还没读到输入的 `read`，现象酷似「遮蔽层吞字节」。曾据此差点误改正确实现 | 等待一律用 `seq` 水位线等新字节，needle 只用于「当前最后一行是不是某个提示」；先本地真 PTY 复现 + 真实 SSH A/B 排除被测代码，再改 harness（§0.2 末段） |
| 19 | **「模型可见工具名」≠「注册全名」** | `llm/orchestrate.go:1139-1165` 的 `stripSandboxPrefixes` 在工具 schema 送进模型前剥掉 `sandbox_`：`sandbox_session_spawn`→`session_spawn`、`sandbox_exec`→`exec`。但提示词、工具描述、auto 升级 hint、错误文案原先全写全名 → 模型照文案调必然 `tool not found`；最坑的是 `background=auto` 失败后返回 `retry with sandbox_session_spawn`，指向一个模型根本调不到的名字。生产日志里 `tool: exec` 成功 N 次正是剥前缀生效的证据，**别据此把 skill 里的短名判成写错** | 注册表 / toolperm 规则 / 管理界面保留全名（裁决发生在 `ResolveTools`，早于剥前缀），只把**面向 LLM 的字符串**改成短名；`sandbox/tool_visible_names_test.go` 用 AST 扫本包所有字符串字面量，禁止把带前缀全名嵌进任何句子，并反向断言短名确实出现在 prompt 与 exec schema 里。`tools.go` 里自相矛盾的「not permitted… use spawn directly」改为 `retry with background=sync` |
| 20 | **宿主部署了新代码 ≠ 容器 channel daemon 更新了** | 内置镜像 tag = 构建上下文 + channel 源码/字节的 md5（`docker/sandbox/build.go:107-144`），源码变了会产出新 tag；但 `ensureRunningLocked` 只按 `containerState` 分流（`sandbox/botcontainer.go:282-303`），**已 running 的容器永不因 tag 变化重建**，重启宿主服务也只是复用。表现为 toolperm 全 allow、`background=auto` 参数也在、模型真调了，却报 `channel session spawn: unsupported: unknown message` | 部署新版本后必须显式 `DELETE /api/bots/:id/container`（`KeepData=true` 保 `/data` named volume）或手工 destroy+ensure 触发 `create()` → `resolveBotImage` 拉新镜像；重建会丢容器可写层（`/tmp`、运行时装的 toolchain、桌面会话），`/data` 与 `/run/thinkbot` 保留。验证要看容器内二进制有没有 `session.spawn` 帧，不能只看宿主 `/health` 的 gitShort |

**两条不变的设计底线**（六层复核没有推翻）：① **能力常供、决策外移** —— `state=running` 永远允许 `send`，`awaiting_input` 只做 hint；② **进程唯一所有权** —— 所有 `exec.Command` / kill / reap 只经 `SessionRegistry` + `ProcessOwner`，工具不得自己按 pid 操作。v3.7 的所有修订都是为了让这两条在真实代码里**真的生效**，而不是只在文档里成立。
