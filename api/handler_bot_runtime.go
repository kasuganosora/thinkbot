package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Bot 运行时检查（概览页）— 接入真实 sandbox 状态
//
// 前端「运行时检查」列表此前为硬编码假数据。本接口用真实的 sandbox
// HealthCheck / ContainerInfo 组装容器相关检查项，反映底层容器/后端的真实状态。
//   GET /api/bots/:id/runtime-checks
// ============================================================================

// RuntimeCheck 单条运行时检查项。
type RuntimeCheck struct {
	Name    string `json:"name"`
	Sub     string `json:"sub,omitempty"`
	Message string `json:"message"`
	Extra   string `json:"extra,omitempty"`
	OK      bool   `json:"ok"`
	Mono    bool   `json:"mono,omitempty"`
}

// RuntimeChecksResp 运行时检查响应。
type RuntimeChecksResp struct {
	HasError bool           `json:"hasError"`
	Backend  string         `json:"backend"`
	Checks   []RuntimeCheck `json:"checks"`
}

// handleBotRuntimeChecks 返回指定 bot 的真实运行时检查项。
// GET /api/bots/:id/runtime-checks
func (s *Server) handleBotRuntimeChecks(c *gin.Context) {
	botID := c.Param("id")
	if s.botSvc == nil {
		Fail(c, fmt.Errorf("bot service unavailable"))
		return
	}

	ctx := c.Request.Context()
	checks := s.buildContainerChecks(ctx, botID)

	hasError := false
	for _, ck := range checks {
		if !ck.OK {
			hasError = true
			break
		}
	}

	backend := ""
	if mgr, err := s.botSvc.WorkspaceManagerForBot(botID); err == nil {
		backend = mgr.Backend()
	}

	OK(c, RuntimeChecksResp{
		HasError: hasError,
		Backend:  backend,
		Checks:   checks,
	})
}

// buildContainerChecks 用真实 sandbox 状态组装容器相关检查项。
func (s *Server) buildContainerChecks(ctx context.Context, botID string) []RuntimeCheck {
	mgr, err := s.botSvc.WorkspaceManagerForBot(botID)
	if err != nil {
		return []RuntimeCheck{{
			Name: "容器初始化", Message: "无法解析工作空间管理器：" + err.Error(), OK: false,
		}}
	}

	// 触发工作空间实例化，使 ContainerInfo 能拿到真实容器名/状态。
	if _, err := mgr.GetOrCreate(botID); err != nil {
		return []RuntimeCheck{{
			Name: "容器初始化", Message: "工作空间创建失败：" + err.Error(), OK: false,
		}}
	}

	info := mgr.ContainerInfo(ctx, botID)
	health := mgr.HealthCheck(ctx, botID)

	checks := make([]RuntimeCheck, 0, 4)

	// 1) 容器初始化
	initOK := info.Created
	initMsg := "工作空间已初始化。"
	if !initOK {
		initMsg = "工作空间尚未初始化。"
	}
	checks = append(checks, RuntimeCheck{
		Name: "容器初始化", Message: initMsg,
		Extra: "backend=" + info.Backend, OK: initOK,
	})

	// 2) 容器记录
	if info.Backend == "docker" && info.Persistent {
		recOK := info.ContainerName != ""
		recExtra := "container=" + info.ContainerName
		if info.Volume != "" {
			recExtra += "  volume=" + info.Volume
		}
		msg := "容器记录存在（docker 持久容器）。"
		if !recOK {
			msg = "容器记录缺失。"
		}
		checks = append(checks, RuntimeCheck{
			Name: "容器记录", Message: msg, Extra: recExtra, OK: recOK,
		})
	} else {
		checks = append(checks, RuntimeCheck{
			Name: "容器记录", Message: "本地模式，无独立容器（命令在宿主进程执行）。",
			Extra: "backend=" + info.Backend, OK: true,
		})
	}

	// 3) 容器任务 / 运行状态
	checks = append(checks, buildContainerTaskCheck(info, health))

	// 4) 容器数据路径
	pathOK := info.WorkDir != ""
	pathExtra := "workDir=" + info.WorkDir
	if info.Volume != "" {
		pathExtra = "volume=" + info.Volume + "  " + pathExtra
	}
	checks = append(checks, RuntimeCheck{
		Name: "容器数据路径", Message: health.Message,
		Extra: pathExtra, OK: pathOK && health.Healthy,
		Mono: true,
	})

	return checks
}

// buildContainerTaskCheck 根据真实状态生成「容器任务」检查项。
func buildContainerTaskCheck(info sandbox.ContainerInfo, health sandbox.HealthStatus) RuntimeCheck {
	state := info.State
	switch state {
	case "running":
		return RuntimeCheck{
			Name: "容器任务", Message: "容器正在运行。",
			Extra: "status=running", OK: true,
		}
	case "local":
		return RuntimeCheck{
			Name: "容器任务", Message: "本地进程模式，随命令按需执行。",
			Extra: "status=local", OK: true,
		}
	case "":
		return RuntimeCheck{
			Name: "容器任务", Message: "容器将在首次使用时创建。",
			Extra: "status=not-created", OK: true,
		}
	case "docker-unavailable":
		return RuntimeCheck{
			Name: "容器任务", Message: "Docker 守护进程不可用。",
			Extra: "status=docker-unavailable", OK: false,
		}
	default:
		return RuntimeCheck{
			Name: "容器任务", Message: fmt.Sprintf("容器状态：%s（将按需启动）。", state),
			Extra: "status=" + state, OK: health.Healthy,
		}
	}
}

// ============================================================================
// Bot 终端（容器 shell）— 接入真实 sandbox
//
// 前端契约（botTerminalApi）：
//   GET  /api/bots/:id/terminal       — 终端状态（连接信息 / 提示符）
//   POST /api/bots/:id/terminal/exec  — 执行命令（真实 docker exec 或本地进程）
//
// 参考实现：session 终端 (handler_session_tool.go) 的 handleSessionTerminal /
// handleSessionTerminalExec，核心模式完全一致：ResolveWorkspace → ws.Exec。
// 区别：session 用 sid（= bot ID），这里直接用 botId。
// ============================================================================

// handleBotTerminal 获取 bot 终端连接状态。
// GET /api/bots/:id/terminal
func (s *Server) handleBotTerminal(c *gin.Context) {
	botID := c.Param("id")
	if s.botSvc == nil {
		Fail(c, fmt.Errorf("bot service unavailable"))
		return
	}

	ws, err := s.botSvc.ResolveWorkspace(botID)
	if err != nil {
		// workspace 不可用时不阻断：返回未连接状态让前端显示提示
		OK(c, gin.H{
			"host":      "unavailable",
			"cwd":       "",
			"connected": false,
			"banner":    "无法解析工作空间：" + err.Error(),
		})
		return
	}

	banner := fmt.Sprintf("Connected to container of bot %s (%s)", botID, ws.WorkDir())
	OK(c, gin.H{
		"host":      fmt.Sprintf("root@%s", botID),
		"cwd":       ws.WorkDir(),
		"connected": true,
		"banner":    banner,
	})
}

// handleBotTerminalExec 在 bot 终端中执行命令。
// POST /api/bots/:id/terminal/exec
//
// 请求体: { "cmd": "ls -la", "cwd": "/data/sub" }
// 响应:   { "output": "...", "cwd": "..." }
//
// 执行路由完全交给 sandbox 兼容层（Workspace.Exec）：有 Docker 则在 bot 的
// 隔离容器内执行，无 Docker 则在本地进程执行，本 handler 不感知具体后端。
func (s *Server) handleBotTerminalExec(c *gin.Context) {
	botID := c.Param("id")
	var req struct {
		Cmd string `json:"cmd" binding:"required"`
		Cwd string `json:"cwd"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("cmd is required"))
		return
	}

	if s.botSvc == nil {
		Fail(c, fmt.Errorf("bot service unavailable"))
		return
	}

	ws, err := s.botSvc.ResolveWorkspace(botID)
	if err != nil {
		Fail(c, fmt.Errorf("resolve workspace: %w", err))
		return
	}

	res, err := ws.Exec(c.Request.Context(), sandbox.ExecRequest{
		Command: req.Cmd,
		WorkDir: req.Cwd,
	})
	if err != nil {
		Fail(c, fmt.Errorf("exec failed: %w", err))
		return
	}

	// 组装终端输出：stdout + stderr，末尾附非零退出码提示。
	output := res.Stdout
	if res.Stderr != "" {
		output += res.Stderr
	}
	if res.ExitCode != 0 {
		if output != "" && !strings.HasSuffix(output, "\n") {
			output += "\n"
		}
		output += fmt.Sprintf("[exit code: %d]\n", res.ExitCode)
	}

	auditLog(c, s.logger, "bot_terminal_exec", "bot", botID, "cmd", req.Cmd)
	OK(c, gin.H{
		"output": output,
		"cwd":    ws.WorkDir(),
	})
}
