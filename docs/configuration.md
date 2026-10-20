# 配置

> thinkbot 的配置分三处：**`.env` 文件**（启动级配置）、**进程环境变量**（少量启动开关），以及 **Web 管理台**（模型、Bot、平台等日常配置，存入数据库）。
>
> 相关文档：[部署与运维](deploy.md) · [外部通知](notify.md) · [记忆系统](memory.md) · [架构](architecture.md)

## 配置从哪里来

- 配置优先级：`overrides > 数据库 > .env 文件 > 进程环境变量`（见 [loader/README.md](../loader/README.md)）。注意 `.env` 会压过同名的进程环境变量。
- `.env` 默认读取工作目录下的 `.env`（容器内 `/app/.env`），可用环境变量 `CONFIG_FILE` 覆盖路径。键名大小写不敏感（统一归一为小写）。
- 管理员可在 Web 管理台「系统设置 → 系统配置」查看与修改全部已登记的配置键及说明（键的登记表在 `config/module.go`）。

## `.env`（来自 `.env.example`）

| 键 | 默认 / 示例 | 说明 |
|---|---|---|
| `api.addr` | `:8080` | 应用监听地址；为空或 `0.0.0.0` 时对外暴露，仅本机访问可设 `127.0.0.1:8080`。容器端口需与 `docker-compose.yml` 的端口映射一致 |
| `api.cors_origins` | 空 | 允许跨域的来源（逗号分隔）；留空仅允许 localhost（开发模式） |
| `api.cookie_secure` | — | Cookie 是否仅经 HTTPS 传输（生产建议 `true`） |
| `db.path` | `data/thinkbot.db` | SQLite 文件路径（落在持久化 data 卷） |
| `log.level` | `info` | `debug` / `info` / `warn` / `error` |
| `sandbox.backend` | `docker` | 沙箱后端；DooD 部署必须为 `docker`（主程序经挂载的 docker.sock 为 bot 创建兄弟容器）。另有 `auto`（Docker 可用则用，否则降级 local）与 `local`（无容器隔离） |
| `sandbox.image` | `builtin` | per-bot 长期容器镜像。`builtin` = 内置浏览器沙箱镜像（含 chromium + patchright），由 thinkbot 按需自行构建，构建上下文已打进二进制，无需手动 `docker build`；填其他镜像名（如 `alpine:latest`）则原样使用。留空等同 `builtin` |
| `sandbox.require_docker` | `true` | `auto` 模式下强制要求 Docker 可用，避免探测失败静默降级到 local 裸跑（显式 `backend=docker` 时无效，docker 不可用会直接报错） |
| `workspace.dir` | `data/workspaces` | bot 工作空间根目录（SOUL.md 从 `{dir}/{botID}/` 读取） |
| `system.timezone` | `Asia/Shanghai` | 影响 bot 的时间感知与沙箱容器 `TZ`；请与 `docker-compose.yml` 中的 `TZ` 保持一致 |
| `loader.*` | 默认关闭 | 自举 loader（仅 selfhost 镜像），见下文 |

### 自举 Loader（`loader.*`，仅 selfhost 镜像，默认关闭）

镜像构建：`THINKBOT_TARGET=selfhost docker compose up -d --build`。全部留空 / 注释时 loader 退化为透传，零影响。

| 键 | 说明 |
|---|---|
| `loader.enabled=true` | 总开关：`true` 进入监管 + 自部署模式；`false` 透传 |
| `loader.ops_addr=127.0.0.1:8090` | 运维 HTTP 监听（默认仅本机；远程暴露需自担风险） |
| `loader.token=` | **必填**：变更类接口 Bearer Token；开启但为空则 loader 拒绝启动 |
| `loader.health_addr=` | 子进程 `/health` URL；留空由 `api.addr` 自动推导 |
| `loader.health_timeout=30s` | smoke test 健康判定截止 |
| `loader.smoke_port=:18080` | 隔离 smoke test 端口（新二进制在独立端口验证，旧服务零中断） |
| `loader.build_timeout=600s` | 单次自部署总超时 |
| `loader.git_dir=/app/src` | 自部署源码目录（selfhost 镜像内为全量仓） |
| `loader.crashloop_max=5` | 崩溃环阈值：窗口内连续崩溃超此数 → 自动回退上一良版本 |
| `loader.crashloop_window=60s` | 崩溃环统计窗口 |

bot 用于自部署的 `tb_*` 工具默认全部禁止，需在「工具权限」中显式放行，见 [loader/README.md](../loader/README.md)。

## 进程环境变量

这些变量由代码直接从进程环境读取（不是 `.env` 键）。Docker 部署时通过 compose 的 `environment:` 传入，例如写在 `docker-compose.override.yml` 里。

| 变量 | 说明 |
|---|---|
| `AUTH_BOOTSTRAP_ADMIN` / `AUTH_BOOTSTRAP_PASSWORD` | 启动时若库中还没有管理员，用这对账号密码创建首位管理员（密码至少 6 位） |
| `AUTH_BOOTSTRAP_TOKEN` | 空库首登自举令牌：空库首次登录时，以该令牌作为密码登录即创建首位管理员 |
| `DB_PATH` | SQLite 路径（默认 `data/thinkbot.db`），映射为 `db.path`；`notify-token` CLI 也用它 |
| `LOG_LEVEL` | 日志级别（默认 `info`），映射为 `log.level` |
| `CONFIG_FILE` | 覆盖 `.env` 路径 |
| `TZ` | 容器时区，compose 默认 `Asia/Shanghai` |
| `THINKBOT_TARGET` | 构建期变量：`selfhost` 选择自托管镜像（默认 `thinkbot-slim`） |
| `THINKBOT_DOCKER_BIN_DIR` | 显式指定 docker 可执行文件所在目录（裸机运行时 PATH 被裁剪的情况，见 [sandbox/README.md](../sandbox/README.md)） |

### 首位管理员

空库首次登录**不会**自动成为管理员（防止任何能访问端口的人接管实例）。二选一：

- **推荐**：设置 `AUTH_BOOTSTRAP_ADMIN` + `AUTH_BOOTSTRAP_PASSWORD`，启动时自动创建；
- 或设置 `AUTH_BOOTSTRAP_TOKEN`，然后在登录页用任意用户名 + 该令牌作为密码完成首次登录。

都没设置时，空库登录会返回 403（`bootstrap disabled`）。

Docker 部署的写法（compose 会自动合并同目录的 `docker-compose.override.yml`）：

```yaml
# docker-compose.override.yml
services:
  thinkbot:
    environment:
      AUTH_BOOTSTRAP_ADMIN: admin
      AUTH_BOOTSTRAP_PASSWORD: change-me-please
```

管理员创建后，变量就不再起作用（只在「还没有管理员」时生效）。建议登录后在「用户设置 → 安全设置」改密码，再删掉 override 中的这两行并 `docker compose up -d`。

## 在 Web 管理台配置（存入数据库）

| 要做的事 | 位置 |
|---|---|
| 添加模型服务商、API Key、模型 | 系统设置 → 模型服务（客户端类型：OpenAI Compatible / OpenAI Responses / Anthropic / Gemini） |
| 配置联网搜索后端 | 系统设置 → 搜索 |
| 新建 Bot、选择主模型 / 轻量模型 | 系统设置 → Bots → 新建 Bot；Bot 设置 → 通用 |
| 接入 Telegram / Misskey | Bot 设置 → 平台（Telegram 需 Bot Token，从 @BotFather 获取；Misskey 需实例 URL + API Token，可选订阅时间线） |
| 人格（SOUL.md / system prompt） | Bot 设置 → 人格 |
| 把你的 Telegram / Misskey 账号绑定到 thinkbot 账号 | 用户设置 → 生成授权码，把 `TB-XXXX-XXXX` 发给 bot（5 分钟有效，见 [identity/README.md](../identity/README.md)） |
| 工具权限 | Bot 设置 → 工具权限（规则说明见下） |
| 记忆、梦境整理 | Bot 设置 → 记忆 / 梦境（见 [memory.md](memory.md)） |
| 定时任务、心跳、主动开口 | Bot 设置 → 定时任务 / 心跳 / 主动开口 |
| MCP、技能、浏览器 Cookie | Bot 设置 → MCP / 技能 / 浏览器 |
| 额度 | Bot 设置 → 额度；系统设置 → 计费看板 / 统计 |

绑定账号是很多功能的前提：例如告警通知会发给「最早的、处于启用状态的管理员」在 Telegram 上绑定的账号，`/chatid` 等命令也要求已绑定。

### 工具授权（toolperm）

`toolperm` 按 bot × 工具 × 平台 × 会话 × 用户 维度配置 allow/deny。判定顺序：首条匹配规则生效 > 平台无规则时按风险分级取默认（基础/对外发言工具放行，敏感工具禁止，exfil 外发通道即使在此分支也默认禁止）> 平台已有规则但未命中时白名单模式（仅基础工具放行）。工具列表过滤 + 执行时复核双重防线。

例如 `telegram_send_document` / `telegram_send_photo` 属 exfil 级，默认拒绝，需显式 allow。

## 告警通知（notify）快速接入

完整说明见 [notify.md](notify.md)。Docker 部署下的最短路径：

1. `.env` 加一行，让通知接口只在独立监听器上提供：
   ```
   notify.listen_addr=0.0.0.0:8091
   ```
2. 把该端口只发布到宿主回环地址（写在 `docker-compose.override.yml` 的 `ports:` 下）：
   ```yaml
   services:
     thinkbot:
       ports:
         - "127.0.0.1:8091:8091"
   ```
   然后 `docker compose up -d`（`notify.listen_addr` 需要重启生效）。
3. 在 Telegram 里给 bot 绑定你的管理员账号（见上文「绑定账号」）。
4. 创建通知令牌并保存到宿主（`<botID>` 换成你的 bot ID）：
   ```bash
   install -d -m 0700 -o root -g root /etc/thinkbot
   docker exec -u thinkbot -w /app thinkbot /app/thinkbot notify-token create \
     --bot <botID> --name server-hooks \
     | install -m 0600 -o root -g root /dev/stdin /etc/thinkbot/notify.token
   echo <botID> > /etc/thinkbot/notify.bot
   ```
5. 安装发送脚本并试一下：
   ```bash
   install -o root -g root -m 0755 scripts/notify/thinkbot-notify /usr/local/sbin/thinkbot-notify
   thinkbot-notify -s server-1/test -l info -t "notify test" -b "hello"
   ```
   mdadm / smartd 钩子（`thinkbot-mdadm-hook`、`60thinkbot-notify`）的安装见 [notify.md](notify.md) 的 Hook scripts 一节。

notify 相关的全部 `notify.*` 键与默认值见 [notify.md › Configuration](notify.md#configuration)。
