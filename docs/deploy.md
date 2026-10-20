# 部署与运维

> 覆盖：Docker 部署细节、更新、数据落点、备份、回滚、迁移。最短上手路径见 [README 快速开始](../README.md#快速开始)。
>
> 相关文档：[配置](configuration.md) · [开发 / 裸机运行](development.md) · [架构](architecture.md)

## 部署方式：DooD（Docker-out-of-Docker）

thinkbot 推荐以 **DooD（Docker-out-of-Docker）** 方式部署：主容器经挂载的 `docker.sock` 指挥宿主 Docker 为各 Bot 创建兄弟容器（沙箱），因此 `sandbox.backend` 必须为 `docker`。

- thinkbot 容器本身只跑主程序（Go 二进制，内含 Web 控制台与 HTTP API）。
- 每个 bot 一个长期运行的兄弟容器 `thinkbot-bot-<botID>`，文件落在同名 named volume；它们由宿主 daemon 创建，不属于本 compose 项目。资源限制（`--memory` / `--cpus` / `--shm-size 512m`）由沙箱代码在创建时施加，主容器不需要 `privileged`。
- 无外部依赖：数据全部在 SQLite（容器内 `data/thinkbot.db`，WAL 模式）与 `data/workspaces`，不需要 db / redis / mq。

### 前提

1. 宿主已安装并运行 Docker daemon；
2. 运行 compose 的用户对 `docker.sock` 有读写权限；
3. 宿主 8080 端口空闲。若宿主另有裸跑的 thinkbot 实例，必须先停掉它或改 `docker-compose.yml` 的宿主端口映射（如 `"18080:8080"`），否则容器报 `bind: address already in use`。容器内的 singleinst 单实例探测只看容器自己的 localhost，发现不了宿主上的实例。

### 完整步骤

```bash
# 克隆仓库
git clone https://github.com/kasuganosora/thinkbot.git
cd thinkbot

# 准备配置（必做）
cp .env.example .env
# 编辑 .env：确认 sandbox.backend=docker（已默认）

mkdir -p data/container logs/container

# 启动（后台运行，含健康检查；镜像内自动完成 Go 与前端构建）
docker compose up -d --build
docker compose logs -f
```

> ⚠️ `cp .env.example .env` 不可跳过。bind mount 不会自动创建缺失的文件——宿主没有 `.env` 时 Docker 会挂进来一个空目录，容器会在启动时直接报错退出（`entrypoint.sh` 已内置该检查）。

> ⚠️ 首位管理员需要显式初始化：空库首次登录不会自动成为管理员。做法见 [configuration.md › 首位管理员](configuration.md#首位管理员)。

> 说明：原 README 写的是「编辑 .env 填入 LLM 供应商 API Key」。据代码核对，模型服务商与 API Key 实际在 Web 管理台「系统设置 → 模型服务」中配置（存入数据库），`.env.example` 中没有 API Key 键。

访问 `http://localhost:8080` 打开 Web 管理界面。`.env` 中的 `sandbox.backend` 已默认 `docker`、`sandbox.image` 默认 `builtin`，与 `docker-compose.yml` 的 DooD 挂载联动；如需改用已存在的预构建镜像，改 `sandbox.image` 即可。

### 带精确版本号构建

若需在镜像里带上精确版本号（供单实例协商与 `/health` 展示），构建时显式传入（不填则回退到二进制 mtime，功能不受影响）：

```bash
docker compose build \
  --build-arg BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  --build-arg GIT_REVISION=$(git rev-parse HEAD) \
  --build-arg VERSION=$(git describe --tags --always 2>/dev/null || echo dev)
docker compose up -d
```

`docker-compose.yml` 刻意不用 `${BUILD_TIME}` 之类的插值：compose 默认把项目根 `.env` 当作变量替换文件，而本项目的 `.env` 是应用配置，键名形如 `api.addr`（含点，非合法 shell 变量名），混用容易踩坑。

### 镜像变体

- 默认 `thinkbot-slim`：最小镜像，不含 loader / 源码。
- `selfhost`：`THINKBOT_TARGET=selfhost docker compose up -d --build`。在 slim 基础上额外包含 git/go/node 工具链、全量源码仓（`/app/src`，含 `.git`）与 `thinkbot-loader`，使容器具备「git pull 自部署 + 健康门控回滚」能力。还需在 `.env` 配置 `loader.enabled=true` 与 `loader.token`，否则 loader 退化为透明启动器（行为与 slim 一致）。运维接口默认监听 `127.0.0.1:8090`。详见 [loader/README.md](../loader/README.md)。

## 常用命令

| 用途 | 命令 |
|---|---|
| 更新（改代码 / 拉新代码后） | `git pull && docker compose up -d --build` |
| 停服（保留数据与日志） | `docker compose down` |
| 查看健康状态 | `docker compose ps`（healthcheck 请求容器内 `/health`） |
| 看日志 | `docker compose logs -f` |

停机时 SQLite 需要做 WAL checkpoint 并释放 bot 沙箱容器，compose 已把宽限期放宽到 `stop_grace_period: 45s`。

## 数据落点

| 宿主路径 | 容器路径 | 内容 |
|---|---|---|
| `./data/container` | `/app/data` | SQLite 库（`thinkbot.db` 及 `-wal` / `-shm`）、bot 工作空间（`workspaces/`）、自部署日志（`loader/`，仅 selfhost） |
| `./logs/container` | `/app/logs` | `thinkbot.log`（JSONL）与 `thinkbot.console.log`（可读文本），均由 lumberjack 轮转 |
| `./.env` | `/app/.env`（只读） | 应用配置（可用环境变量 `CONFIG_FILE` 覆盖路径） |
| `./skills` | `/app/skills`（只读） | 文件系统技能（`skills/<技能名>/SKILL.md`），目录为空属正常 |
| named volume `thinkbot-bot-<botID>` | 沙箱容器内工作目录 | 每个 bot 沙箱里的文件 |

刻意挂 `container/` 子目录而不是 `./data` / `./logs` 根：`entrypoint.sh` 启动时会 `chown -R thinkbot:thinkbot`（uid 1000），直接挂根会改掉宿主上裸跑实例的数据属主。容器与宿主裸跑实例的数据彼此独立。

前端静态资源已打进镜像 `/app/static`。若要不重建镜像热替换前端：先 `cd web && npm run build`（产物输出到项目根 `static/`），再取消 `docker-compose.yml` 里 `./static:/app/static:ro` 一行的注释。

## 备份

需要备份的东西：

1. `./data/container/`——**最重要**：数据库（用户、bot、模型服务商、平台凭据、记忆、聊天记录、通知令牌哈希等）和 bot 工作空间。删掉这个挂载等于删库。
2. `./.env`——应用配置。
3. 各 bot 的沙箱 named volume `thinkbot-bot-<botID>`（`docker volume ls | grep thinkbot-bot-` 查看）。
4. 若接了告警通知：宿主 `/etc/thinkbot/`（`notify.token` 等，见 [notify.md](notify.md)）。

SQLite 是 WAL 模式，最稳妥的冷备做法是先停服再拷贝：

```bash
docker compose down
tar czf thinkbot-backup-$(date +%Y%m%d).tgz data/container .env
docker compose up -d
```

> 不停服直接拷 `thinkbot.db` 可能拿到不一致的快照（`-wal` 中尚未 checkpoint 的数据）。上面的命令是通用做法，不是项目内置脚本。

沙箱容器还可在 Web 管理台「Bot 设置 → 容器」中做快照、导出与导入（对应 API `POST /api/bots/:id/container/snapshots`、`/export`、`/import`、`/restore`）。

## 回滚

- **slim 镜像（默认）**：回到旧版本代码后重新构建，例如 `git checkout <旧提交或标签> && docker compose up -d --build`。数据库迁移由启动时 `dao.Migrate` 自动执行，回滚前建议先按上文备份 `data/container`。
- **selfhost 镜像 + loader**：
  - 自部署时旧二进制常态保留为 `thinkbot.prev`，新二进制先在隔离端口（`loader.smoke_port`，默认 `:18080`）验证，验证不过旧服务不中断；
  - 手动回退：`POST /loader/rollback`（需 `X-Loader-Token`），回到上一良版本并重启；
  - 自动回退：`loader.crashloop_window`（默认 60s）内连续崩溃超过 `loader.crashloop_max`（默认 5）次，loader 自动把 `thinkbot.prev` 复制回来启动；
  - 每次部署的日志落在 `data/loader/deploy-<id>.json`，审计在 `data/loader/deploy-history.jsonl`。

  详见 [loader/README.md](../loader/README.md)。

## 迁移到新机器

1. 旧机器停服：`docker compose down`。
2. 拷贝 `data/container/`、`.env`（以及需要的话 `skills/`、宿主 `/etc/thinkbot/`）到新机器的仓库目录；保持文件属主可由容器 `chown`（entrypoint 会自动处理）。
3. 沙箱文件：在 Web 管理台「Bot 设置 → 容器」导出 / 导入，或用 Docker 自带方式迁移 named volume `thinkbot-bot-<botID>`。
4. 新机器上 `docker compose up -d --build`。

从 Memoh 迁移记忆：Web 管理台「Bot 设置 → 记忆 → 导入 Memoh 备份」（`POST /api/bots/:id/memory/import`，读取工作空间 tar/tar.gz 中的 `memory/YYYY-MM-DD.md` 与 `PROFILES.md`），见 [agent/memory/README.md](../agent/memory/README.md)。

## 网络与安全提示

- 只在本机访问时，可在 `.env` 设 `api.addr=127.0.0.1:8080`（容器内仍需监听容器端口，Docker 部署更常见的做法是把端口映射改成 `"127.0.0.1:8080:8080"`）。
- 公网访问请放在反向代理后，并在 `.env` 设置 `api.cookie_secure=true`、`api.cors_origins`。
- 告警通知接口建议只挂在独立的本机监听器上，见 [notify.md › Network restriction](notify.md#network-restriction)。
