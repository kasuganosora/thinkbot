package mcp

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap"
)

// ============================================================================
// Manager — 管理多个 MCP 服务器的连接
// ============================================================================

// ServerConfig 描述一个 MCP 服务器的连接配置。
type ServerConfig struct {
	// Name 服务器名称（唯一标识，也用作工具名前缀）。
	Name string

	// Transport 传输类型: "stdio" 或 "http"。
	Transport string

	// Command stdio 模式下的可执行命令（如 "npx"）。
	Command string

	// Args stdio 模式下的命令参数。
	Args []string

	// Env stdio 模式下的环境变量（"KEY=VALUE" 格式）。
	Env []string

	// URL HTTP 模式下的服务器地址。
	URL string

	// Headers HTTP 模式下的自定义请求头。
	Headers map[string]string

	// Enabled 是否启用。false 时跳过此服务器。
	Enabled bool
}

// Manager 管理多个 MCP 服务器连接的生命周期。
//
// 职责：
//   - 按 ServerConfig 创建和初始化 Client
//   - 缓存每个服务器的工具列表（首次使用时获取）
//   - 提供统一的工具查询和调用接口
//   - 优雅关闭所有连接
//   - 运行时针对单个服务器开启/关闭
type Manager struct {
	mu             sync.RWMutex
	clients        map[string]*Client // name → client
	configs        map[string]ServerConfig
	logger         *zap.SugaredLogger
	onServerChange func() // 服务器状态变更回调（由 Provider 设置以失效缓存）
}

// NewManager 创建 MCP 管理器。
func NewManager(logger *zap.SugaredLogger) *Manager {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &Manager{
		clients: make(map[string]*Client),
		configs: make(map[string]ServerConfig),
		logger:  logger.With("component", "mcp_manager"),
	}
}

// AddServer 注册一个 MCP 服务器配置（不立即连接）。
// 如果已存在同名配置则覆盖。
func (m *Manager) AddServer(cfg ServerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[cfg.Name] = cfg
}

// RemoveServer 移除并关闭一个服务器。
func (m *Manager) RemoveServer(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if client, ok := m.clients[name]; ok {
		_ = client.Close()
		delete(m.clients, name)
	}
	delete(m.configs, name)
}

// EnableServer 启用并连接指定的 MCP 服务器。
// 如果服务器已连接，则幂等返回 nil。
// 连接成功后会触发 onServerChange 回调。
func (m *Manager) EnableServer(ctx context.Context, name string) error {
	m.mu.Lock()
	cfg, ok := m.configs[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mcp: server %q not configured", name)
	}
	if cfg.Enabled {
		_, alreadyConnected := m.clients[name]
		m.mu.Unlock()
		if alreadyConnected {
			return nil // 已启用且已连接
		}
	} else {
		cfg.Enabled = true
		m.configs[name] = cfg
		m.mu.Unlock()
	}

	if err := m.connectOne(ctx, cfg); err != nil {
		return err
	}

	m.logger.Infow("mcp server enabled", "server", name)
	m.notifyServerChange()
	return nil
}

// DisableServer 禁用并断开指定的 MCP 服务器。
// 断开后会触发 onServerChange 回调。
func (m *Manager) DisableServer(name string) error {
	m.mu.Lock()
	cfg, ok := m.configs[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("mcp: server %q not configured", name)
	}
	if !cfg.Enabled {
		m.mu.Unlock()
		return nil // 已经是禁用状态
	}
	cfg.Enabled = false
	m.configs[name] = cfg

	client, wasConnected := m.clients[name]
	if wasConnected {
		delete(m.clients, name)
	}
	m.mu.Unlock()

	if wasConnected {
		_ = client.Close()
	}

	m.logger.Infow("mcp server disabled", "server", name)
	m.notifyServerChange()
	return nil
}

// IsServerEnabled 检查指定服务器是否已启用。
func (m *Manager) IsServerEnabled(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfg, ok := m.configs[name]
	return ok && cfg.Enabled
}

// IsServerConnected 检查指定服务器是否已连接。
func (m *Manager) IsServerConnected(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.clients[name]
	return ok
}

// ServerStatus 描述单个服务器的当前状态。
type ServerStatus struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
}

// ListServers 返回所有已配置服务器的状态。
func (m *Manager) ListServers() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]ServerStatus, 0, len(m.configs))
	for name, cfg := range m.configs {
		_, connected := m.clients[name]
		result = append(result, ServerStatus{
			Name:      name,
			Transport: cfg.Transport,
			Enabled:   cfg.Enabled,
			Connected: connected,
		})
	}
	return result
}

// SetOnServerChange 设置服务器状态变更回调。
// 由 Provider 在注册时调用，以便在服务器增减后自动失效缓存。
func (m *Manager) SetOnServerChange(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onServerChange = fn
}

// notifyServerChange 调用变更回调（如果已设置）。
func (m *Manager) notifyServerChange() {
	m.mu.RLock()
	fn := m.onServerChange
	m.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// Connect 连接所有已注册且 Enabled 的服务器。
// 已经连接的服务器会被跳过。
func (m *Manager) Connect(ctx context.Context) error {
	m.mu.Lock()
	configs := make([]ServerConfig, 0, len(m.configs))
	for _, cfg := range m.configs {
		if cfg.Enabled {
			configs = append(configs, cfg)
		}
	}
	m.mu.Unlock()

	var errs []error
	for _, cfg := range configs {
		if err := m.connectOne(ctx, cfg); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("mcp: %d server(s) failed to connect: %v", len(errs), errs)
	}
	return nil
}

// connectOne 连接单个服务器。
func (m *Manager) connectOne(ctx context.Context, cfg ServerConfig) error {
	m.mu.Lock()
	if _, exists := m.clients[cfg.Name]; exists {
		m.mu.Unlock()
		return nil // 已连接
	}
	m.mu.Unlock()

	client, err := m.createClient(ctx, cfg)
	if err != nil {
		m.logger.Errorw("failed to connect mcp server",
			"server", cfg.Name,
			"err", err)
		return err
	}

	// MCP 握手
	serverInfo, err := client.Initialize(ctx)
	if err != nil {
		_ = client.Close()
		return err
	}

	m.mu.Lock()
	m.clients[cfg.Name] = client
	m.mu.Unlock()

	m.logger.Infow("mcp server connected",
		"server", cfg.Name,
		"server_name", serverInfo.Name,
		"server_version", serverInfo.Version)
	return nil
}

// createClient 根据配置创建传输层和客户端。
func (m *Manager) createClient(ctx context.Context, cfg ServerConfig) (*Client, error) {
	var tp transport
	var err error

	switch cfg.Transport {
	case "stdio", "":
		if cfg.Command == "" {
			return nil, fmt.Errorf("mcp: server %q: stdio transport requires command", cfg.Name)
		}
		tp, err = newStdioTransport(ctx, cfg.Command, cfg.Args, cfg.Env)
	case "http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("mcp: server %q: http transport requires url", cfg.Name)
		}
		tp = newHTTPTransport(cfg.URL, cfg.Headers)
	default:
		return nil, fmt.Errorf("mcp: server %q: unknown transport %q", cfg.Name, cfg.Transport)
	}
	if err != nil {
		return nil, err
	}

	return newClient(cfg.Name, tp, m.logger), nil
}

// Close 关闭所有 MCP 服务器连接。
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for name, client := range m.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %q: %w", name, err))
		}
	}
	m.clients = make(map[string]*Client)
	if len(errs) > 0 {
		return fmt.Errorf("mcp: close errors: %v", errs)
	}
	return nil
}

// ConnectedServers 返回已连接的服务器名称列表。
func (m *Manager) ConnectedServers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.clients))
	for name := range m.clients {
		names = append(names, name)
	}
	return names
}

// GetClient 获取指定名称的客户端。
func (m *Manager) GetClient(name string) (*Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clients[name]
	return c, ok
}

// ListAllTools 列出所有已连接服务器的工具，返回以服务器名分组的工具列表。
func (m *Manager) ListAllTools(ctx context.Context) (map[string][]mcpTool, error) {
	m.mu.RLock()
	clients := make(map[string]*Client, len(m.clients))
	for name, c := range m.clients {
		clients[name] = c
	}
	m.mu.RUnlock()

	result := make(map[string][]mcpTool)
	for name, c := range clients {
		tools, err := c.ListTools(ctx)
		if err != nil {
			m.logger.Warnw("failed to list tools",
				"server", name,
				"err", err)
			continue
		}
		result[name] = tools
	}
	return result, nil
}

// ServerCount 返回已配置的服务器数量。
func (m *Manager) ServerCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.configs)
}
