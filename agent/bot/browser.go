package bot

import (
	"bytes"
	"context"
	"io"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/mcp"
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// mcpStdioStderr 把浏览器 wrapper 的 stderr 回写到 thinkbot 日志（便于排障）。
// wrapper 仅在此输出诊断信息（绝不打印 cookie 值），按行回写。
type mcpStdioStderr struct {
	log    *zap.SugaredLogger
	redact []string
}

func (w *mcpStdioStderr) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if line != "" && w.log != nil {
		// 与握手失败时的 stderr 尾部使用同一套正则打码，再套上显式密文替换和长度上限。
		line = mcp.RedactStderr(line, w.redact)
		w.log.Infow("browser-mcp stderr", "line", sanitizeBrowserText(line, w.redact))
	}
	return len(p), nil
}

var _ io.Writer = (*mcpStdioStderr)(nil)

// ============================================================================
// 浏览器 MCP 接线 — per-bot，运行在 bot 持久容器内（见 docs/sandbox-browser-image-design.md）
//
// 链路：thinkbot 主进程 → `docker exec -i thinkbot-bot-<id>` → 容器内
//       共享 Xvfb :99 → node /usr/local/bin/thinkbot-browser-mcp（stdio JSON-RPC）→
//       容器内 headful chromium（patchright 驱动）。
//
// 工具命名遵循 MCP 框架约定 <server>__<tool>：本服务固定名为 "browser"，
// 故容器内工具即 browser__navigate 等，并已在
// toolperm/risk.go 的 broadcastPrefixes 登记 "browser__" 以约束发帖类工具
// 不能绕过 SpeakMode 第四条发言路径。
// ============================================================================

// setupBrowserMCP 为本 bot 接入 per-bot 浏览器 MCP 服务。
// 调用前须确保 wsMgr 为 docker 后端且 SandboxConfig.BrowserEnabled=true；其余前置检查在此兜底。
//
// 容器拉起、脚本同步和 MCP 握手都在后台进行。一次握手超时（慢启动的浏览器进程）
// 不会让本次进程永久失去浏览器工具，也不会挡住 channel 和其它工具的就绪。
func setupBrowserMCP(b *Bot, params BotParams, wsMgr *sandbox.BotWorkspaceManager) error {
	if wsMgr == nil || wsMgr.Backend() != "docker" {
		return nil
	}
	if !params.SandboxConfig.BrowserEnabled {
		return nil
	}
	if params.ToolManager == nil {
		return nil
	}
	if b.logger == nil {
		b.logger = zap.NewNop().Sugar()
	}

	redact := browserRedactList(params.SandboxConfig.BrowserProxy)
	browserMgr := mcp.NewManager(b.logger.With("component", "browser_mcp"))
	b.browserMCP = browserMgr
	b.browserCookieSaver = params.BrowserCookieSaver
	b.browserCookieStartupRecover = params.BrowserCookieStartupRecover
	b.browserBG = startBrowserMCP(browserMCPConfig{
		Manager: browserMgr,
		Tools:   params.ToolManager,
		Logger:  b.logger.With("component", "browser_mcp"),
		Redact:  redact,
		Prepare: func(ctx context.Context) (mcp.ServerConfig, error) {
			return prepareBrowserMCP(ctx, b, params, wsMgr, redact)
		},
	})
	b.logger.Infow("browser mcp scheduled", "bot_id", params.ID)
	return nil
}

// prepareBrowserMCP 在后台尝试里把容器和 cookie 准备好，再给出本次要拉起的 stdio 配置。
// 容器还没起来、docker 暂时不可用，都作为可重试的失败返回，不在 bot 启动路径上等待。
func prepareBrowserMCP(ctx context.Context, b *Bot, params BotParams, wsMgr *sandbox.BotWorkspaceManager, redact []string) (mcp.ServerConfig, error) {
	if err := wsMgr.StartBot(ctx, params.ID); err != nil {
		return mcp.ServerConfig{}, errs.Wrap(err, "bot: ensure browser container")
	}
	containerName := wsMgr.ContainerInfo(ctx, params.ID).ContainerName
	if containerName == "" {
		return mcp.ServerConfig{}, errs.New("bot: cannot resolve browser container name")
	}
	// 已存在的容器不会随内置镜像更新而重建：先把本二进制内嵌的新版 wrapper 同步进去。
	syncBrowserMCPScript(ctx, containerName, b.logger)
	deliverBrowserCookies(ctx, b, params, wsMgr)

	// 代理透传：部署侧自有出口（IP 归部署侧），空值直连。
	// 注意：docker exec 不继承 docker 客户端进程的 env，必须把代理以 `-e` 注入容器内。
	dockerArgs := []string{"exec", "-i", containerName}
	if params.SandboxConfig.BrowserProxy != "" {
		dockerArgs = append(dockerArgs, "-e", "BOT_BROWSER_PROXY="+params.SandboxConfig.BrowserProxy)
	}
	// 经 launch 脚本以非 root 用户运行 wrapper（见 Dockerfile / browser-launch.sh）。
	dockerArgs = append(dockerArgs, "/usr/local/bin/thinkbot-browser-launch")
	return mcp.ServerConfig{
		Name:      browserServerName,
		Transport: "stdio",
		Command:   "docker",
		Args:      dockerArgs,
		Enabled:   true,
		// 把 wrapper 的 stderr 回写 thinkbot 日志。先打码再落日志，避免代理口令进日志。
		Stderr:           &mcpStdioStderr{log: b.logger, redact: redact},
		HandshakeTimeout: browserHandshakeTimeout(params.SandboxConfig),
		Redact:           redact,
	}, nil
}

// browserHandshakeTimeout 取出沙箱配置里的握手时限。零值或越界回退到 3 分钟，
// 避免漏配时落回通用 MCP 的 8s。
func browserHandshakeTimeout(cfg sandbox.Config) time.Duration {
	if err := sandbox.ValidateBrowserHandshakeTimeout(cfg.BrowserHandshakeTimeout); err != nil {
		return sandbox.DefaultBrowserHandshakeTimeout
	}
	return cfg.BrowserHandshakeTimeout
}

// deliverBrowserCookies 在握手前回收上一轮残留 cookie，再把面板里的 cookie 写进容器。
// 浏览器进程在首次 tools/call 时才读状态文件，因此写在拉起进程之前即可。
func deliverBrowserCookies(ctx context.Context, b *Bot, params BotParams, wsMgr *sandbox.BotWorkspaceManager) {
	// 启动期回收：在 loader 覆盖写状态文件之前，先把容器内「上一轮残留」的 cookie 合并回 DB。
	// 非优雅关闭（SIGKILL/崩溃）时 Close() 未跑、cookie 仅落在 wrapper 周期 saveState 文件里，
	// 若不在此回收，下方 loader 会用（可能为空）的 DB 内容覆盖该文件，导致孤儿 cookie 永久丢失。
	// 优雅重启时文件==DB，此步幂等；DB 独有项（面板编辑）被保留。
	if params.BrowserCookieStartupRecover != nil {
		if ws, werr := wsMgr.GetOrCreate(params.ID); werr == nil {
			if data, rerr := ws.ReadFile(ctx, "/data/.browser-state.json"); rerr == nil && len(data) > 0 {
				// 仅当文件确实含 cookie 时才回收，避免空状态文件（{"cookies":[]}）误报。
				if bytes.Contains(data, []byte("\"name\"")) {
					// 成功日志（含 created/updated 条数）由回收实现打印，此处只记失败。
					if serr := params.BrowserCookieStartupRecover(ctx, data); serr != nil {
						b.logger.Warnw("browser cookie startup recover failed", "err", serr)
					}
				}
			}
		} else {
			b.logger.Warnw("browser workspace unavailable for startup recover", "err", werr)
		}
	}

	// 会话前投递 cookie：把 Web 面板管理的 cookie 注入容器内状态文件，
	// 浏览器进程首次 tools/call 时读取并 addCookies。
	if params.BrowserCookieLoader != nil {
		data, lerr := params.BrowserCookieLoader(ctx)
		if lerr != nil {
			b.logger.Warnw("browser cookie load failed", "err", lerr)
		} else if len(data) > 0 {
			ws, werr := wsMgr.GetOrCreate(params.ID)
			if werr != nil {
				b.logger.Warnw("browser workspace unavailable", "err", werr)
			} else if werr := ws.WriteFile(ctx, "/data/.browser-state.json", data); werr != nil {
				b.logger.Warnw("browser cookie deliver failed", "err", werr)
			} else {
				b.logger.Infow("browser cookies delivered", "bytes", len(data))
			}
		}
	}
}

// AlignBrowserDisplay moves this bot's browser onto DISPLAY=:99 when it is
// still on an older screen. The container is not recreated. The desktop
// session (openbox) is started by the RFB helper, not by launching Chromium.
// Custom images whose launcher does not share :99 are left alone.
func (b *Bot) AlignBrowserDisplay(ctx context.Context) {
	if b == nil || b.browserMCP == nil || b.workspaceMgr == nil {
		return
	}
	b.alignMu.Lock()
	defer b.alignMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	name := b.workspaceMgr.ContainerInfo(ctx, b.ID).ContainerName
	if name == "" {
		return
	}
	// Sync first so an older builtin launcher gains DISPLAY=:99 before the
	// share check. Custom images return without being rewritten.
	updated := syncBrowserMCPScript(ctx, name, b.logger)
	if !sandbox.ContainerLaunchSharesDisplay(ctx, name) {
		return
	}
	disp, err := sandbox.ContainerBrowserDisplay(ctx, name)
	if err != nil {
		b.logger.Warnw("browser display lookup failed", "err", err, "bot_id", b.ID)
	}
	onShared := disp == ":99" || strings.HasPrefix(disp, ":99.")
	if disp != "" && (updated || !onShared) {
		b.logger.Infow("restarting browser onto the shared display", "from", disp, "script_updated", updated, "bot_id", b.ID)
		if b.browserBG != nil {
			if err := b.browserBG.Restart(ctx); err != nil {
				b.logger.Warnw("browser restart onto shared display failed", "err", err)
			}
			return
		}
		if err := b.browserMCP.DisableServer("browser"); err != nil {
			b.logger.Warnw("browser disable before display align failed", "err", err)
		}
		if err := b.browserMCP.EnableServer(ctx, "browser"); err != nil {
			b.logger.Warnw("browser restart onto shared display failed", "err", err)
		}
	}
}
