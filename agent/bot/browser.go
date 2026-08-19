package bot

import (
	"context"
	"io"
	"strings"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/mcp"
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// mcpStdioStderr 把浏览器 wrapper 的 stderr 回写到 thinkbot 日志（便于排障）。
// wrapper 仅在此输出诊断信息（绝不打印 cookie 值），按行回写。
type mcpStdioStderr struct {
	log *zap.SugaredLogger
}

func (w *mcpStdioStderr) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if line != "" {
		w.log.Infow("browser-mcp stderr", "line", line)
	}
	return len(p), nil
}

var _ io.Writer = (*mcpStdioStderr)(nil)

// ============================================================================
// 浏览器 MCP 接线 — per-bot，运行在 bot 持久容器内（见 docs/sandbox-browser-image-design.md）
//
// 链路：thinkbot 主进程 → `docker exec -i thinkbot-bot-<id>` → 容器内
//       xvfb-run → node /usr/local/bin/thinkbot-browser-mcp（stdio JSON-RPC）→
//       容器内 headful chromium（patchright 驱动）。
//
// 工具命名遵循 MCP 框架约定 <server>__<tool>：本服务固定名为 "browser"，
// 故容器内工具即 browser__navigate 等，并已在
// toolperm/risk.go 的 broadcastPrefixes 登记 "browser__" 以约束发帖类工具
// 不能绕过 SpeakMode 第四条发言路径。
// ============================================================================

// setupBrowserMCP 为本 bot 接入 per-bot 浏览器 MCP 服务。
// 调用前须确保 wsMgr 为 docker 后端且 SandboxConfig.BrowserEnabled=true；其余前置检查在此兜底。
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

	ctx := context.Background()

	// 容器名是确定的（thinkbot-bot-<botID>），不依赖容器是否已被创建。
	containerName := wsMgr.ContainerInfo(ctx, params.ID).ContainerName
	if containerName == "" {
		return errs.New("bot: cannot resolve browser container name")
	}

	// 确保容器已运行：浏览器 server 经 docker exec 起进程，容器不存在则 exec 失败。
	if err := wsMgr.StartBot(ctx, params.ID); err != nil {
		return errs.Wrap(err, "bot: ensure browser container")
	}

	browserMgr := mcp.NewManager(b.logger.With("component", "browser_mcp"))
	// 代理透传：部署侧自有出口（IP 归部署侧），空值直连。
	// 注意：docker exec 不继承 docker 客户端进程的 env，必须把代理以 `-e` 注入容器内。
	dockerArgs := []string{"exec", "-i", containerName}
	if params.SandboxConfig.BrowserProxy != "" {
		dockerArgs = append(dockerArgs, "-e", "BOT_BROWSER_PROXY="+params.SandboxConfig.BrowserProxy)
	}
	// 经 launch 脚本以非 root 用户运行 wrapper（见 Dockerfile / browser-launch.sh）。
	dockerArgs = append(dockerArgs, "/usr/local/bin/thinkbot-browser-launch")
	cfg := mcp.ServerConfig{
		Name:      "browser",
		Transport: "stdio",
		Command:   "docker",
		Args:      dockerArgs,
		Enabled:   true,
		// 把 wrapper 的 stderr 回写 thinkbot 日志，否则子进程错误被静默丢弃，排障困难。
		Stderr: &mcpStdioStderr{log: b.logger},
	}
	browserMgr.AddServer(cfg)

	// EnableServer 立即连接（docker exec 起进程 + MCP 握手）。失败则浏览器工具不可用，
	// 调用方会降级处理（不致命）。
	if err := browserMgr.EnableServer(ctx, "browser"); err != nil {
		return errs.Wrap(err, "bot: enable browser mcp server")
	}

	// 把浏览器工具注册进本 bot 的 ToolManager（per-bot，互不串台）。
	if err := mcp.RegisterTools(params.ToolManager, browserMgr); err != nil {
		return errs.Wrap(err, "bot: register browser mcp tools")
	}

	b.browserMCP = browserMgr
	b.logger.Infow("browser mcp wired", "container", containerName, "bot_id", params.ID)

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

	return nil
}
