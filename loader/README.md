# thinkbot-loader 自举外壳

`thinkbot-loader` 是一个**独立于主程序**的 Go 二进制，作为容器的 PID1 监管 thinkbot：
拉起/守护子进程、暴露运维 HTTP 接口、并在 `.env` 开启 `loader.enabled` 时支持从 git
自行拉代码重新编译二进制（健康门控回滚）。

## 两种模式（由 `.env` 的 `loader.enabled` 控制）

- **`false`（默认）**：退化为透明启动器，`syscall.Exec` 直接接管 thinkbot 进程，
  行为与未引入 loader 前完全一致，现有部署零影响。
- **`true`**：监管 + 自部署 + 运维接口。

> 启用但 `loader.token` 为空时 loader **拒绝启动**（fail-safe），避免无鉴权的危险操作暴露。

## 进程架构

```
entrypoint.sh (docker.sock 组归属 + 降权 uid=1000)
   └─ exec /app/thinkbot-loader          （selfhost 镜像才有该二进制）
         ├─ loader.enabled=false → exec /app/thinkbot        （透明，现状）
         └─ loader.enabled=true  → Supervisor + OpsServer
                                       └─ child: /app/thinkbot  (CWD=/app，读 .env)
```

loader 与 thinkbot 是**两个进程**：子进程崩溃/升级不影响监管者。

## 组件

| 文件 | 职责 |
|---|---|
| `config.go` | 从 `.env` 读 `loader.*` 配置；由 `api.addr` 推导健康 URL；解析二进制路径 |
| `health.go` | 轮询子进程 `/health`，解析 `status/pid/gitShort`；`WaitUntilHealthy` 带超时 |
| `supervisor.go` | 启动/等待子进程、信号转发、指数退避重启、崩溃环检测触发回退；`ReplaceBin`/`Restart`/`RequestShutdown` |
| `deployer.go` | 单飞锁、快照 prev、按来源三档编译（工作树/ pull / checkout）+ `npm build` + `go build`(带 ldflags)、隔离 smoke test、健康门控切换/回滚、`RestorePrev`、部署日志落盘 |
| `ops.go` | 运维 HTTP 接口（`/loader/health` 公开 + 其余 token 保护） |
| `logger.go` | loader 自身控制台 logger（不写文件，避免与子进程日志混淆） |

入口：`cmd/loader/main.go`。

## 运维 HTTP 接口（默认 `127.0.0.1:8090`，由 `loader.ops_addr` 覆盖）

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/loader/health` | 公开 | loader 存活 + 子进程 pid/版本/健康 |
| GET | `/loader/status` | token | 子进程/监管状态、重启次数、最近部署、自部署能力 |
| POST | `/loader/deploy` | token | 触发自部署（body `{ref, pull}`）；返回 `{id,status}` |
| GET | `/loader/deploy/:id` | token | 轮询部署进度 + 构建日志（含失败原因） |
| GET | `/loader/deploy/history` | token | 最近若干次部署审计摘要（`?limit=N`），用于定位“上次失败” |
| POST | `/loader/rollback` | token | 回退到上一良版本 `thinkbot.prev` 并重启 |
| POST | `/loader/restart` | token | 仅重启子进程，不重新编译 |
| GET | `/loader/logs` | token | 读/流式 tail 子进程日志（`file=console\|json&lines=N&follow=true`）；`file=deploy&id=<id>` 读某次部署的完整构建/失败日志（跨重启可读） |

鉴权：变更类接口需头 `X-Loader-Token: <loader.token>` 或 `Authorization: Bearer <loader.token>`。

## 自部署 + 健康门控回滚

核心安全点：**旧二进制常态保留为 `thinkbot.prev`，新二进制先在隔离端口验证，
验证不过旧服务零中断**。

```
[运行 v1 @:8080]
   │ POST /deploy
   ├─ cp /app/thinkbot → /app/thinkbot.prev              （快照已知良）
   ├─ git fetch && checkout <ref>; cd web && npm run build; go build → /app/thinkbot.new
   │      └─ 编译失败 → 保留 v1 继续服务，返回错误（零影响）
   ├─ 启动 thinkbot.new，临时 .env 改 api.addr=:18080 + 独立 DB_PATH，轮询 /health
   │      ├─ 健康 → 停 v1 → mv new→thinkbot → 启动 v2 @:8080
   │      └─ 超时/崩溃 → kill new，丢弃；v1 全程在线，告警
```

> **为何用临时 `.env` 做端口隔离**：thinkbot 配置优先级为
> `overrides > db > .env文件 > os环境变量`，`.env` 文件会压过 `API_ADDR` 进程环境变量，
> 无法用环境变量改端口。故 smoke test 在临时目录生成一份改写 `api.addr` 的 `.env`
> 并配独立 `DB_PATH`，彻底避免与运行中的 v1 抢 `:8080` 与 SQLite（零争用、零中断）。

### 部署来源三档（build-from-working-tree）

`POST /loader/deploy` 的 body 决定编译什么：

- `ref` 为空且 `pull=false`（**默认**）：直接编译**当前工作树**，保留容器内 bot 的本地改动。
  这是「bot 改代码→部署」自举闭环的默认路径。
- `pull=true`：先 `git pull --ff-only` 合入上游（不丢弃本地未提交改动，冲突则中止），再编译工作树。
- `ref` 非空：fetch + checkout 该 ref（会丢弃本地改动，用于部署指定干净版本）。

### 部署日志落盘

每次部署无论成功失败都落盘到 `data/loader/deploy-<id>.json`，并向
`data/loader/deploy-history.jsonl` 追加审计行。故即便 thinkbot 重启，bot 仍能经
`/loader/deploy/:id` 或 `/loader/logs?file=deploy&id=` 读到上次的完整构建/失败日志。

## bot 自举闭环（容器内 thinkbot 调用 loader）

loader 的运维接口跑在 `127.0.0.1:8090`，与 thinkbot **同容器**，因此 thinkbot 内的 bot
（LLM agent）也能直接调它——形成「用户反馈 → bot 改源码 → bot 编译部署 → 失败则读日志再修再部署」的
自我修复闭环。

该能力由 `agent/tools/selfhost` 动态工具提供者暴露（仅 `loader.enabled=true` 时注册），包含：

| 工具 | 风险 | 说明 |
|---|---|---|
| `tb_loader_status` | sensitive | 查 loader/子进程状态、部署能力、最近部署 |
| `tb_deploy` | sensitive | 触发自部署，返回 id（异步，需轮询） |
| `tb_deploy_status` | sensitive | 按 id 查部署进度 + 完整日志 |
| `tb_deploy_history` | sensitive | 列最近部署，定位上次失败 |
| `tb_logs` | sensitive | 读运行/部署日志 |
| `tb_read_source` | sensitive | 读源码树文件（`loader.git_dir`，默认 `/app/src`） |
| `tb_write_source` | sensitive | 写源码树文件（路径穿越防护，禁止写 `.git`） |
| `tb_rollback` | sensitive | 回退上一良版本并重启 |
| `tb_restart` | sensitive | 仅重启子进程 |

> ⚠️ **默认全部禁止**：9 个 `tb_*` 工具统一按「敏感工具（sensitive）」分级，**默认不开放**。
> 即便 `.env` 已开 `loader.enabled` 注册了这些工具，也必须由管理员在「工具权限」页为对应
> bot / 平台**显式写 allow 规则**才能启用（建议 `tool=tb_*` + 指定平台 + 指定可信用户）。
> 没有 allow 规则时，LLM 既看不到也调不到它们——这是「loader 工具默认关闭，需在权限系统开启」的
> 硬性保证，防止自托管高权限通道被对话中的 LLM 默认持有。

工具经 `config` 读取 `loader.token` 注入 `X-Loader-Token` 头，**凭据不进入 prompt**（LLM 看不到）；
只读工具同样走受控端点。典型闭环：用户报缺功能 → bot 用 `tb_read_source` 看代码 →
`tb_write_source` 改码 → `tb_deploy` → `tb_deploy_status` 轮询 → 若 `failed`，用
`tb_logs?file=deploy&id=` 看错误 → 修复 → 再 `tb_deploy`。

> ⚠️ 安全提示：这套工具让 bot 具备「改写自身源码并热部署」的能力，威力极大。
> 仅在可信自托管环境开启 `loader.enabled`，务必配置强随机 `loader.token`，并在权限系统中
> 仅对可信 bot / 用户放开 `tb_*` 工具（写类工具尤其要收口到具体用户）。

## 运行时崩溃自动回滚（兜底）

子进程异常退出：指数退避重启；若 `crashloop_max` 窗口内连续崩溃超阈值 → loader 把
`thinkbot.prev` 复制回 `thinkbot` 并启动，跳出坏版本崩溃环。若 prev 也崩则持续重试 prev
并告警（不再二次回退，避免死循环）。

## 信号处理与优雅退出

loader 捕获 `SIGTERM`/`SIGINT` → 置「正在关停」→ 转发子进程 → 等子进程退出（沿用 45s grace）
→ loader 退出。`restart: unless-stopped` 仅兜底 loader 自身死掉。

## 构建 / 部署

- 普通构建：`go build -o thinkbot-loader ./cmd/loader`（依赖 `config`、`internal/buildinfo`）。
- 自托管镜像：`THINKBOT_TARGET=selfhost docker compose up -d --build`（见 `docker-compose.yml`、
  `Dockerfile` 的 `selfhost` 阶段）。镜像含 git/go/node 工具链与全量源码仓 `/app/src`。
- 验证：`go test ./loader/...`、`go build ./...`、`go vet ./loader/...`。
