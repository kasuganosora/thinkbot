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
| `deployer.go` | 单飞锁、快照 prev、`git pull`/`checkout` + `npm build` + `go build`(带 ldflags)、隔离 smoke test、健康门控切换/回滚、`RestorePrev` |
| `ops.go` | 运维 HTTP 接口（`/loader/health` 公开 + 其余 token 保护） |
| `logger.go` | loader 自身控制台 logger（不写文件，避免与子进程日志混淆） |

入口：`cmd/loader/main.go`。

## 运维 HTTP 接口（默认 `127.0.0.1:8090`，由 `loader.ops_addr` 覆盖）

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/loader/health` | 公开 | loader 存活 + 子进程 pid/版本/健康 |
| GET | `/loader/status` | token | 子进程/监管状态、重启次数、最近部署、自部署能力 |
| POST | `/loader/deploy` | token | 触发自部署（body `{ref,force}`）；返回 `{id,status}` |
| GET | `/loader/deploy/:id` | token | 轮询部署进度 + 构建日志 |
| POST | `/loader/rollback` | token | 回退到上一良版本 `thinkbot.prev` 并重启 |
| POST | `/loader/restart` | token | 仅重启子进程，不重新编译 |
| GET | `/loader/logs` | token | 读/流式 tail 子进程日志（`file=console\|json&lines=N&follow=true`，follow 走 SSE） |

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
