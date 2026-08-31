# sandbox — Bot 沙箱工作空间

为每个 Bot 提供隔离的持久化工作空间，支持 Docker 容器沙箱和本地文件系统两种模式。LLM 可在工作空间内执行命令、读写文件。

## 功能

- **平台无关**：统一的 `Workspace` 接口，上层无需关心底层是 Docker 还是本地进程
- **Docker 隔离**：默认「一 bot 一长期容器」模式，文件落在 named volume，宿主机磁盘不落 bot 文件（真正隔离）
- **本地降级**：无 Docker 时退化为本地进程执行（**无容器隔离**，命令直接跑在宿主上）
- **工具注册**：通过 `BotWorkspaceToolProvider` 自动为每会话注册工作空间工具（`sandbox_exec` / `sandbox_read_file` 等）
- **持久化**：per-bot 工作空间目录（`data/workspaces/{botID}/`）跨会话保留
- **安全隔离**：禁止目录遍历（`..`）、symlink 逃逸；路径限制在工作空间根目录内
- **执行可靠性**：卡死看门狗 + 硬上限兜底 + 结果完整性/可信度信号（OOM/超时/信号杀检测）
- **Docker 路径自愈**：探测前主动在常见安装位置补 Docker 可执行文件到 PATH，避免 launchd/systemd 裁剪 PATH 导致静默退化为本地模式

## 后端模式

`Config.Backend` 决定命令执行的隔离方式（`NewSandbox` / `NewBotWorkspaceManager` 共用同一逻辑）：

| 值 | 行为 |
|----|------|
| `auto`（默认） | Docker 可用则用 Docker，否则降级 local；`RequireDocker=true` 时降级改为报错 |
| `docker` | 强制 Docker，不可用直接报错 |
| `local` | 强制本地进程执行 |

- **PersistentContainer**（仅影响 `BotWorkspaceManager` 的 Docker 后端）：默认 `true`。为 true 时每个 bot 绑定一个长期运行的容器（`thinkbot-bot-<botID>`），挂载 named volume（`thinkbot-bot-<botID>`）到容器内 `/data`；false 时为旧行为（每条命令起临时容器 `docker run --rm`）。
- **RequireDocker**：`auto` 模式下探测不到 Docker 直接报错，不降级（避免无隔离裸跑）。

## Docker 可用性探测与 PATH 自愈

`dockerAvailability()` 返回 `(bool, string)`——除了布尔可用性，还返回**人类可读的不可达原因**，用于让「静默降级到 local」变得可诊断：

- 先调用 `ensureDockerPath()` 自愈 PATH：通过 launchd/systemd 启动的进程，其 PATH 常被裁剪（macOS 默认不含 `/opt/homebrew/bin`），而 docker CLI 多装在那里；`LookPath("docker")` 失败 → auto 静默降级 local → LLM 命令直接跑在宿主、且看不到容器 volume 里的文件。探测前主动在候选目录（Homebrew、Docker Desktop、Rancher Desktop、Colima、OrbStack 等，另可经环境变量 `THINKBOT_DOCKER_BIN_DIR` 显式指定）中查找 docker，找到即补进本进程 PATH（仅首次、幂等，正常 shell 启动零影响）。
- 随后 `exec.LookPath("docker")` 探测可执行文件；再带 3s 超时执行 `docker version` 探测 daemon 是否在运行。
- 任一步失败，返回 `(false, reason)`；调用方据此降级或报错，并在日志/`sandbox_health` 中暴露原因。

## 关键类型

| 类型 | 说明 |
|------|------|
| `BotWorkspaceManager` | 持久化 per-bot 工作空间管理器（多 Bot） |
| `Sandbox` | 后端工厂接口：`Create` / `Close` / `Backend` |
| `Workspace` | 单工作空间接口（见下） |
| `SandboxManager` | 临时会话级工作空间池（`per-session`，空闲自动清理） |
| `Config` | 后端/资源/超时/安全配置 |
| `ExecRequest` / `ExecResult` | 命令执行请求/结果（含可信度信号） |
| `HealthStatus` | 健康状态（Healthy/Backend/Status/Message） |
| `botContainer`（未导出） | per-bot 长期 Docker 容器（named volume、OOM 就地提内存、快照） |

> Docker 后端底层有 `dockerWorkspace`/`localWorkspace` 实现 `Workspace`；per-bot 持久化路径则由 `botWorkspace` 包装（Docker 持久容器模式下转发到 `botContainer`，本地模式直接操作宿主目录）。

## Workspace 接口

```go
type Workspace interface {
    ID() string                                  // 工作空间唯一标识
    WorkDir() string                             // 工作目录（持久容器模式下为虚拟根 /data）
    Exec(ctx, ExecRequest) (*ExecResult, error) // 执行命令
    ReadFile(ctx, path) ([]byte, error)         // 读文件
    WriteFile(ctx, path, data) error            // 写文件（自动建父目录）
    ListDir(ctx, path) ([]FileEntry, error)     // 列目录
    HealthCheck(ctx) HealthStatus               // 健康状态（容器存活/目录可用）
    Close() error                                // 释放资源（持久化工作空间为 no-op）
}

// 可选：支持流式输出的工作空间实现该接口，否则仅用 Exec 即可。
type StreamWorkspace interface {
    ExecStream(ctx, ExecRequest, onChunk func(ExecChunk)) (*ExecResult, error)
}
```

## LLM 工具

通过 `sandbox.RegisterBotWorkspaceTools(toolMgr, mgr)` 注册。子 Agent 与主 Agent 共用同一 per-bot 沙箱（同一 BotID），故子 Agent 同样拥有这些工具；递归防护由 `spawn` 工具 scope 排除子 Agent 实现。工具 Scopes 为 `["private","group"]`。

| 工具 | 说明 | 备注 |
|------|------|------|
| `sandbox_exec` | 执行 shell 命令，返回 stdout/stderr/exitCode + 可信度信号 | 自动剥离命令末尾 `\| head`/`\| tail` 管道；验证型命令 OOM 时自动提内存重试一次 |
| `sandbox_read_file` | 读文件（支持 offset/limit 分段、带行号） | — |
| `sandbox_write_file` | 写文件（自动建父目录、覆盖） | — |
| `sandbox_replace_in_file` | 精确替换字符串片段（支持 `replace_all`） | — |
| `sandbox_delete_file` | 删文件/目录 | `DeferredLoad: true`（默认仅暴露名称+描述） |
| `sandbox_move_file` | 移动/重命名 | `DeferredLoad: true` |
| `sandbox_list_dir` | 列目录 | — |
| `sandbox_search_content` | 内容搜索（ripgrep，回退 grep） | — |
| `sandbox_health` | 健康检查（backend/状态/原因） | `DeferredLoad: true` |
| `run_code` | 在沙箱内一次执行多步脚本（bash/python/node），只回传最终 curated 输出（Programmatic Tool Calling） | 与 `sandbox_exec` 同隔离级别；支持 `workdir`/`timeout`/`stuck_timeout` |

> 完整工具提示词段落见 `tools.go` 的 `botWorkspaceToolPromptSection`。

`sandbox_exec` 参数：`command`（必填）、`workdir`、`timeout`（硬上限秒，0=卡死阈值×3）、`stuck_timeout`（卡死看门狗秒，默认 300）。返回额外字段：`truncated`、`reliable`、`aborted`、`oomKilled`、`warnings`、`workdir`；当 `reliable=false` 时还带 `reliabilityWarning`，提示 LLM 结果不完整不可信。

## 命令执行：卡死看门狗与可靠性

单条命令**不再用固定超时一刀切杀掉**，而是双机制：

- **卡死看门狗（StuckTimeout）**：命令连续无输出超过阈值（默认 5 分钟，可由 `sandbox.stuck_timeout` 配置）且已过启动宽限期、进程仍存活，才判卡死并终止。慢但持续输出的命令（如编译）不会被误杀。
- **硬上限（Timeout）**：总时长兜底（默认 = 卡死阈值 × 3，可由 `sandbox.timeout` 配置），无论有无输出，超过即强制终止，防无限挂起。

结果携带完整性/可信度信号（见 `ExecResult`）：`Reliable`/`Aborted`/`OOMKilled`/`Warnings`。检测分三层：退出码/超时/输出文本特征（`finalizeExecResult`）、cgroup `oom_kill` 前后对比、验证型命令 OOM 时经 `RetryOOMWithElevatedMemory` 在容器内就地 `docker update --memory` 提内存重试一次（封顶 `oomRetryMaxMB=16384`，不落库）。

## 配置

通过 `config.Store` 管理（详见配置文档与 `Config` 结构体）。关键键：

| 配置键 | 默认值 | 说明 |
|--------|--------|------|
| `sandbox.backend` | `auto` | `auto`/`docker`/`local` |
| `sandbox.stuck_timeout` | `300` | 卡死看门狗阈值（秒） |
| `sandbox.timeout` | `0` | 单命令硬上限（秒），0 = 卡死阈值 × 3 |
| `sandbox.require_docker` | `false` | auto 模式下强制要求 Docker，否则报错 |
| `sandbox.image` | `builtin` | Docker 镜像；`builtin`/空 = thinkbot 内置浏览器沙箱镜像（启动 bot 时按需自构建），其他值（如 `alpine:latest`）作为预构建镜像原样使用 |
| `workspace.dir` | `data/workspaces` | per-bot 工作空间根目录 |
| `system.timezone` | 服务器本地 | 容器/进程 TZ |

`Config` 运行时字段还包括 `MemoryLimit`（默认 `2g`）、`CPULimit`（默认 `1.0`）、`NetworkDisabled`、`Timezone`（默认 `UTC`）、`MaxOutput`（默认 1MB）、`MaxFileWrite`（默认 10MB）、`PersistentContainer`（docker 后端强制启用）、`BrowserEnabled`/`BrowserProxy`（per-bot 浏览器 MCP，见 `sandbox.browser.enabled`/`sandbox.browser.proxy` 配置）。

## 安全隔离

`validatePath(root, path)` 统一校验：

- 路径入参先做 `..` 段拒绝（防目录遍历）；
- 剥离虚拟根前缀 `/data`（agent 以 `/data` 为工作根，统一 docker/local 语义）；
- 解析 symlink 后确认仍在 root 内（防 symlink 逃逸）。

所有文件/命令操作均经此校验，路径不会逃逸出工作空间根目录。
