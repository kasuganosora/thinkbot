package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/config"
	"go.uber.org/zap"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 配置加载 — 从 config.Store 读取 MCP 服务器配置
// ============================================================================

// LoadServers 从 config.Store 加载所有 MCP 服务器配置。
//
// 配置格式（.env 或数据库）:
//
//	mcp.enabled = true
//	mcp.<name> = {"transport":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-filesystem","."],"enabled":true}
//	mcp.<name> = {"transport":"http","url":"https://example.com/mcp","headers":{"Authorization":"Bearer xxx"},"enabled":true}
//
// 值为 JSON 格式的 ServerConfig（不含 Name 字段，Name 从键名提取）。
func LoadServers(store *config.Store) []ServerConfig {
	all := store.GetByPrefix("mcp.")
	var servers []ServerConfig

	for key, val := range all {
		// 跳过 mcp.enabled 等非服务器配置
		if key == "enabled" || strings.Contains(key, ".") {
			continue
		}
		if val == "" {
			continue
		}

		// 解析 JSON
		var cfg serverConfigJSON
		if err := json.Unmarshal([]byte(val), &cfg); err != nil {
			continue
		}

		sc := ServerConfig{
			Name:      key,
			Transport: cfg.Transport,
			Command:   cfg.Command,
			Args:      cfg.Args,
			Env:       cfg.Env,
			URL:       cfg.URL,
			Headers:   cfg.Headers,
			Enabled:   cfg.Enabled,
		}
		servers = append(servers, sc)
	}

	return servers
}

// serverConfigJSON 是 MCP 服务器配置的 JSON 序列化格式。
type serverConfigJSON struct {
	Transport string            `json:"transport"`         // "stdio" | "http"
	Command   string            `json:"command,omitempty"`  // stdio
	Args      []string          `json:"args,omitempty"`     // stdio
	Env       []string          `json:"env,omitempty"`      // stdio
	URL       string            `json:"url,omitempty"`      // http
	Headers   map[string]string `json:"headers,omitempty"`  // http
	Enabled   bool              `json:"enabled"`
}

// SetupFromConfig 从 config.Store 加载 MCP 配置，创建 Manager，连接所有服务器，
// 并将 Provider 注册到 ToolManager。
//
// 如果 mcp.enabled 为 false 或没有配置任何服务器，返回 nil Manager（no-op）。
// 调用方应在应用生命周期中持有返回的 Manager 以便后续 Close。
func SetupFromConfig(ctx context.Context, store *config.Store, toolMgr *tools.ToolManager, logger *zap.SugaredLogger) (*Manager, error) {
	if !store.GetBool("mcp.enabled", false) {
		return nil, nil
	}

	servers := LoadServers(store)
	if len(servers) == 0 {
		logger.Infow("mcp: enabled but no servers configured")
		return nil, nil
	}

	mgr := NewManager(logger)
	enabledCount := 0
	for _, sc := range servers {
		mgr.AddServer(sc)
		if sc.Enabled {
			enabledCount++
		}
	}

	logger.Infow("mcp: loading servers",
		"total", len(servers),
		"enabled", enabledCount)

	if err := mgr.Connect(ctx); err != nil {
		// 连接失败不 fatal，部分服务器可能离线
		logger.Warnw("mcp: some servers failed to connect", "err", err)
	}

	if err := RegisterTools(toolMgr, mgr); err != nil {
		return nil, errs.Wrap(err, "mcp: register tools")
	}

	logger.Infow("mcp: setup complete",
		"connected_servers", mgr.ConnectedServers())
	return mgr, nil
}
