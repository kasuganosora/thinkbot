package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// Bot MCP 服务器管理 Handler
//
// 每个 Bot 独立管理 MCP 服务器配置，存储在 data/mcp/{botId}/servers.json。
//
// 路由：
//   GET    /api/bots/:id/mcp              → 列表
//   POST   /api/bots/:id/mcp              → 新增
//   PUT    /api/bots/:id/mcp/:mid         → 更新
//   DELETE /api/bots/:id/mcp/:mid         → 删除
//   POST   /api/bots/:id/mcp/import       → 批量导入（JSON 配置）
// ============================================================================

// mcpServerEntry 是返回给前端的 MCP 服务器实体。
type mcpServerEntry struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Type      string            `json:"type"`    // "stdio" | "http" | "sse"
	Command   string            `json:"command"` // stdio
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Cwd       string            `json:"cwd"`
	URL       string            `json:"url"`     // http/sse
	Headers   map[string]string `json:"headers"` // http/sse
	Enabled   bool              `json:"enabled"`
	Status    string            `json:"status"` // "draft" | "running" | "disabled" | "error"
	CreatedAt string            `json:"createdAt"`
	UpdatedAt string            `json:"updatedAt"`
}

// mcpStoreFile 返回 Bot 的 MCP 服务器配置文件路径。
func mcpStoreFile(botID string) string {
	return filepath.Join("data", "mcp", botID, "servers.json")
}

// loadMcpServers 从文件加载 MCP 服务器列表。
func loadMcpServers(botID string) ([]mcpServerEntry, error) {
	path := mcpStoreFile(botID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var servers []mcpServerEntry
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, err
	}
	return servers, nil
}

// saveMcpServers 将 MCP 服务器列表保存到文件。
func saveMcpServers(botID string, servers []mcpServerEntry) error {
	path := mcpStoreFile(botID)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(servers, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// handleListBotMcp 列出指定 Bot 的 MCP 服务器。
func (s *Server) handleListBotMcp(c *gin.Context) {
	botID := c.Param("id")
	servers, err := loadMcpServers(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load mcp servers"))
		return
	}
	if servers == nil {
		servers = []mcpServerEntry{}
	}
	OK(c, gin.H{"servers": servers})
}

// handleCreateBotMcp 创建一个 MCP 服务器配置。
func (s *Server) handleCreateBotMcp(c *gin.Context) {
	botID := c.Param("id")
	var req struct {
		Name    string            `json:"name"`
		Type    string            `json:"type"`
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Env     map[string]string `json:"env"`
		Cwd     string            `json:"cwd"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Enabled *bool             `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}

	servers, _ := loadMcpServers(botID)

	now := time.Now().UTC().Format(time.RFC3339)
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if req.Type == "" {
		if req.URL != "" {
			req.Type = "http"
		} else {
			req.Type = "stdio"
		}
	}
	if req.Args == nil {
		req.Args = []string{}
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}

	entry := mcpServerEntry{
		ID:        idgen.New("mcp"),
		Name:      req.Name,
		Type:      req.Type,
		Command:   req.Command,
		Args:      req.Args,
		Env:       req.Env,
		Cwd:       req.Cwd,
		URL:       req.URL,
		Headers:   req.Headers,
		Enabled:   enabled,
		Status:    "draft",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if entry.Name == "" {
		entry.Name = "未命名"
	}

	servers = append(servers, entry)
	if err := saveMcpServers(botID, servers); err != nil {
		Fail(c, errs.Wrap(err, "save mcp servers"))
		return
	}

	auditLog(c, s.logger, "create_bot_mcp", "bot_id", botID, "mcp", entry.Name)
	OK(c, entry)
}

// handleUpdateBotMcp 更新 MCP 服务器配置。
func (s *Server) handleUpdateBotMcp(c *gin.Context) {
	botID := c.Param("id")
	mid := c.Param("mid")

	var req map[string]any
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}

	servers, err := loadMcpServers(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load mcp servers"))
		return
	}

	idx := -1
	for i, sv := range servers {
		if sv.ID == mid {
			idx = i
			break
		}
	}
	if idx < 0 {
		Fail(c, errs.NotFound("mcp server not found"))
		return
	}

	// 应用更新
	sv := &servers[idx]
	if v, ok := req["name"].(string); ok {
		sv.Name = v
	}
	if v, ok := req["type"].(string); ok {
		sv.Type = v
	}
	if v, ok := req["command"].(string); ok {
		sv.Command = v
	}
	if v, ok := req["args"]; ok {
		if args, ok := v.([]any); ok {
			sv.Args = make([]string, 0, len(args))
			for _, a := range args {
				if s, ok := a.(string); ok {
					sv.Args = append(sv.Args, s)
				}
			}
		}
	}
	if v, ok := req["env"]; ok {
		if env, ok := v.(map[string]any); ok {
			sv.Env = make(map[string]string, len(env))
			for k, val := range env {
				if s, ok := val.(string); ok {
					sv.Env[k] = s
				}
			}
		}
	}
	if v, ok := req["cwd"].(string); ok {
		sv.Cwd = v
	}
	if v, ok := req["url"].(string); ok {
		sv.URL = v
	}
	if v, ok := req["headers"]; ok {
		if headers, ok := v.(map[string]any); ok {
			sv.Headers = make(map[string]string, len(headers))
			for k, val := range headers {
				if s, ok := val.(string); ok {
					sv.Headers[k] = s
				}
			}
		}
	}
	if v, ok := req["enabled"].(bool); ok {
		sv.Enabled = v
	}

	// 根据 enabled 和配置变更更新 status
	if sv.Enabled {
		sv.Status = "running"
	} else {
		sv.Status = "disabled"
	}

	sv.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if err := saveMcpServers(botID, servers); err != nil {
		Fail(c, errs.Wrap(err, "save mcp servers"))
		return
	}

	auditLog(c, s.logger, "update_bot_mcp", "bot_id", botID, "mcp_id", mid)
	OK(c, *sv)
}

// handleRemoveBotMcp 删除 MCP 服务器。
func (s *Server) handleRemoveBotMcp(c *gin.Context) {
	botID := c.Param("id")
	mid := c.Param("mid")

	servers, err := loadMcpServers(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load mcp servers"))
		return
	}

	found := false
	for i, sv := range servers {
		if sv.ID == mid {
			servers = append(servers[:i], servers[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		Fail(c, errs.NotFound("mcp server not found"))
		return
	}

	if err := saveMcpServers(botID, servers); err != nil {
		Fail(c, errs.Wrap(err, "save mcp servers"))
		return
	}

	auditLog(c, s.logger, "remove_bot_mcp", "bot_id", botID, "mcp_id", mid)
	OK(c, nil)
}

// handleImportBotMcp 批量导入 MCP 服务器（JSON 配置格式）。
// 请求体格式：{ "mcpServers": { "name": { "command": ..., "args": [...], ... } } }
func (s *Server) handleImportBotMcp(c *gin.Context) {
	botID := c.Param("id")
	var req struct {
		McpServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}

	servers, _ := loadMcpServers(botID)
	now := time.Now().UTC().Format(time.RFC3339)

	var created []mcpServerEntry
	for name, raw := range req.McpServers {
		var cfg struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			Cwd     string            `json:"cwd"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			continue
		}

		tp := cfg.Type
		if tp == "" {
			if cfg.URL != "" {
				if strings.Contains(strings.ToLower(string(raw)), "sse") {
					tp = "sse"
				} else {
					tp = "http"
				}
			} else {
				tp = "stdio"
			}
		}

		if cfg.Args == nil {
			cfg.Args = []string{}
		}
		if cfg.Env == nil {
			cfg.Env = map[string]string{}
		}
		if cfg.Headers == nil {
			cfg.Headers = map[string]string{}
		}

		entry := mcpServerEntry{
			ID:        idgen.New("mcp"),
			Name:      name,
			Type:      tp,
			Command:   cfg.Command,
			Args:      cfg.Args,
			Env:       cfg.Env,
			Cwd:       cfg.Cwd,
			URL:       cfg.URL,
			Headers:   cfg.Headers,
			Enabled:   true,
			Status:    "draft",
			CreatedAt: now,
			UpdatedAt: now,
		}
		servers = append(servers, entry)
		created = append(created, entry)
	}

	if err := saveMcpServers(botID, servers); err != nil {
		Fail(c, errs.Wrap(err, "save mcp servers"))
		return
	}

	auditLog(c, s.logger, "import_bot_mcp", "bot_id", botID, "count", fmt.Sprintf("%d", len(created)))
	OK(c, gin.H{"servers": created})
}
