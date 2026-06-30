package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"

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
// 请求体: { "cmd": "ls -la" }
// 响应:   { "output": "...", "cwd": "/home/user" }
func (s *Server) handleSessionTerminalExec(c *gin.Context) {
	sid := c.Param("sid")
	var req struct {
		Cmd string `json:"cmd" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("cmd is required"))
		return
	}

	// 目前终端执行是 stub 实现——记录命令并返回模拟结果。
	// 完整实现需要集成 PTY 或 Docker exec。
	result := gin.H{
		"output": fmt.Sprintf("[session %s] command received: %s\n(terminal exec not yet implemented)", sid, req.Cmd),
		"cwd":    "/",
	}

	auditLog(c, s.logger, "session_terminal_exec", "session", sid, "cmd", req.Cmd)
	OK(c, result)
}

// --- 文件浏览 API ---

// FileEntry 文件条目。
type FileEntry struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"` // "dir" | "file"
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
}

// handleSessionFiles 列出会话工作区文件。
// GET /api/sessions/:sid/files?path=/
func (s *Server) handleSessionFiles(c *gin.Context) {
	sid := c.Param("sid")
	path := c.DefaultQuery("path", "/")

	files := s.getSessionFiles(sid, path)
	OK(c, gin.H{
		"path":    path,
		"entries": files,
	})
}

// handleSessionFileMkdir 在会话工作区创建目录。
// POST /api/sessions/:sid/files/mkdir
//
// 请求体: { "path": "/workspace", "name": "new_dir" }
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

	auditLog(c, s.logger, "session_file_mkdir", "session", sid, "path", req.Path, "name", req.Name)
	OK(c, gin.H{"ok": true})
}

// handleSessionFileUpload 上传文件到会话工作区。
// POST /api/sessions/:sid/files/upload
//
// 请求体: { "path": "/workspace", "name": "file.txt", "size": 1024 }
func (s *Server) handleSessionFileUpload(c *gin.Context) {
	sid := c.Param("sid")
	var req struct {
		Path string `json:"path" binding:"required"`
		Name string `json:"name" binding:"required"`
		Size int64  `json:"size"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("path and name are required"))
		return
	}

	auditLog(c, s.logger, "session_file_upload", "session", sid, "path", req.Path, "name", req.Name, "size", req.Size)
	OK(c, gin.H{"ok": true})
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

func (s *Server) getSessionFiles(sid, path string) []FileEntry {
	key := sessionToolKey(sid, "files")
	raw, ok := s.store.Get(key)
	if !ok || raw == "" {
		return []FileEntry{}
	}
	// 存储为 map[path][]FileEntry
	var fileMap map[string][]FileEntry
	if err := json.Unmarshal([]byte(raw), &fileMap); err != nil {
		return []FileEntry{}
	}
	entries, ok := fileMap[path]
	if !ok {
		return []FileEntry{}
	}
	return entries
}

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
