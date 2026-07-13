package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Session Tool Handler — 会话级工具 API
//
// 前端契约（sessionToolApi）：
//   GET  /api/sessions/:sid/terminal       — 终端状态
//   POST /api/sessions/:sid/terminal/exec  — 执行命令
//   GET  /api/sessions/:sid/files          — 文件列表
//   POST /api/sessions/:sid/files/mkdir    — 创建目录
//   POST /api/sessions/:sid/files/upload   — 上传文件
//   GET  /api/sessions/:sid/status         — 会话状态
//   POST /api/sessions/:sid/compact        — 压缩上下文
// ============================================================================

// --- 终端 API ---

// TerminalTab 终端标签页。
type TerminalTab struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

// TerminalState 终端状态。
type TerminalState struct {
	Host      string        `json:"host"`
	Connected bool          `json:"connected"`
	Tabs      []TerminalTab `json:"tabs"`
}

// handleSessionTerminal 获取会话终端状态。
// GET /api/sessions/:sid/terminal
func (s *Server) handleSessionTerminal(c *gin.Context) {
	sid := c.Param("sid")

	state := s.getSessionTerminalState(sid)
	OK(c, state)
}

// handleSessionTerminalExec 在会话终端中执行命令。
// POST /api/sessions/:sid/terminal/exec
//
// 请求体: { "cmd": "ls -la", "cwd": "sub/dir" }
// 响应:   { "output": "...", "exitCode": 0, "cwd": "..." }
//
// 执行路由完全交给 sandbox 兼容层（Workspace.Exec）：有 Docker 环境则在该 bot 的
// 隔离容器内执行，无 Docker 则在宿主机本地进程执行，本 handler 不感知具体后端。
func (s *Server) handleSessionTerminalExec(c *gin.Context) {
	sid := c.Param("sid")
	var req struct {
		Cmd string `json:"cmd" binding:"required"`
		Cwd string `json:"cwd"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("cmd is required"))
		return
	}

	if s.botSvc == nil {
		Fail(c, errs.Internal("bot service unavailable"))
		return
	}
	// session ID 即 bot ID（当前设计下 session 与 bot 1:1 映射）。
	ws, err := s.botSvc.ResolveWorkspace(sid)
	if err != nil {
		Fail(c, errs.Internal("resolve workspace: "+err.Error()))
		return
	}

	res, err := ws.Exec(c.Request.Context(), sandbox.ExecRequest{
		Command: req.Cmd,
		WorkDir: req.Cwd,
	})
	if err != nil {
		Fail(c, errs.Internal("exec failed: "+err.Error()))
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

	auditLog(c, s.logger, "session_terminal_exec", "session", sid, "cmd", req.Cmd)
	OK(c, gin.H{
		"output":    output,
		"exitCode":  res.ExitCode,
		"truncated": res.Truncated,
		"cwd":       ws.WorkDir(),
	})
}

// --- 文件浏览 API ---
// Session Files 代理到 Bot Files — 通过 session 对应的 bot 查找实际文件数据。
// 如果 session 无法映射到 bot，返回空列表。

// handleSessionFiles 列出会话工作区文件（代理到 Bot 文件存储）。
// GET /api/sessions/:sid/files?path=/
func (s *Server) handleSessionFiles(c *gin.Context) {
	sid := c.Param("sid")
	path := c.DefaultQuery("path", "/")

	// session ID 即 bot ID（当前设计下 session 与 bot 1:1 映射）
	entries := s.getBotFileEntries(sid, path)
	OK(c, gin.H{
		"path":    path,
		"entries": entries,
	})
}

// handleSessionFileMkdir 在会话工作区创建目录（代理到 Bot 文件存储）。
// POST /api/sessions/:sid/files/mkdir
func (s *Server) handleSessionFileMkdir(c *gin.Context) {
	sid := c.Param("sid")
	var req struct {
		Path string `json:"path" binding:"required"`
		Name string `json:"name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("path and name are required"))
		return
	}

	if err := s.botFileMkdir(c, sid, req.Path, req.Name); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "session_file_mkdir", "session", sid, "path", req.Path, "name", req.Name)
	OK(c, gin.H{"ok": true})
}

// handleSessionFileUpload 上传文件到会话工作区（代理到 Bot 文件存储）。
// POST /api/sessions/:sid/files/upload
// 接收 multipart/form-data：字段 file（文件）、path（目标路径）。
func (s *Server) handleSessionFileUpload(c *gin.Context) {
	sid := c.Param("sid")

	fileHeader, err := c.FormFile("file")
	if err != nil {
		Fail(c, errs.BadRequest("file is required: "+err.Error()))
		return
	}
	path := c.PostForm("path")
	if path == "" {
		path = "/"
	}

	f, err := fileHeader.Open()
	if err != nil {
		Fail(c, errs.Internal("failed to open uploaded file: "+err.Error()))
		return
	}
	defer f.Close()

	if err := s.botFileUpload(c, sid, path, fileHeader.Filename, f); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "session_file_upload", "session", sid, "path", path, "name", fileHeader.Filename, "size", fileHeader.Size)
	OK(c, gin.H{"ok": true})
}

// handleSessionFileDownload 下载会话工作区文件（代理到 Bot 文件存储）。
// GET /api/sessions/:sid/files/download?path=/xxx
func (s *Server) handleSessionFileDownload(c *gin.Context) {
	sid := c.Param("sid")
	s.serveBotFileDownload(c, sid, c.Query("path"))
}

// --- 会话状态 API ---

// SessionStatus 会话状态信息。
type SessionToolStatus struct {
	Messages     int      `json:"messages"`
	ContextUsed  int      `json:"contextUsed"`
	ContextLimit *int     `json:"contextLimit"` // nil = 无限制
	CacheHitRate float64  `json:"cacheHitRate"`
	CacheRead    int      `json:"cacheRead"`
	CacheWrite   int      `json:"cacheWrite"`
	Skills       []string `json:"skills"`
}

// handleSessionStatus 获取会话状态。
// GET /api/sessions/:sid/status
func (s *Server) handleSessionStatus(c *gin.Context) {
	sid := c.Param("sid")

	status := s.getSessionStatus(sid)
	OK(c, status)
}

// handleSessionCompact 压缩会话上下文。
// POST /api/sessions/:sid/compact
func (s *Server) handleSessionCompact(c *gin.Context) {
	sid := c.Param("sid")

	auditLog(c, s.logger, "session_compact", "session", sid)
	OK(c, gin.H{"ok": true})
}

// ============================================================================
// Config Store 辅助方法 — session 工具数据持久化
// ============================================================================

func sessionToolKey(sid, sub string) string {
	return "session." + sid + ".tool." + sub
}

func (s *Server) getSessionTerminalState(sid string) TerminalState {
	raw, ok := s.store.Get(sessionToolKey(sid, "terminal"))
	if !ok || raw == "" {
		// 默认状态：连接且有一个默认标签页
		return TerminalState{
			Host:      "localhost",
			Connected: true,
			Tabs: []TerminalTab{
				{ID: "default", Name: "Terminal", Active: true},
			},
		}
	}
	var state TerminalState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return TerminalState{Host: "localhost", Connected: false, Tabs: []TerminalTab{}}
	}
	return state
}

// getSessionFiles 已废弃 — Session Files 现在代理到 Bot Files（getBotFileEntries）。

func (s *Server) getSessionStatus(sid string) SessionToolStatus {
	raw, ok := s.store.Get(sessionToolKey(sid, "status"))
	if !ok || raw == "" {
		return SessionToolStatus{
			Messages:     0,
			ContextUsed:  0,
			ContextLimit: nil,
			CacheHitRate: 0,
			CacheRead:    0,
			CacheWrite:   0,
			Skills:       []string{},
		}
	}
	var status SessionToolStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		return SessionToolStatus{Skills: []string{}}
	}
	return status
}
