package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
// Per-bot MCP 配置持久化 — data/mcp/{botId}/servers.json
//
// Admin UI（api/handler_bot_mcp.go）与 bot 可调用工具共用此存储。
// 运行时 Manager 通过 LoadBotServers + UpsertServer 热挂载；仅写文件不挂载
// 会导致「保存成功但工具列表不刷新，重启后才出现」的 bug。
// ============================================================================

// BotServerEntry 是写入 servers.json 的 MCP 服务器实体（与 Admin UI 字段对齐）。
type BotServerEntry struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Type      string            `json:"type"` // "stdio" | "http" | "sse"
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	Cwd       string            `json:"cwd"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Enabled   bool              `json:"enabled"`
	Status    string            `json:"status"` // "draft" | "running" | "disabled" | "error"
	CreatedAt string            `json:"createdAt"`
	UpdatedAt string            `json:"updatedAt"`
}

// BotStoreFile 返回 Bot 的 MCP 服务器配置文件路径。
func BotStoreFile(botID string) string {
	return filepath.Join("data", "mcp", botID, "servers.json")
}

// LoadBotServers 从文件加载 MCP 服务器列表。文件不存在时返回空切片。
func LoadBotServers(botID string) ([]BotServerEntry, error) {
	path := BotStoreFile(botID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var servers []BotServerEntry
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, err
	}
	return servers, nil
}

// SaveBotServers 将 MCP 服务器列表保存到文件。
func SaveBotServers(botID string, servers []BotServerEntry) error {
	path := BotStoreFile(botID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if servers == nil {
		servers = []BotServerEntry{}
	}
	data, err := json.MarshalIndent(servers, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// NormalizeTransport 将 UI/配置中的 type 映射为 Manager 支持的 transport。
// "sse" 走 http 传输（HTTP transport 已能解析 text/event-stream）。
func NormalizeTransport(tp string) string {
	switch strings.ToLower(strings.TrimSpace(tp)) {
	case "http", "sse":
		return "http"
	case "stdio", "":
		return "stdio"
	default:
		return strings.ToLower(strings.TrimSpace(tp))
	}
}

// EnvMapToSlice 将 map 环境变量转为 "KEY=VALUE" 切片。
func EnvMapToSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		if k == "" {
			continue
		}
		out = append(out, k+"="+v)
	}
	return out
}

// ToServerConfig 将持久化条目转为运行时 ServerConfig。
func (e BotServerEntry) ToServerConfig() ServerConfig {
	args := e.Args
	if args == nil {
		args = []string{}
	}
	return ServerConfig{
		Name:      e.Name,
		Transport: NormalizeTransport(e.Type),
		Command:   e.Command,
		Args:      args,
		Env:       EnvMapToSlice(e.Env),
		Cwd:       e.Cwd,
		URL:       e.URL,
		Headers:   e.Headers,
		Enabled:   e.Enabled && e.ReadyToConnect(),
	}
}

// ReadyToConnect 判断配置是否足够发起连接（避免 draft 空壳去 spawn 失败进程）。
func (e BotServerEntry) ReadyToConnect() bool {
	tp := NormalizeTransport(e.Type)
	switch tp {
	case "http":
		return strings.TrimSpace(e.URL) != ""
	default: // stdio
		return strings.TrimSpace(e.Command) != ""
	}
}

// DeriveStatus 根据 enabled / 就绪态 / 连接结果推导 status 字段。
func DeriveStatus(enabled, ready, connected bool, connectErr error) string {
	if !enabled {
		return "disabled"
	}
	if !ready {
		return "draft"
	}
	if connectErr != nil {
		return "error"
	}
	if connected {
		return "running"
	}
	return "draft"
}

// NowRFC3339 返回 UTC RFC3339 时间戳（供条目 CreatedAt/UpdatedAt 使用）。
func NowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// ValidateServerName 校验服务器名是否可用作工具前缀。
func ValidateServerName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("mcp: server name is required")
	}
	if strings.ContainsAny(name, " \t\n/") {
		return fmt.Errorf("mcp: server name must not contain whitespace or '/'")
	}
	if strings.EqualFold(name, "browser") {
		return fmt.Errorf("mcp: server name %q is reserved", name)
	}
	return nil
}
