# 架构与功能详解

> 本文档收录原 README 中的技术细节（完整特性清单、项目结构、技术栈）。上手请先看 [README](../README.md)。
>
> 相关文档：[配置](configuration.md) · [部署与运维](deploy.md) · [开发](development.md) · [记忆系统](memory.md) · [外部通知](notify.md)

## 定位

> 给每个渠道背后一个能长期在线的 AI 伙伴。开源。  
> 会聊天、会记事、会动手，合上电脑它也不歇着。

thinkbot 是一个开源的多渠道 AI 机器人。你可以把它接进 Web、Telegram、Misskey，用自己习惯的模型来跑；它带着分层长期记忆、工具与技能、独立沙箱工作区，还能按工作流把多步事情串起来。

跟它在不同地方聊，上下文还能接上。它会自己醒来看看有没有该做的事，夜里整理白天攒下的记忆，出了告警也能用自己的语气告诉你。一个人用、挂在群里用都行——设计目标就是挂着长期跑，而不是聊两句就散的临时窗口。

## 核心特性

- **多 LLM 供应商**：OpenAI / Anthropic / Google Gemini / xAI Grok，统一接口
- **多渠道接入**：Misskey / Telegram / Web，统一消息归一化；Telegram 渠道工具覆盖发帖、置顶、群管理等，并支持发图 / 发文件（`telegram_send_photo` / `telegram_send_document`）
- **分层记忆系统**：L0 工作记忆 → L1 长期记忆 → L3 画像（用户画像 + Bot 自我画像），L0 自动巩固到 L1；L2 场景记忆为可选扩展点（`memory.Aggregator` 接口已定义，仓库未提供实现，生产管线不经过）
- **工具调用**：Function Calling，支持沙箱工作空间（Docker/本地）
- **工具授权**：`toolperm` 按 bot × 工具 × 平台 × 会话 × 用户 维度配置 allow/deny。判定顺序：首条匹配规则生效 > 平台无规则时按风险分级取默认（基础/对外发言工具放行，敏感工具禁止，exfil 外发通道即使在此分支也默认禁止）> 平台已有规则但未命中时白名单模式（仅基础工具放行）。工具列表过滤 + 执行时复核双重防线
- **Pipeline 架构**：可组合的 Stage 管道，中间件 + 谓词过滤
- **Token 用量管理**：月度配额（Bot/Channel/Chat 三级限额 + 超额拦截）、单次预算控制、全链路记账（SubAgent/Workflow/Memory 均不漏记）
- **主动参与**：三层漏斗决策引擎（规则 → LLM 快判 → 时序门控）
- **自主心跳**：per-bot 周期唤醒并走完整编排链路，准入关卡 + 发言闸门 + 频控三级节制
- **外部通知（notify）**：mdadm / smartd / cron 等本机程序经 `POST /api/notify`（请求体 `bot` 指定经哪个 bot；推荐只挂在独立本地监听器 `notify.listen_addr`，如宿主 `127.0.0.1:8091`）让 bot 把告警发到主人私聊。token 只存哈希、带 bot 作用域（bot 列表或全部）；默认 bot 模式由 bot 以自身人格（SOUL.md + system_prompt，不召回长期记忆，少量近期对话只定语气）用自己的话转述，标识符须逐字照抄、不揣测原因（无工具、通知作为不可信数据；warn/critical 附原文块，info 若有标识符未逐字出现也附原文块；失败回落原文），也可 raw 原样转发；绕过 reply_control 等闸门，按 bot 去重与限流，写审计与主人会话历史。见 [docs/notify.md](notify.md)
- **工作流引擎**：基于 DAG 的多步骤自动化工作流
- **技能系统**：从文件系统动态加载可扩展技能
- **MCP 集成**：支持 Model Context Protocol 工具服务器
- **Web 控制台**：聊天界面 + 管理台（`web/`，Vue 3 + Vite + TDesign，构建产物输出到 `static/` 由后端托管）

### 补充说明（重写 README 时据代码核对）

- Web 管理台中「模型服务」可选的客户端类型为 `OpenAI Compatible` / `OpenAI Responses` / `Anthropic` / `Gemini`（`web/src/components/ProvidersManager.vue`）；`llm/grok/` 下另有 xAI Grok 适配包。
- 渠道注册表（`api/channel_registry.go`）目前登记 Telegram 与 Misskey；Web 聊天为内置渠道。Mastodon 适配器在 `feat/mastodon-channel` 分支开发中，尚未合入 master。
- 外部通知（notify）目前只支持投递到 Telegram 渠道。
- 每个 bot 的沙箱是一个长期运行的兄弟容器 `thinkbot-bot-<botID>`，挂载同名 named volume；容器默认联网，`NetworkDisabled=true` 时才 `--network none`（见 `sandbox/README.md`）。
- 所有 bot 共用一个 SQLite 数据库（默认 `data/thinkbot.db`），记忆按 bot / 会话 / 用户作用域隔离。

## 项目结构

```
thinkbot/
├── agent/          # 核心 Agent 框架（Engine + Pipeline + 记忆 + 工具）
│   ├── bot/        #   Bot 实例与管理
│   ├── command/    #   斜杠命令拦截 Stage（LLM 之前短路执行）
│   ├── core/       #   核心类型（Message/Envelope/Stage）
│   ├── engagement/ #   主动参与决策
│   ├── heartbeat/  #   per-bot 自主心跳唤醒（准入 + 闸门 + 频控）
│   ├── inbound/    #   统一消息入口网关（Ingress）
│   ├── memory/     #   分层记忆系统
│   ├── outbound/   #   Action 派发 + EventBus（Web SSE）
│   ├── pipeline/   #   消息处理管道
│   ├── prompt/     #   系统提示词构建
│   ├── session/    #   会话串行化
│   ├── stages/     #   内建 Stage
│   ├── storage/    #   记忆持久化仓储（SQLite/GORM 适配器）
│   └── tools/      #   工具管理
├── api/            # HTTP API 服务（Gin）
├── auth/           # 用户认证与权限
├── channel/        # 渠道适配器（Misskey/Telegram）
├── cmd/            # 程序入口
├── config/         # 配置管理
├── cron/           # 定时任务调度
├── dao/            # 数据访问层（GORM）
├── db/             # 数据库初始化
├── docker/         # entrypoint.sh 与内置浏览器沙箱镜像构建上下文（docker/sandbox 为 Go 包，go:embed 后按需构建）
├── docs/           # Swagger 文档（swaggo 生成，供 /swagger 路由）+ 使用文档（configuration / deploy / development / memory / notify 等）+ 设计文档（heartbeat-redesign 等）
├── filesystem/     # 空占位目录（仅 .gitkeep，暂无代码）
├── identity/       # 平台账号 ↔ thinkbot 账号绑定
├── internal/       # 内部支撑包（仅模块内可见，不构成对外 API）
│   ├── buildinfo/  #   构建信息（ldflags 注入，/health 回报）
│   ├── interaction/ #  跨平台「提问—等待应答」注册表（user_choice 核心）
│   ├── searchproviders/ # 搜索提供方配置 + 12 家后端 + 回退熔断
│   └── singleinst/ #   启动早期单实例版本协商
├── llm/            # LLM 供应商适配层
│   ├── openai/     #   OpenAI（兼容 DeepSeek 等）
│   ├── anthropic/  #   Anthropic Claude
│   ├── google/     #   Google Gemini
│   └── grok/       #   xAI Grok
├── mcp/            # MCP 协议客户端
├── notify/         # 外部程序 → bot → 主人通知（POST /api/notify，bot 作用域 token / bot 模式 / 去重 / 限流 / 审计，见 docs/notify.md）
├── plugin/         # 空占位目录（仅 .gitkeep，暂无代码）
├── sandbox/        # Bot 沙箱工作空间
├── scripts/        # 辅助脚本（redeploy.sh 重部署、run_thinkbot.py 守护启动、watch.sh 日志观察）
├── skill/          # 技能系统（加载 / 注册 / 发现）
├── skills/         # 技能内容目录（每个子目录一个技能，SKILL.md 为核心）
├── static/         # 前端构建产物（npm run build 生成，勿手工编辑）
├── stats/          # 用量统计
├── subagent/       # 子代理管理
├── toolperm/       # Bot 工具权限（风险分级默认策略 + 发言模式）
├── tools/          # 内建工具集
├── util/           # 通用工具库
├── web/            # Web 前端源码（Vue 3 + Vite + TDesign）
└── workflow/       # 工作流引擎
```

## 技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.27（`github.com/kasuganosora/thinkbot`，CGO 需开启，见 [development.md](development.md) 中的 sqlite 说明） |
| Web 框架 | Gin |
| ORM | GORM + SQLite（`gorm.io/driver/sqlite` / `mattn/go-sqlite3`） |
| API 文档 | swaggo（`/swagger`） |
| 依赖注入 | go.uber.org/fx |
| 日志 | Zap + Lumberjack |
| 可观测性 | OpenTelemetry |
| 实时通信 | WebSocket / SSE |

