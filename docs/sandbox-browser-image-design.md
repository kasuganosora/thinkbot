# 设计文档：沙箱浏览器镜像（thinkbot-sandbox）

> 状态：执行中（2026-08-19）
> 已完成并验证：P1 Dockerfile + 镜像构建；P1 thinkbot 改造（`--shm-size`/ripgrep 双发行版）；P0 spike（x.com/小红书 经 patchright 落地页 200 + `__cf_bm`/`web_session` cookie 过反 bot）；**P2 浏览器 MCP wrapper（`docker/sandbox/browser-mcp.js`）+ 经 `docker exec -i` 链路端到端验证通过**；P2.5 Cookie 后端+前端；risk.go 登记 `browser__`（broadcast 守卫）进 broadcast。
> **P2 thinkbot 侧接线已完成（2026-08-19）**：`agent/bot/bot.go` + 新文件 `agent/bot/browser.go` 在 docker 后端 + `sandbox.browser.enabled=true` 时，按确定容器名 `thinkbot-bot-<id>` 动态 `AddServer` 一个 stdio MCP server（`docker exec -i <cid> xvfb-run -a -s "-screen 0 1920x1080x24" node /usr/local/bin/thinkbot-browser-mcp`），`EnableServer` 连接 + `mcp.RegisterTools` 注入该 bot 的 per-bot ToolManager（工具名 `browser__<tool>`）；会话前经 `wsMgr.WriteFile` 投递 DB cookie 到 `/data/.browser-state.json`，`Bot.Close` 时先关 MCP（wrapper 回写 cookie）再 `ReadFile` 回收持久化；`botservice.go` 注入 cookie loader/saver 闭包解耦 dao；`sandbox.Config.BrowserEnabled` + 配置键 `sandbox.browser.enabled` 落地。全量 `go build ./...` 通过。
> **P3（2026-08-19）已完成**：① stderr 接管（`mcp.ServerConfig.Stderr` + `newStdioTransport` 透传 + `agent/bot/browser.go` 注入回写 bot 日志的 writer，浏览器子进程错误不再静音）；② `BOT_BROWSER_PROXY` 配置键（`sandbox.browser.proxy`）+ `sandbox.Config.BrowserProxy` + `botservice` 读取 + `browser.go` 透传为 wrapper 的 `BOT_BROWSER_PROXY` 环境变量，IP 归部署侧；③ 删 bot 同步清 cookie（`DeleteDefinition` 删 `BotBrowserCookie`，杜绝凭据残留）。
> 剩余 P3 全部完成（2026-08-19）：非 root 加固（浏览器 MCP 经 `docker/sandbox/browser-launch.sh` 以非 root `bot` 用户运行，缩小 `--no-sandbox`+root 提权面；agent 沙箱与 named volume 仍 root 兼容，不动既有 volume 权限）、`browser_fetch` 轻量工具（在 `docker/sandbox/browser-mcp.js` 实现为 `fetch` 工具，纯 Node fetch 不启 chromium，经 MCP 暴露为 `browser__fetch`）。
> 作者：thinkbot agent（栞娜）
> 日期：2026-08-18
> 前置调研：opencode（anomalyco/opencode）浏览器控制机制 —— 本体不内置浏览器，靠 MCP 外挂 `@playwright/mcp`，工具调用统一过权限层，截图走 image attachment 喂多模态。

---

## 1. TL;DR

- 新建独立沙箱镜像 `thinkbot-sandbox:latest`（`docker/sandbox/Dockerfile`），替代默认的 `alpine:latest`（`config/module.go:1054`）。
- 浏览器选 **Chromium**（apt 版），不用原版 Chrome：**Google 官方 Linux Chrome 只有 amd64，无 arm64 构建**，而本地开发机是 Apple Silicon（arm64）。详见 §3。
- 驱动层：Node + **Patchright 起步**（Playwright 反检测补丁版），agent 侧经 `docker exec -i` stdio 起按需进程——与 opencode 同构，且是 per-bot 容器（无端口映射）下唯一可行的 MCP 形态。`@playwright/mcp` 仅用于 P2 链路验证（Patchright 无官方 MCP server，生产用自建薄封装，见 §4）。
- 分阶段落地：P0 spike（x.com/小红书实测）→ P1 镜像 → P2 stdio-over-exec MCP 桥 → P3 工具注册 + 权限 + 打磨。
- **需求确认（2026-08-19）**：① 浏览器**必须运行在 per-bot 容器内**（本方案本来就是这个形态，见 §4——浏览器进程随 MCP server 起在 `thinkbot-bot-<id>` 容器里，profile 落该 bot 的 named volume，天然 per-bot 隔离）；② 新增 **Web 端 Cookie 管理**（注入 / 编辑 / 删除），见 §10。

## 2. 现状盘点（代码事实）

| 事实 | 位置 |
|---|---|
| 沙箱镜像全局配置 `sandbox.image`，默认 `alpine:latest` | `config/keys.go:138`、`config/module.go:1054`、`.env.example:12` |
| per-bot 容器 `docker create <image> sleep infinity`，**无 `-p` 端口映射**（有出网无入网） | `sandbox/botcontainer.go:275` |
| agent 到容器唯一通道是 `docker exec`（`sh -c req.Command`） | `sandbox/botworkspace.go:805` |
| 镜像启动时 `docker pull`，失败仅 Debug 级日志（非致命） | `sandbox/botworkspace.go:108-111` |
| alpine 里没有浏览器、Node、glibc | — |

推论：**http/socket 形态的 MCP server 和常驻 Chrome DevTools 服务在 per-bot 容器里都不可达**，只有「按需起进程 + stdio 通信」一条路。

## 3. 浏览器选型：Chromium，不是原版 Chrome

| 维度 | 原版 Chrome（google-chrome-stable） | Chromium（debian apt 版） |
|---|---|---|
| **CPU 架构** | ❌ **官方 Linux 版仅 amd64**，无 arm64 deb/rpm | ✅ amd64 + arm64 都有 |
| 本地 Apple Silicon 调试 | 只能 `--platform=linux/amd64` 模拟跑，性能差且行为偏差 | 原生 |
| 服务器（142.44.213.100, x86_64） | 可用 | 可用 |
| 私有 codec（H.264/AAC） | 有 | debian chromium 版视编译而定 |
| 安装源 | 需加 dl.google.com apt repo + GPG key（构建环境可能慢/被墙） | 官方源直接装 |
| 许可 | 专有软件 | 开放 |
| Playwright 支持 | `channel: "chrome"` | `executablePath` 指系统 chromium，或 playwright 自带 chromium |

**结论：主线用 debian apt 的 Chromium**。理由：① arm64 硬约束（两边架构一致，测试无偏差）；② 安装链路最短；③ thinkbot 场景（抓 Misskey、网页摘要、截图）不需要私有 codec。

关于指纹/反爬：maid.lat 之前遇过 Cloudflare `1010`，但那是 UA 层问题（已验证带正常浏览器 UA 可解），与 Chrome vs Chromium 二进制无关——UA 由 Playwright 启动参数控制，两者都能伪装。

> 若未来确认需要原版 Chrome（如真实浏览器指纹要求），留 `ARG USE_CHROME=1` 分支：`[ "$(dpkg --print-architecture)" = amd64 ]` 时装 google-chrome-stable，arm64 仍回落 chromium。**但这意味着两边环境不一致，非必要不启用。**

## 4. 驱动层选型：Playwright MCP over `docker exec` stdio

**浏览器进程运行位置（明确需求）**：浏览器**跑在 per-bot 容器 `thinkbot-bot-<id>` 内部**，不是宿主、不是共享容器。链路是 `thinkbot 主进程 → docker exec -i → 容器内 MCP server 进程 → 容器内 chromium`。带来的隔离性：每个 bot 独立 profile/cookie/缓存/出网、互不可见；容器销毁即彻底清除；资源限制沿用 per-bot 的 `--memory`/`--cpus`。

三个候选对比（对照此前沙箱 Chrome 方案 ①②③）：

| 方案 | 形态 | 可行性 | 评价 |
|---|---|---|---|
| ① 独立常驻容器 + http MCP | 全局 `-p` 容器跑 CDP 服务 | 可行 | ❌ 失去 per-bot 隔离；浏览器会话跨 bot 串台 |
| ② **stdio MCP 经 `docker exec -i`** | 每次工具会话按需起 `@playwright/mcp` | ✅ **推荐** | per-bot 隔离 + 会话内状态保持（tab/cookie）；opencode 同构 |
| ③ headless chromium CLI | `chromium --headless --dump-dom/--screenshot` | 可行 | 补充位：轻量抓取不必起 MCP，直接走现有 Exec 通道 |

方案②的关键细节（抄 opencode）：

- **好消息：不需要新写 transport**。thinkbot `mcp/` 包已有通用 stdio transport（`mcp/transport.go:47 newStdioTransport(ctx, command, args, env)`，就是 `exec.CommandContext` + stdin/stdout 管道），把 `command="docker"`、`args=["exec","-i","thinkbot-bot-<id>", ...]` 配上即可复用——**P2 工作量从"实现 transport"缩小为"动态注册一个会话级 MCP server 配置"**。
- **但 `cmd.Stderr = nil` 会吞掉 MCP server 的 stderr**（`mcp/transport.go:64`）：浏览器起不来时错误全静音，排查会瞎。P2 必须改成接管 stderr 并 Infow 记录（对照 opencode 用的是 `stderr: "pipe"`）。
- **进程生命周期**：一个 agent 会话绑定一个 MCP server 进程（`docker exec -i thinkbot-bot-<id> <driver> ...`），会话结束/超时即 kill exec。浏览器由 MCP server 首次调用时拉起，随进程退出。驱动 `<driver>` 两档：P2 链路验证用 `npx -y @playwright/mcp`（官方现成），生产换 Patchright 薄封装（Patchright 无官方 MCP server，封装只是 launch 参数 + 进程管理的胶水层）。
- **`docker exec` 的 kill 语义（坑）**：`exec.CommandContext` cancel 只杀本地 `docker exec` 客户端进程，**容器内的 node/chromium 会变成孤儿继续跑**（docker exec 不传播信号给容器内进程）。必须额外做容器内清理（`docker exec <c> pkill -f <driver>`）或让封装层自己监听 stdin EOF 退出。§8#3 的"exec 进程泄漏"就是指这个，比表述的更严重。
- **user-data-dir 与 storageState 的 API 冲突（必须选边）**：Playwright/Patchright 里 `launchPersistentContext(userDataDir)` 与 `browser.newContext({storageState})` 是**互斥的两套**——persistent context 不接受 storageState 参数。取舍：
  - **选 persistent context（推荐）**：`launchPersistentContext('/data/.browser-profile', {...})`，指纹/缓存/localStorage 全持久化，最像真人长期使用的浏览器。cookie 管理改为**启动后用 `context.addCookies(...)` 注入**、`context.cookies()` 导出、删除用 `context.clearCookies()` 后重注入。§10 的 storageState JSON 仍是 DB 侧的存储格式，只是投递手段从"launch 参数"变成"启动后 API 调用"。
  - 选 newContext：cookie 注入更直接，但每次都是新 profile → 指纹/缓存不连续，对 x.com/小红书是减分项。
  → **定案：persistent context + addCookies 注入**。§10.1 的"运行时投递"按此实现。
- **user-data-dir 持久化（关键）**：浏览器 profile 必须显式指向 named volume 内路径（`/data/.browser-profile`，容器工作目录常量 `VirtualRoot = "/data"`，见 `sandbox/sandbox.go:379`），否则 MCP 进程每次重启登录态与指纹全丢。
- **并发约束**：Chromium profile 有文件锁，同一 user-data-dir 不能双开 → per-bot 同时只允许一个浏览器会话；多 agent 并发要么排队，要么各自独立 profile（指纹隔离性变差，不推荐）。
- **progress token 保活**：MCP `callTool` 带 `resetTimeoutOnProgress`，防止长导航超时断连。
- **超时三层对齐**：MCP callTool 超时 ≥ thinkbot 工具层超时 ≥ 单页最长等待（Turnstile 挑战可达 40s+），否则外层提前腰斩内层白等。
- **截图回传（原方案不成立，已重设计）**：thinkbot 工具结果是 `llm.ToolResultPart{Result any}`，两个 adapter 都对它 `json.Marshal` 成纯文本（`llm/anthropic/adapter.go:650 toolResultToContent`、`llm/openai/adapter.go:376`）——**图片塞进工具结果只会变成一串 base64 文本，模型看不到内容**。这不是 thinkbot 疏漏：OpenAI `function_call_output` 协议本身只接受字符串（Anthropic tool_result 支持 image block，但 GLM 主力走 openai 协议）。二选一：
  - **A（推荐，P2 默认）**：截图落盘 workspace，工具结果**只回文本**（路径 + 最终 URL + 页面标题 + 可访问性树/DOM 摘要）。可访问性树对 LLM 其实比图片更好用（可定位、可点击、token 更省），截图主要留给人看和存证。需要"看图"时复用已有 `MultimodalStage` 转写路径（`agent/stages/multimodal.go:209`，辅助模型把图转文字）或单开 `describe_image` 工具。
  - **B（改协议层，单列）**：给 ToolResultPart 加 attachment 通道，orchestrate 后把 image 作为**下一条 user 消息的 ImagePart** 注入（绕开 tool_result 不支持图片）。改动面到 `llm/orchestrate.go`，P2 不做。
  → 文档其余处凡提"截图喂多模态"均指 A。
- **instructions 注入**：MCP server 的 `instructions` 字段并入工具描述，省一次发现成本。

## 5. 镜像设计

`docker/sandbox/Dockerfile`（草案）：

```dockerfile
# syntax=docker/dockerfile:1
FROM debian:bookworm-slim

# 基础工具 + 浏览器 + 中文字体（截图不豆腐）+ xvfb（headful 伪装）
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git wget unzip jq xvfb \
      chromium fonts-noto-cjk \
      python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

# Node LTS + Patchright（Playwright 反检测补丁版，drop-in API）
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g patchright \
    && npm cache clean --force

# 指向系统 chromium。注意：executablePath 不是环境变量能定的——
# Playwright/Patchright 官方 env 只有 PLAYWRIGHT_BROWSERS_PATH /
# PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD，实际路径在 launch 时传 executablePath
# （@playwright/mcp 用 --executable-path，自建封装用 launch({ executablePath })）。
# CHROMIUM_FLAGS 同样由封装层读取转为 launch args（chromium 本体只认命令行参数）。
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 \
    BOT_BROWSER_EXECUTABLE=/usr/bin/chromium \
    BOT_BROWSER_LAUNCH_ARGS="--no-sandbox --disable-dev-shm-usage --disable-gpu --disable-blink-features=AutomationControlled"

# 非 root 兜底（chromium sandbox 已关）。
# ⚠️ 破坏性改动，P1 暂不启用：thinkbot 的 docker create/exec 均不传 --user
# （botcontainer.go:250-275 / botworkspace.go:805），一切跑镜像默认用户。切非 root 后：
# ① 已存在 named volume 属 root，新用户写不进（需 chown / 首次 create 初始化权限）
# ② 现有沙箱工具（写文件、apt/pip 装包）会 EACCES。
# → P1 保持 root（与现状一致；本来就无 --privileged），非 root 列为 P3 加固，
#   届时同步改 create/exec 的 --user 与 volume 权限初始化。
# RUN useradd -ms /bin/bash bot
# USER bot
WORKDIR /data   # 与 sandbox.VirtualRoot 一致（sandbox/sandbox.go:379）；named volume 挂载点
```

要点：

| 点 | 说明 |
|---|---|
| `--no-sandbox` | 容器内无 user namespace，Chromium 自带 sandbox 起不来。**注意 P1 仍以 root 跑**（见 Dockerfile 注释），`--no-sandbox` + root 是容器内提权面最大的组合；兜底靠无 `--privileged` + 容器边界，非 root 加固列 P3 |
| **`--disable-dev-shm-usage` vs `--shm-size` 取舍** | 二者别同时依赖：加 `--shm-size=512m`（改 `botcontainer.go`）是正解，性能好；`--disable-dev-shm-usage` 把共享内存换成磁盘临时文件，渲染大页面明显变慢。P1 先给 `--shm-size`，flag 留作无法改 create 参数时的兜底 |
| **apt chromium 版本兼容** | bookworm 的 chromium 版本落后于 Playwright/Patchright driver 的适配版本，CDP 协议大概率向后兼容但**未验证**——P0 必测；不过则备选 `patchright install chromium` 用自带构建（镜像 +300MB） |
| **xvfb 屏幕参数** | 默认 xvfb 分辨率/色深是低配（8 位色），canvas 指纹会露馅；必须 `-screen 0 1920x1080x24` 起 |
| **TZ/语言一致性** | 幸好 `botcontainer.go` 已有 `-e TZ=<cfg.Timezone>` 注入（`botcontainer.go:259`）；镜像补 `ENV LANG=zh_CN.UTF-8`（可覆盖），viewport/时区/语言三者和出口 IP 地理保持一致 |
| **WebRTC 泄漏** | 走代理时 WebRTC 会绕过代理暴露真实 IP——launch args 加 `--force-webrtc-ip-handling-policy=disable_non_proxied_udp`（或封装层禁 WebRTC） |
| **内存预算** | per-bot 容器默认 `--memory 2g`（`sandbox.go:223`）——headful Chromium 一实例 0.5~1GB + Node/MCP 进程，2g 很紧张（有 2G OOM 历史事故）；启用浏览器的 bot 建议调到 3g+（per-bot 覆盖机制已有，`botworkspace.go:145`） |
| 体积估算 | bookworm-slim 75MB + chromium+deps ~500MB + 字体 ~300MB + node ~100MB + xvfb ~50MB ≈ **1.0~1.4GB**（对比 alpine ~10MB） |
| 多架构 | 本地 buildx arm64 调试；服务器 x86_64 各自 build（镜像不含密钥，可重复构建，不强求 registry） |
| 现有工具链保留 | git/python3 顺手带上，`run_code` 等现有沙箱工具受益 |

## 6. thinkbot 侧改造点

| # | 改造 | 位置 | 阶段 |
|---|---|---|---|
| 1 | `docker/sandbox/Dockerfile` + 构建脚本 + `.env.example` 的 `sandbox.image` 说明 | 新文件 | P1 |
| 2 | `botcontainer.go` create 时加 `--shm-size=512m`（chrome 稳定性，优于 --disable-dev-shm-usage） | `sandbox/botcontainer.go:250-275` | P1 |
| 3 | **ripgrep 引导兼容**：create 后的引导命令是 `apk add ripgrep`（`botcontainer.go:297`），debian 镜像无 apk 会静默失败（`\|\| true` 兜底，rg 缺失退化 grep）——改为 `apk add \|\| apt-get install -y` 双发行版兼容，或干脆把 ripgrep 装进镜像 | `sandbox/botcontainer.go:296-298` | P1 |
| 4 | MCP 桥：复用 `mcp/` 现有 stdio transport（`command=docker`, `args=[exec,-i,...]`）+ 会话级注册/注销 + stderr 接管 + 容器内孤儿清理 | `mcp/`、`sandbox/` | P2 |
| 5 | MCP 工具注册进 agent 工具集：命名 `browser_<tool>`，inputSchema 透传 | `agent/` 工具注册处 | P2 |
| 6 | 截图走方案 A：落盘 + 工具结果只回文本（路径/URL/标题/a11y 树），不塞 image（协议限制见 §4） | 封装层 + 工具注册 | P2 |
| 7 | Patchright 薄封装：`launchPersistentContext`（executablePath/xvfb/user-data-dir/代理/BOT_BROWSER_* env 消费）+ cookie 注入导出 + 工具面（navigate/click/screenshot/等） | 容器内 JS（随镜像分发） | P2 |
| 8 | 可观测性：浏览器工具每次调用记 Infow——导航 URL、最终 URL、HTTP 状态、截图落盘路径、失败原因（遵循项目"内容型日志必须 Infow"纪律；截图二进制与 **cookie 值** 不进日志） | P2 封装 + `orchestrate.go` 现有工具日志 | P2 |
| 9 | 权限（**位置纠正：`toolperm/risk.go`，不在 `sandbox/`**）：`browser_` 前缀登记进 `sensitivePrefixes`；**能发帖的浏览器工具（x.com 发推、小红书评论）必须进 `broadcastPrefixes`/`broadcastTools`**——否则绕过发言三态（`SpeakMode` mute/passive 只拦 `misskey_create_*` 和 `__outbound_reply`，管不到浏览器发帖，等于开了第四条发言路径）；只读类（navigate/screenshot/read）走 `sensitivePrefixExceptions` 或留 sensitive；外加工具层限速。**且不得注册任何读 cookie 的工具**（§10.4） | `toolperm/risk.go` | **P2（不能拖到 P3）** |
| 10 | **Cookie 管理后端**：DB 表 `bot_browser_cookies` + `api/handler_bot_browser.go`（7 个端点，见 §10.2）+ 导入解析（storageState / cookies.txt / 扩展 JSON）+ 掩码与审计 | `dao/`、`api/`、`api/router.go` | **P2.5** |
| 11 | **Cookie 管理前端**：`web/src/components/bot/BotBrowser.vue` + 挂进 `BotSettings.vue` 导航（参照 `BotMcp.vue`/`BotFiles.vue`） | `web/src/` | **P2.5** |
| 12 | 轻量补充：`browser_fetch` 工具（实现为 `browser-mcp.js` 的 `fetch` 工具，纯 Node fetch 不启 chromium，暴露为 `browser__fetch`；非独立 Exec） | `docker/sandbox/browser-mcp.js` | **P3 ✅ 已完成** |
| 13 | `BOT_BROWSER_PROXY` 配置键 → 封装层 launch `proxy` 参数透传（机房部署接住宅代理） | config keys + P2 封装 | P3 |
| 14 | 删 bot 时同步清 cookie 表（`DestroyBot` 只删 volume，DB 凭据会留，见 §8#12） | `sandbox/botworkspace.go` + dao | P3 |

## 7. 反检测风险评估（Playwright 会被网站识别吗）

分目标站防御等级（2026-08 现状，经 web 调研核实）：

| 防御等级 | 代表 | 原版 Playwright 表现 |
|---|---|---|
| 无 / 基础 | 自建实例（Misskey）、普通内容站 | ✅ 畅通，不检测自动化 |
| 中 | Cloudflare 基础版 / JS Challenge | ⚠️ 大概率拦（`navigator.webdriver`、CDP `Runtime.enable` 痕迹、HeadlessChrome UA） |
| 高 | CF Enterprise / Datadome / Akamai / PerimeterX | ❌ 基本拦 + 行为分析（鼠标轨迹、事件时序） |

**thinkbot 主场景评估（2026-08-18 修正：主战场是 x.com 与小红书，非 Misskey）**：

x.com / 小红书属于上表"高"档甚至更高，且难度是**四层叠加**，浏览器指纹只是其中一层：

| 层 | 现实 | 浏览器方案能否解决 |
|---|---|---|
| 1. 自动化指纹 | headless 特征、CDP 痕迹、启动参数 | ✅ 可解：headful + Patchright/Camoufox |
| 2. IP 信誉 | **机房 IP 几乎必被标记**；小红书对海外 IP 风控更严（bot 服务器在海外机房 = 双重减分）；x.com 对数据中心 IP 高频访问限流 | ❌ 浏览器解决不了：需住宅代理或家宽出口 |
| 3. 登录态/账号风控 | x.com 未登录基本看不了内容；小红书未登录 web 只剩残缺页面 → **必须有真实账号 cookie**；自动化操作登录号有封号/锁号风险 | ⚠️ cookie 可持久化（named volume），但账号风控是使用纪律问题（频率、行为像人） |
| 4. 行为分析 | 鼠标轨迹、滚动、点击时序；小红书还查 canvas/webgl/字体指纹一致性 | ⚠️ 部分可解（Camoufox 指纹随机化），瞬时工具式点击仍是弱项 |

**结论：四层中，指纹与会话两层归 thinkbot（本方案），IP 与账号两层归部署侧（用户自理）。** 责任边界：

| 层 | 归属 | 提供物 |
|---|---|---|
| 自动化指纹 | **thinkbot（镜像内置）** | headful + xvfb + Patchright 起步，Camoufox 可选层 |
| 会话持久化 | **thinkbot（基础设施）** | cookie/存储走 per-bot named volume，容器重建不丢登录态 |
| 出口 IP | **部署侧** | thinkbot 可跑在用户自己的 PC（家宽出口即天然合规）；镜像/驱动层留**浏览器代理配置入口**（`BOT_BROWSER_PROXY` → Playwright launch `proxy` 参数），机房部署时用户自行接住宅代理 |
| 账号 | **部署侧** | 用户自己的账号 cookie（手动导出 storage_state 导入，禁自动登录）；频率/行为纪律由工具层限速约束（P2/P3）；cookie 明文存 volume 的风险由用户知晓（§8#12） |

**技术选型因此上调一档（镜像默认配置）**：

1. **headful + xvfb** 为默认（xvfb-run 包 chromium，约 +50MB）——headless 在这两个站基本等于自报家门。
2. **Patchright**（Apache-2.0，Playwright 源码级补丁 fork，API 99% drop-in，修掉 Runtime.enable/webdriver/自动化参数泄漏）替代原版 Playwright 作为驱动库——不再是"遇到再升级"，是起步配置。
3. **Camoufox**（Firefox C++ 级指纹 fork，标准检测 0% 检出）作为镜像可选层（Firefox-only、重）——若 Patchright 在小红书实测不过再上；注意它与 @playwright/mcp 官方 server 的组合需自建薄封装。
4. 指纹一致性：viewport/时区/语言与出口 IP 地理一致（x.com/小红书都做交叉校验）；TZ 注入机制 thinkbot 已有（`botcontainer.go:259`），缺的是 viewport/语言配置入口。
5. 登录态：cookie/存储持久化 = storageState JSON 存 thinkbot DB + 启动前投递到 per-bot volume（**Web 页面管理注入/编辑/删除，见 §10**）；**禁止表单自动登录**（登录流是风控最敏感环节，自动填账密极易触发锁号），账号导入路径 = 用户在自己浏览器登录后导出 cookie，从 Web 页面粘贴导入。
6. **明确不承诺**：对抗这两个站是持续军备竞赛，本方案提供的是"可用的基础设施 + 可替换的驱动层"，不是一次性解决。

已过时方案（不用）：playwright-extra/stealth 插件——JS 层运行时 patch，硬编码值反而可被原型链检测识破。

## 8. 风险与坑

1. **全局 `sandbox.image` 切换**：影响所有 bot；存量 per-bot 容器按旧镜像建，切换后需 `DestroyBot` 重建。建议先在一个测试 bot 上验证。
2. **镜像 pull 失败是 Debug 级非致命**（`botworkspace.go:110`）：镜像不存在时容器起不来，错误要到 exec 阶段才暴露——切换前先手动 `docker pull`/build。
3. **exec 进程泄漏（比想象严重）**：`docker exec` 的 cancel 只杀本地客户端进程，**容器内 node/chromium 变孤儿继续跑**（信号不传播）→ 必须容器内 `pkill -f <driver>` 或封装层监听 stdin EOF 自退。孤儿 chromium 每个吃 0.5~1GB，几次就把 2g 容器撑爆。
4. **MCP server stderr 被吞**：`mcp/transport.go:64` `cmd.Stderr = nil`，浏览器起不来时错误全静音 → P2 必须接管并 Infow。
5. **npm 源在服务器上可能慢**：预装进镜像后运行时零下载，构建时一次性成本。
6. **内存**：默认 `--memory 2g`（`sandbox.go:223`）对 headful Chromium + Node 太紧（有 2G OOM 历史事故）；启用浏览器的 bot 调 3g+（per-bot 覆盖已有，`botworkspace.go:145`）。
7. **chromium 版本兼容**：apt chromium（bookworm）vs Patchright driver 的 CDP 版本差——P0 验证 `executablePath=/usr/bin/chromium` 全功能可用；不过则 `patchright install chromium` 用自带构建（+300MB）。
8. **ripgrep 引导退化**：`botcontainer.go:297` 的 `apk add ripgrep` 在 debian 镜像静默失败 → §6#3。
9. **单会话约束**：Chromium profile 锁 → per-bot 同时一个浏览器会话，并发 agent 需排队；封装层要做互斥，否则第二个 exec 起的浏览器直接崩（profile locked）。
10. **超时链**：MCP callTool / thinkbot 工具层 / 页面最长等待三层不对齐会互相腰斩（Turnstile 挑战 40s+）。
11. **`--network none` 冲突**：`NetworkDisabled` 配置为真时容器无网（`botcontainer.go:271`），浏览器工具必须在注册时检查并给出明确错误，而不是让 agent 干等超时。
12. **cookie 是账号本体级凭据**：现在有两处副本——① thinkbot DB 的 storageState（权威，须加密+掩码，见 §10.4）② 容器 volume 内的 profile 与投递文件（宿主 `/var/lib/docker/volumes/...` 明文）。规则：cookie 值永不进日志/永不给 agent 工具；`DestroyBot` 删 volume 但 **DB 里的 cookie 会留下**（重建容器可恢复登录态——这是特性，但用户要知道"删了 bot 容器不等于删了账号凭据"，删 bot 时应同步清 cookie 表）。
13. **storageState 与 user-data-dir 的职责别搞混**：cookie 权威来源是 storageState JSON（可管理、可编辑）；user-data-dir 只承载缓存/指纹相关的 profile 数据。若两者冲突（profile 里旧 cookie vs 注入的新 cookie），以注入为准 → 封装层用 `newContext({storageState})` 而非依赖 profile 自带 cookie；否则用户在 Web 上删了 cookie，浏览器还拿 profile 里的旧值继续用。
14. **maid.lat 限流**（次要场景）：Playwright 复用会话 cookie 可减少触发；沿用 `channel/misskey/api.go` 已有的重试退避经验。

## 9. 分阶段实施

- **P0（spike，先于一切编码）**：本地起 xvfb + headful chromium（Patchright 驱动，`-screen 0 1920x1080x24`），实测 ① x.com 首页/搜索能否加载（无 cookie → 有 cookie 两轮）② 小红书 web 首页/搜索同理 ③ apt chromium 与 patchright driver 的兼容性（executablePath 全功能）。**"有 cookie"轮的 cookie 由用户手动导出**（自己浏览器登录后导出 storage_state），不测自动登录。本机（家宽）即代表"用户 PC 部署"的基线环境；机房 IP 场景由部署侧自行复测（IP 层归用户，见 §7 责任边界）。**以实测结果决定驱动层投入级别**（原版够 / Patchright 够 / 需 Camoufox）。
- **P1（镜像）**：Dockerfile + 本地 arm64 build + 单容器手动验证（chromium 截图中文不豆腐、xvfb-run headful 可用、patchright 能起浏览器、**ripgrep 引导兼容改造生效**）。
- **P2（MCP 桥）**：复用 `mcp/` 现有 stdio transport（command=docker exec，见 §4）+ 会话级 MCP 注册/注销 + stderr 接管 + 容器内孤儿清理 + 工具注册 + `toolperm/risk.go` 登记（发帖类进 broadcast，**安全前置不可拖**）+ 截图落盘走文本结果（方案 A）+ Patchright 薄封装（user-data-dir→`/data/.browser-profile`、BOT_BROWSER_* env、代理透传、单会话互斥）+ 工具调用 Infow 日志。先用 @playwright/mcp 跑通链路再换封装。测试 bot 端到端：agent 说"打开 X 页面并总结"→ 文本+截图路径回会话。
- **P2.5（Cookie 管理，本需求主体）**：DB 表 + API（`api/handler_bot_browser.go`）+ `BotBrowser.vue`，见 §10。优先级高于 P3——没有 cookie 管理，x.com/小红书场景基本不可用。
- **P3（打磨）**：~~browser_fetch 轻量工具~~（✅ 已实现 `browser__fetch`）、~~非 root 加固~~（✅ 仅浏览器 MCP 降权至 `bot` 用户，未切全容器 `USER` 以兼容 volume/沙箱）、`BOT_BROWSER_PROXY` 配置键落地（✅ 已修 `docker exec -e` 透传——此前经 `cfg.Env` 传给 docker 进程但进不了容器，从未真正生效）、工具层限速细化、viewport/语言配置入口、（可选）ToolResultPart attachment 通道即截图方案 B。

P0 不通过则整案降级为"普通站浏览器工具"，主战场（x.com/小红书）改走别的路线（App 协议逆向 / 第三方数据源），另立方案讨论。

---

## 10. Web 端 Cookie 管理（注入 / 编辑 / 删除）

需求：在 Web 页面管理 bot 浏览器的 cookie，免去"命令行往容器里塞文件"。这同时替换掉 §7#5 那条反人类的"用户手动导出 storage_state 放进 workspace"。

### 10.1 存储形态决策（关键，不能拍脑袋）

Chromium 的原生 cookie 存在 profile 里的 `Cookies` SQLite 文件，且 `encrypted_value` 列**用 OS keyring 加密**（Linux 上是 `gnome-keyring`/`kwallet`，容器内通常降级为硬编码密钥 "peanuts" 的 AES）。直接读写这个文件的问题：格式随 Chromium 版本变、加密方式依赖运行环境、并发写会破坏 profile。

→ **不碰 Chromium 的 SQLite。采用 Playwright `storage_state` JSON 作为唯一事实来源**：

| 层 | 内容 |
|---|---|
| 权威存储 | thinkbot DB 新表 `bot_browser_cookies`（或复用 config 存 JSON blob），字段与 Playwright storageState 对齐：`domain / name / value / path / expires / httpOnly / secure / sameSite` |
| 运行时投递 | persistent context 起好后调 `context.addCookies(cookies)` 注入（**不是 launch 的 storageState 参数**，二者互斥，见 §4）；thinkbot 在启 MCP 前把 JSON 写入 `/data/.browser-state.json`，封装层读取并注入 |
| 反向同步 | 会话结束时封装层 `context.cookies()` 导出 → 回写 DB（浏览器登录/刷新产生的新 cookie 不丢，这是"便捷性"的另一半）；冲突以**浏览器实际值为准**（它才是活的），但用户在 Web 上的删除操作要打标记避免被同步覆盖回来 |

好处：格式稳定（Playwright 官方 schema）、可编辑（纯 JSON）、可版本化、不依赖容器内 keyring、cookie 与 profile 解耦（换驱动/重建容器都不丢）。

### 10.2 API 设计（沿用现有 per-bot 子资源风格）

参照 `botsAdmin` 分组（`api/router.go:180-185` 的 MCP 路由是现成模板），新增：

```
GET    /api/bots/:id/browser/cookies            # 列表（value 默认脱敏）
POST   /api/bots/:id/browser/cookies            # 新增单条
PUT    /api/bots/:id/browser/cookies/:cid       # 编辑
DELETE /api/bots/:id/browser/cookies/:cid       # 删除单条
DELETE /api/bots/:id/browser/cookies            # 清空（?domain= 可按域清）
POST   /api/bots/:id/browser/cookies/import     # 批量导入：Playwright storageState JSON
                                                #            / Netscape cookies.txt
                                                #            / 浏览器扩展导出的 JSON 数组
GET    /api/bots/:id/browser/cookies/export     # 导出 storageState（需二次确认，见 10.4）
```

新建 `api/handler_bot_browser.go`，权限沿用 `requirePermission(auth.PermBotManage)`。

### 10.3 前端（Vue3 + TDesign）

`web/src/components/bot/BotBrowser.vue`，挂进 `BotSettings.vue` 左侧导航（现有结构 `web/src/views/BotSettings.vue:28-70`，与 `BotMcp`/`BotFiles` 同级），命名 tab「浏览器」：

- Cookie 表格：域名 / 名称 / 值（默认掩码，点击展开）/ 过期 / 属性标记；按域名分组。
- 操作：新增、行内编辑、删除、按域清空、粘贴导入（textarea 收 storageState JSON 或 cookies.txt）、导出。
- **踩过的坑复用**：① 值列很长 → 单列 flex + `nowrap` + 掩码，别放两列网格（工具名被截成 `task_co…` 的老问题）；② 导入用 `t-dialog` 时 body 挂外层，scoped 样式命中不了 → 非 scoped `<style>` + 自定义 class + `max-height/overflow-y:auto`；③ `t-form-item` 内控件溢出 → `:deep(.t-form__controls-content){flex-direction:column}`。
- 附「当前浏览器会话状态」小卡片：是否有活动会话、profile 大小、最后使用时间（cookie 改动需下次会话生效，要给用户明确提示）。

### 10.4 安全（这块不能马虎）

cookie 等于账号本体，比 API token 更敏感（可绕过二次验证）。

| 项 | 措施 |
|---|---|
| 传输/展示 | 列表接口默认返回掩码值（`abc***xyz`），完整值需单条 GET + 显式 `?reveal=true`，并记审计日志 |
| 存储 | DB 落盘**加密**（复用 thinkbot 现有凭据加密方式；若目前 provider apikey 是明文存，则本表至少与其一致并在文档标注风险） |
| 导出 | 导出全量 storageState = 导出账号，需二次确认弹窗 + 审计日志 |
| 权限 | 仅 `PermBotManage`；**绝不暴露给 agent**——agent 不需要读 cookie 明文，浏览器工具自动带上即可。不要给 LLM 任何 `get_cookies` 类工具（否则 cookie 会进 prompt、进 L0 记忆、进日志） |
| 日志 | cookie 值**永不进日志**（含 Infow 的工具日志），只记域名 + 条数 |

### 10.5 阶段归属

- **P2**：cookie 投递机制（封装层读 `/data/.browser-state.json` → `context.addCookies`）+ 会话结束 `context.cookies()` 反向同步。这是 cookie 能生效的前提。
- **P2.5（本需求主体）**：DB 表 + API + `BotBrowser.vue`。独立于 P3 打磨项，优先级高于它——没有 cookie 管理，x.com/小红书场景基本没法用。

---

## 附录：验证记录（2026-08-19，本地 arm64 + 家宽出口）

### 镜像验证
- `docker/sandbox/Dockerfile` → `thinkbot-sandbox:local`，arm64 构建成功，体积 ~1.85GB。
- chromium headless 截图 baidu 成功（50KB png，中文不豆腐）；xvfb-run headful dump-dom 正常。
- patchright 全局安装，`NODE_PATH=/usr/lib/node_modules` 下 `require('patchright')` + `chromium.launch({executablePath:'/usr/bin/chromium'})` 全功能可用（§7 担心的 CDP 版本兼容问题不存在）。

### P0 反 bot 站点实测（本机家宽 = 用户 PC 部署基线）
| 站点 | HTTP | 标题 / 落地 | 关键 cookie |
|---|---|---|---|
| example.com | 200 | Example Domain | — |
| **x.com** | 200 | "X. It's what's happening / X" | `__cf_bm`（**Cloudflare 反 bot 通过后才下发**）、`guest_id`、`personalization_id` |
| **小红书** | 200 | "小红书 - 你的生活兴趣社区"（跳 /explore） | `web_session`、`sec_poison_id`、`websectiga`、`webId` |

结论：指纹层（Patchright + headful + 中文字体/时区/UA）在用户 PC 基线下足以通过两站初始反 bot；账号/登录态/IP 仍归部署侧（见 §7）。

### P2 浏览器 MCP wrapper 端到端（模拟 thinkbot 的 `docker exec -i` 链路）
- 命令形态：`docker exec -i <cid> xvfb-run -a -s "-screen 0 1920x1080x24" node /usr/local/bin/thinkbot-browser-mcp`
- JSON-RPC：`initialize` → `tools/list`（11 工具）→ `navigate`(x.com/小红书) → `get_text` → `screenshot`(落盘 png) → `cookies_list`（cookie 注入/回收正常）。
- stdin EOF → wrapper 优雅 shutdown + 回写 `/data/.browser-state.json`（cookie 持久化闭环验证）。
- 修复：patchright 下 `page.accessibility` 取值方式差异导致 navigate 摘要崩溃，已加防御（不影响导航本身）。

### 已知接线坑（P2 thinkbot 侧已落地）
- MCP server 命名：框架统一为 `<server>__<tool>`，故 risk.go 登记 `browser__`（非 `mcp__browser__`，运行时工具名实为 `browser__navigate` 等）。✅ 已在 `setupBrowserMCP` 用固定 server 名 `browser`。
- 容器名来源：docker 名 `thinkbot-bot-<id>`（确定值，不依赖容器是否已创建），经 `wsMgr.ContainerInfo(ctx, botID).ContainerName` 取得，`docker exec` 接受容器名。✅
- cookie 文件投递/回收：会话前 `ws.WriteFile("/data/.browser-state.json", dbCookies)`；`Bot.Close` 先 **优雅调用 `browser__close` 工具**（`wrapper.saveState` 落盘后回包）→ 再 `browserMCP.Close()` 关传输 → `ws.ReadFile` 回收。⚠️ 坑：`docker exec -i` 默认不代理信号，直接 `browserMCP.Close()` 的 SIGKILL 不会触发 wrapper 的 `shutdown/saveState`，cookie 会丢；故必须显式 flush。兜底：wrapper 在每次 `navigate` 后及每 30s 周期落盘，即便被硬杀也只丢当前页瞬时态。状态文件权限：thinkbot 经 `WriteFile`（root，`cat >` 保留 inode owner）写入，故 launch 脚本降权前先 `chown bot:bot` 浏览器目录/状态文件，否则降权 wrapper 回写 EACCES。✅
- `cmd.Stderr=nil`（`mcp/transport.go:64`）吞错 → **已解决**：`ServerConfig.Stderr io.Writer` 字段 + `newStdioTransport` 透传；`agent/bot/browser.go` 注入 `mcpStdioStderr` 把 wrapper 的 stderr 回写到 bot 日志（wrapper 仅打诊断信息、绝不打印 cookie 值）。`EnableServer` 握手错误与进程内 chromium 崩溃细节均可见。
- `docker exec` cancel 只杀本地客户端，容器内 node/chromium 孤儿 → wrapper 已注册 SIGTERM/SIGINT/stdin-EOF 自清理，且 `transport.Close` 通过 `cancel()+stdin.Close()` 触发 EOF。✅ 另：wrapper 仅首个工具调用才起浏览器，空闲时零资源。
