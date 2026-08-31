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

thinkbot 推荐以 **DooD（Docker-out-of-Docker）** 方式部署：主容器经挂载的 `docker.sock` 指挥宿主 Docker 为各 Bot 创建兄弟容器（沙箱），因此 `sandbox.backend` 必须为 `docker`。

```bash
# 克隆仓库
git clone https://github.com/kasuganosora/thinkbot.git
cd thinkbot

# 准备配置（必做）
cp .env.example .env
# 编辑 .env：填入 LLM 供应商 API Key，确认 sandbox.backend=docker（已默认）

# 启动（后台运行，含健康检查；镜像内自动完成 Go 与前端构建）
docker compose up -d --build
```

> ⚠️ `cp .env.example .env` 不可跳过。bind mount 不会自动创建缺失的文件——宿主没有 `.env` 时 Docker 会挂进来一个空目录，容器会在启动时直接报错退出（`entrypoint.sh` 已内置该检查）。

数据落在 `./data/container`（SQLite 库与 bot 工作空间）、日志落在 `./logs/container`，均持久化到宿主目录。刻意使用 `container/` 子目录，避免容器启动时的 `chown -R` 影响宿主上裸机运行实例的数据属主。

访问 `http://localhost:8080` 打开 Web 管理界面。`.env` 中的 `sandbox.backend` 已默认 `docker`、`sandbox.image` 默认 `builtin`，与 `docker-compose.yml` 的 DooD 挂载联动；如需改用已存在的预构建镜像，改 `sandbox.image` 即可。

若需在镜像里带上精确版本号（供单实例协商与 `/health` 展示），构建时显式传入：

```bash
docker compose build \
  --build-arg BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  --build-arg GIT_REVISION=$(git rev-parse HEAD) \
  --build-arg VERSION=$(git describe --tags --always 2>/dev/null || echo dev)
docker compose up -d
```

### 不使用 Docker（裸机运行）

```bash
# 重新生成 swagger 文档（如需）后编译
go build -ldflags="-s -w" -o thinkbot ./cmd && ./thinkbot
```

> 提示：默认 `go build` 产物约 50MB；加上 `-ldflags="-s -w"` 可剥离调试符号与 DWARF 信息，体积显著减小（与 Docker 构建路径一致）。二进制需能访问宿主 `docker` 才能使用 `sandbox.backend=docker`，否则请将 `sandbox.backend` 改为 `local`。

> ⚠️ **不要设置 `CGO_ENABLED=0`**。数据库驱动 `gorm.io/driver/sqlite` 底层为 `mattn/go-sqlite3`（C 实现），`CGO_ENABLED=0` 下仍能编译通过，但驱动会被替换成 stub，运行期打开数据库时报 `go-sqlite3 requires cgo to work. This is a stub` 而无法启动。交叉编译时需配置对应平台的 C 工具链。

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
