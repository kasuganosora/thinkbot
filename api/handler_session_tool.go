package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/stats"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// compactLLMTimeout 手动 /compact 触发记忆压缩时给 LLM 调用的预算。
// 必须与自动路径 maybeCompact 的 compactionLLMTimeout（5min）口径一致——
// 不能用 HTTP 请求上下文（gin 默认 ~120s 会掐断慢速 GLM 首字）。
const compactLLMTimeout = 5 * time.Minute

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
	Cwd       string        `json:"cwd"`
	Tabs      []TerminalTab `json:"tabs"`
}

// handleSessionTerminal 获取会话终端状态。
// GET /api/sessions/:sid/terminal
func (s *Server) handleSessionTerminal(c *gin.Context) {
	sid := c.Param("sid")

	state := s.getSessionTerminalState(sid)
	// session ID 即 bot ID（1:1 映射），补全真实工作目录（如 /data）。
	if s.botSvc != nil {
		if ws, err := s.botSvc.ResolveWorkspace(sid); err == nil {
			state.Cwd = ws.WorkDir()
		}
	}
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
	defer func() { _ = f.Close() }()

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
//
// 手动触发指定 bot 的记忆压缩（带 pre-LLM 预压缩的生产路径）。
// 可选 query 参数 scope（格式 kind:id，如 channel:web:123 / bot:xyz / global）：
// 指定则只压该 scope；不传则压该 bot 的主作用域（channel:<sid> + bot:<sid>）。
// 若 bot 未运行则返回明确提示，不再假成功。
func (s *Server) handleSessionCompact(c *gin.Context) {
	sid := c.Param("sid")

	if s.botSvc == nil {
		Fail(c, errs.Internal("bot service unavailable"))
		return
	}
	memRepo, ok := s.botSvc.GetMemoryRepo(sid)
	if !ok {
		Fail(c, errs.BadRequest("bot 未运行，无法压缩记忆"))
		return
	}

	scopes, err := resolveCompactScopes(sid, c.Query("scope"))
	if err != nil {
		Fail(c, errs.BadRequest("invalid scope: "+err.Error()))
		return
	}
	scopeKeys := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		scopeKeys = append(scopeKeys, fmt.Sprintf("%s:%s", sc.Kind, sc.ID))
	}

	// 异步触发压缩：压缩可能耗时数分钟（每批 GLM 聚类合并上限见
	// sqlite_compactor.compactLLMTimeout），若同步等待会拖垮 HTTP 请求——
	// 此前曾因此让 curl 在 6min 处超时（HTTP_STATUS:000）。改为后台 goroutine
	// 执行，与自动路径 maybeCompact 的「尽力而为、异步维护」语义一致。
	go func() {
		runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		for _, sc := range scopes {
			if cerr := memRepo.CompactScope(runCtx, sc); cerr != nil {
				s.logger.Warnw("api compact: scope failed", "scope", sc.Key(), "err", cerr)
				continue
			}
			s.logger.Infow("api compact: scope done", "scope", sc.Key())
		}
	}()

	auditLog(c, s.logger, "session_compact", "session", sid)
	OK(c, gin.H{"ok": true, "message": "已触发记忆压缩（后台执行）", "scopes": scopeKeys})
}

// resolveCompactScopes 解析压缩范围：scope 参数为空时返回该 bot 的主作用域
// （channel:<sid> + bot:<sid>）；否则按 kind:id 解析单个 scope。
func resolveCompactScopes(botID, scopeParam string) ([]memory.Scope, error) {
	if scopeParam == "" {
		return []memory.Scope{
			memory.ChannelScope(botID),
			memory.BotScope(botID),
		}, nil
	}
	sc, err := parseScopeParam(scopeParam)
	if err != nil {
		return nil, err
	}
	return []memory.Scope{sc}, nil
}

// parseScopeParam 解析 "kind:id" 形式的 scope 参数。
func parseScopeParam(s string) (memory.Scope, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return memory.Scope{}, fmt.Errorf("expected kind:id")
	}
	kind := memory.ScopeKind(parts[0])
	id := parts[1]
	switch kind {
	case memory.ScopeChannel, memory.ScopeUser, memory.ScopeBot:
		if id == "" {
			return memory.Scope{}, fmt.Errorf("id required for %s", kind)
		}
		return memory.Scope{Kind: kind, ID: id}, nil
	case memory.ScopeGlobal:
		return memory.Scope{Kind: kind, ID: ""}, nil
	default:
		return memory.Scope{}, fmt.Errorf("unknown scope kind %q", parts[0])
	}
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

// getSessionStatus 组装会话状态：数据全部来自真实来源。
//   - Messages:  ChatHistory 中该 bot 的消息总数
//   - Cache*:    stats_usage_daily 聚合的真实缓存命中/读写统计
//   - ContextUsed: 该 bot 累计消耗的 token 总量（真实）
//   - Skills:    会话级 skill 使用记录（暂无来源时返回空数组）
//
// session ID 即 bot ID（1:1 映射）。
func (s *Server) getSessionStatus(sid string) SessionToolStatus {
	status := SessionToolStatus{Skills: []string{}}

	// 消息数（真实，来自 chat_history）
	if s.chatHistory != nil {
		if n, err := s.chatHistory.CountMessages(sid); err != nil {
			s.logger.Warnw("session status: count messages failed", "sid", sid, "err", err)
		} else {
			status.Messages = int(n)
		}
	}

	// 缓存与 token 统计（真实，来自 stats_usage_daily 聚合）
	if s.db != nil {
		rows, err := stats.GetBotModelStats(s.db, sid, nil, nil)
		if err != nil {
			s.logger.Warnw("session status: query stats failed", "sid", sid, "err", err)
		} else {
			var totalReq, hitReq, cacheRead, cacheWrite, totalTokens int
			for _, r := range rows {
				totalReq += r.TotalRequests
				hitReq += r.CacheHitRequests
				cacheRead += r.CacheReadTokens
				cacheWrite += r.CacheWriteTokens
				totalTokens += r.TotalTokens
			}
			if totalReq > 0 {
				status.CacheHitRate = float64(hitReq) / float64(totalReq)
			}
			status.CacheRead = cacheRead
			status.CacheWrite = cacheWrite
			status.ContextUsed = totalTokens
		}
	}

	// 上下文上限：会话级实时窗口上限暂无稳定来源，保持 nil（前端显示 "--"），避免造假。
	status.ContextLimit = nil

	return status
}
