package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/mcp"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// Bot MCP 服务器管理 Handler
//
// 每个 Bot 独立管理 MCP 服务器配置，存储在 data/mcp/{botId}/servers.json。
// 写入后若 Bot 正在运行，立即 UpsertServer / RemoveServer，使工具列表热刷新
//（不再需要重启）。
//
// 路由：
//   GET    /api/bots/:id/mcp              → 列表
//   POST   /api/bots/:id/mcp              → 新增
//   PUT    /api/bots/:id/mcp/:mid         → 更新
//   DELETE /api/bots/:id/mcp/:mid         → 删除
//   POST   /api/bots/:id/mcp/import       → 批量导入（JSON 配置）
// ============================================================================

// mcpSecretMask 是敏感 env/header 值经读取接口返回前端时的掩码占位串。
//
// 契约（与前端一致）：前端把整份配置回传做更新时，若某敏感字段仍是该串，
// 后端判定为「用户未改动」，沿用已存的真实值，不覆盖。
// 采用固定串而非「按原值长度打码」，是因为本 handler 的更新是 map 整体覆盖，
// 无法像指针字段那样区分「字段未传」与「传了」——只有固定哨兵能被无歧义识别。
// 真实凭据几乎不可能是连续 8 个星号，碰撞风险可忽略。
const mcpSecretMask = "********"

// maskMCPSecretMap 返回一份敏感值已打码的新 map；空值与非敏感键原样保留。
// 绝不改动入参 map。
func maskMCPSecretMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v != "" && mcp.IsSensitiveConfigKey(k) {
			out[k] = mcpSecretMask
		} else {
			out[k] = v
		}
	}
	return out
}

// maskMCPEntrySecrets 返回可安全回显给前端的条目拷贝：敏感 env/header 打码。
//
// 采用值拷贝 + 新 map，绝不改动入参——入参可能已写入 servers.json，或正被
// syncMCPRuntime 拿来连接真实 MCP server；就地改会破坏运行时连接或落盘值。
func maskMCPEntrySecrets(e mcp.BotServerEntry) mcp.BotServerEntry {
	out := e
	out.Env = maskMCPSecretMap(e.Env)
	out.Headers = maskMCPSecretMap(e.Headers)
	return out
}

// maskMCPEntries 批量掩码，返回新切片，不改原切片。
func maskMCPEntries(entries []mcp.BotServerEntry) []mcp.BotServerEntry {
	if len(entries) == 0 {
		return entries
	}
	out := make([]mcp.BotServerEntry, len(entries))
	for i := range entries {
		out[i] = maskMCPEntrySecrets(entries[i])
	}
	return out
}

// ErrMCPMaskUnresolvable 表示前端回传了掩码串，但本地没有对应的原值可以还原。
//
// 典型场景是用户把敏感键改名（旧键消失、新键带着掩码回来）：掩码无法反推出真实
// 凭据，静默跳过会让凭据在保存时被丢掉，而界面上还显示着已保存。这种情况必须
// 让请求失败并提示用户重新填写真实值。
var ErrMCPMaskUnresolvable = errs.New("masked secret cannot be resolved: no stored value for this key; re-enter the real value")

// mergeMCPSecretMap 把前端回传的 env/headers 应用到条目：敏感键且值为
// mcpSecretMask 时沿用旧值（用户未改动），避免掩码串把真实凭据写回覆盖；
// 空字符串仍表示显式清空；旧值不存在的掩码键无法还原，返回 ErrMCPMaskUnresolvable。
func mergeMCPSecretMap(old map[string]string, req map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(req))
	for k, val := range req {
		s, ok := val.(string)
		if !ok {
			continue
		}
		if mcp.IsSensitiveConfigKey(k) && s == mcpSecretMask {
			oldVal, exists := old[k]
			if !exists {
				return nil, errs.Wrapf(ErrMCPMaskUnresolvable, "mcp: key %q", k)
			}
			out[k] = oldVal
			continue
		}
		out[k] = s
	}
	return out, nil
}

// handleListBotMcp 列出指定 Bot 的 MCP 服务器。
func (s *Server) handleListBotMcp(c *gin.Context) {
	botID := c.Param("id")
	servers, err := mcp.LoadBotServers(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load mcp servers"))
		return
	}
	if servers == nil {
		servers = []mcp.BotServerEntry{}
	}
	// 若 bot 在跑，用运行时连接态刷新 status，避免 UI 与真实状态脱节。
	if mgr, ok := s.runningUserMCP(botID); ok {
		for i := range servers {
			e := &servers[i]
			connected := mgr.IsServerConnected(e.Name)
			e.Status = mcp.DeriveStatus(e.Enabled, e.ReadyToConnect(), connected, nil)
		}
	}
	OK(c, gin.H{"servers": maskMCPEntries(servers)})
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

	servers, _ := mcp.LoadBotServers(botID)

	now := mcp.NowRFC3339()
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

	entry := mcp.BotServerEntry{
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
	if err := mcp.ValidateServerName(entry.Name); err != nil && entry.Name != "未命名" {
		Fail(c, errs.BadRequest(err.Error()))
		return
	}

	servers = append(servers, entry)
	if err := mcp.SaveBotServers(botID, servers); err != nil {
		Fail(c, errs.Wrap(err, "save mcp servers"))
		return
	}

	// 热挂载：配置已够连接条件时立即 Upsert；否则仅登记，等保存补全。
	entry = s.syncMCPRuntime(botID, "", entry)

	auditLog(c, s.logger, "create_bot_mcp", "bot_id", botID, "mcp", entry.Name)
	OK(c, maskMCPEntrySecrets(entry))
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

	servers, err := mcp.LoadBotServers(botID)
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

	oldName := servers[idx].Name
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
			// 敏感键值为掩码串表示「用户未改动」，沿用旧值，避免回传覆盖真实凭据。
			merged, err := mergeMCPSecretMap(sv.Env, env)
			if err != nil {
				Fail(c, errs.BadRequest(err.Error()))
				return
			}
			sv.Env = merged
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
			merged, err := mergeMCPSecretMap(sv.Headers, headers)
			if err != nil {
				Fail(c, errs.BadRequest(err.Error()))
				return
			}
			sv.Headers = merged
		}
	}
	if v, ok := req["enabled"].(bool); ok {
		sv.Enabled = v
	}

	if err := mcp.ValidateServerName(sv.Name); err != nil && sv.Name != "未命名" {
		Fail(c, errs.BadRequest(err.Error()))
		return
	}

	sv.UpdatedAt = mcp.NowRFC3339()

	if err := mcp.SaveBotServers(botID, servers); err != nil {
		Fail(c, errs.Wrap(err, "save mcp servers"))
		return
	}

	updated := s.syncMCPRuntime(botID, oldName, *sv)
	// 写回 status（sync 可能改写）
	for i := range servers {
		if servers[i].ID == updated.ID {
			servers[i] = updated
			break
		}
	}
	_ = mcp.SaveBotServers(botID, servers)

	auditLog(c, s.logger, "update_bot_mcp", "bot_id", botID, "mcp_id", mid)
	OK(c, maskMCPEntrySecrets(updated))
}

// handleRemoveBotMcp 删除 MCP 服务器。
func (s *Server) handleRemoveBotMcp(c *gin.Context) {
	botID := c.Param("id")
	mid := c.Param("mid")

	servers, err := mcp.LoadBotServers(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load mcp servers"))
		return
	}

	var removedName string
	found := false
	for i, sv := range servers {
		if sv.ID == mid {
			removedName = sv.Name
			servers = append(servers[:i], servers[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		Fail(c, errs.NotFound("mcp server not found"))
		return
	}

	if err := mcp.SaveBotServers(botID, servers); err != nil {
		Fail(c, errs.Wrap(err, "save mcp servers"))
		return
	}

	if mgr, ok := s.runningUserMCP(botID); ok && removedName != "" {
		mgr.RemoveServer(removedName)
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

	servers, _ := mcp.LoadBotServers(botID)
	now := mcp.NowRFC3339()

	var created []mcp.BotServerEntry
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

		entry := mcp.BotServerEntry{
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
		entry = s.syncMCPRuntime(botID, "", entry)
		servers = append(servers, entry)
		created = append(created, entry)
	}

	if err := mcp.SaveBotServers(botID, servers); err != nil {
		Fail(c, errs.Wrap(err, "save mcp servers"))
		return
	}

	auditLog(c, s.logger, "import_bot_mcp", "bot_id", botID, "count", fmt.Sprintf("%d", len(created)))
	OK(c, gin.H{"servers": maskMCPEntries(created)})
}

// runningUserMCP 返回运行中 bot 的用户 MCP Manager。
func (s *Server) runningUserMCP(botID string) (*mcp.Manager, bool) {
	if s.botSvc == nil {
		return nil, false
	}
	return s.botSvc.RunningUserMCP(botID)
}

// syncMCPRuntime 将一条配置同步到运行中 bot 的 MCP Manager。
// oldName 非空且与 entry.Name 不同时，先移除旧名（支持重命名）。
// bot 未运行时仅更新 entry.Status 推导值并返回。
func (s *Server) syncMCPRuntime(botID, oldName string, entry mcp.BotServerEntry) mcp.BotServerEntry {
	mgr, ok := s.runningUserMCP(botID)
	if !ok || mgr == nil {
		entry.Status = mcp.DeriveStatus(entry.Enabled, entry.ReadyToConnect(), false, nil)
		return entry
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if oldName != "" && oldName != entry.Name {
		mgr.RemoveServer(oldName)
	}

	cfg := entry.ToServerConfig()
	var connectErr error
	if err := mgr.UpsertServer(ctx, cfg); err != nil {
		connectErr = err
		if s.logger != nil {
			s.logger.Warnw("mcp hot-mount failed",
				"bot_id", botID, "server", entry.Name, "err", err)
		}
	}
	connected := mgr.IsServerConnected(entry.Name)
	entry.Status = mcp.DeriveStatus(entry.Enabled, entry.ReadyToConnect(), connected, connectErr)
	return entry
}
