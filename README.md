# thinkbot

基于 Go 的多渠道 AI 聊天机器人框架，支持多 LLM 供应商、多渠道接入、分层记忆系统、工具调用和沙箱工作空间。

## 核心特性

- **多 LLM 供应商**：OpenAI / Anthropic / Google Gemini / xAI Grok，统一接口
- **多渠道接入**：Misskey / Telegram / Web，统一消息归一化
- **分层记忆系统**：L0 工作记忆 → L1 长期记忆 → L2 场景记忆 → L3 用户画像，自动巩固
- **工具调用**：Function Calling，支持沙箱工作空间（Docker/本地）
- **Pipeline 架构**：可组合的 Stage 管道，中间件 + 谓词过滤
- **Token 用量管理**：月度配额（Bot/Channel/Chat 三级限额 + 超额拦截）、单次预算控制、全链路记账（SubAgent/Workflow/Memory 均不漏记）
- **主动参与**：三层漏斗决策引擎（规则 → LLM 快判 → 时序门控）
- **工作流引擎**：基于 DAG 的多步骤自动化工作流
- **技能系统**：从文件系统动态加载可扩展技能
- **MCP 集成**：支持 Model Context Protocol 工具服务器

## 快速开始

```bash
# 克隆仓库
git clone https://github.com/kasuganosora/thinkbot.git
cd thinkbot

# 配置环境变量
cp config/config.example .env
# 编辑 .env 填入 API Key 等配置

# 编译运行
go build -o thinkbot ./cmd && ./thinkbot
```

访问 `http://localhost:8080` 打开 Web 管理界面。

## 项目结构

```
thinkbot/
├── agent/          # 核心 Agent 框架（Engine + Pipeline + 记忆 + 工具）
│   ├── bot/        #   Bot 实例与管理
│   ├── core/       #   核心类型（Message/Envelope/Stage）
│   ├── engagement/ #   主动参与决策
│   ├── memory/     #   分层记忆系统
│   ├── pipeline/   #   消息处理管道
│   ├── prompt/     #   系统提示词构建
│   ├── session/    #   会话串行化
│   ├── stages/     #   内建 Stage
│   └── tools/      #   工具管理
├── api/            # HTTP API 服务（Gin）
├── auth/           # 用户认证与权限
├── channel/        # 渠道适配器（Misskey/Telegram）
├── cmd/            # 程序入口
├── config/         # 配置管理
├── dao/            # 数据访问层（GORM）
├── db/             # 数据库初始化
├── llm/            # LLM 供应商适配层
│   ├── openai/     #   OpenAI（兼容 DeepSeek 等）
│   ├── anthropic/  #   Anthropic Claude
│   ├── google/     #   Google Gemini
│   └── grok/       #   xAI Grok
├── mcp/            # MCP 协议客户端
├── sandbox/        # Bot 沙箱工作空间
├── skill/          # 技能系统
├── stats/          # 用量统计
├── subagent/       # 子代理管理
├── tools/          # 内建工具集
├── util/           # 通用工具库
└── workflow/       # 工作流引擎
```

## 技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.25 |
| Web 框架 | Gin |
| ORM | GORM + SQLite |
| 依赖注入 | go.uber.org/fx |
| 日志 | Zap + Lumberjack |
| 可观测性 | OpenTelemetry |
| 实时通信 | WebSocket / SSE |

## License

MIT
