package memory

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap"
)

// ============================================================================
// ProviderRegistry — 记忆提供者工厂注册表
//
// 参考 Memoh 的 Registry + Factory 模式：
//   - 工厂函数延迟实例化，按需创建 provider
//   - 实例缓存，避免重复创建
//   - 支持从配置动态创建 provider
//   - 单一激活 provider（builtin + 最多一个外部）
//
// 与 ProviderManager 的区别：
//   - ProviderManager 管理已创建的 provider 生命周期
//   - ProviderRegistry 管理 provider 的创建（工厂模式）
//   - Registry → 创建 → 注册到 Manager
// ============================================================================

// ProviderFactory 从配置创建 MemoryProvider 的工厂函数。
type ProviderFactory func(config ProviderFactoryConfig) (MemoryProvider, error)

// ProviderFactoryConfig 传递给工厂的配置。
type ProviderFactoryConfig struct {
	// Name provider 名称标识。
	Name string
	// Platform 平台标识（"cli", "telegram", "discord", "cron"）。
	Platform string
	// BotID Bot 标识。
	BotID string
	// UserID 平台用户标识。
	UserID string
	// HomeDir 数据目录路径。
	HomeDir string
	// AgentContext agent 上下文类型。
	AgentContext string
	// Params provider 特有的自定义参数。
	Params map[string]any
	// Logger 日志记录器。
	Logger *zap.SugaredLogger
}

// ProviderEntry 注册表中的 provider 条目。
type ProviderEntry struct {
	// Factory 创建 provider 的工厂函数。
	Factory ProviderFactory
	// DefaultEnabled 是否默认启用（无需显式配置即激活）。
	DefaultEnabled bool
	// Priority 优先级（数值越大越优先，用于自动选择）。
	Priority int
	// Description 描述信息。
	Description string
}

// ProviderRegistry 记忆提供者注册表。
//
// 使用方式：
//
//	reg := NewProviderRegistry(logger)
//	reg.Register("sqlite", ProviderEntry{
//	    Factory:        NewSQLiteProvider,
//	    DefaultEnabled: false,
//	    Priority:       10,
//	})
//
//	// 从配置创建 provider
//	provider, err := reg.Create("sqlite", ProviderFactoryConfig{
//	    Name:    "sqlite",
//	    HomeDir: "/data/bot",
//	    Params:  map[string]any{"db_path": "memory.db"},
//	})
//
//	// 注册到 ProviderManager
//	manager.AddProvider(provider)
type ProviderRegistry struct {
	mu      sync.RWMutex
	entries map[string]*ProviderEntry // name -> entry
	cache   map[string]MemoryProvider // cacheKey -> instance
	logger  *zap.SugaredLogger
}

// NewProviderRegistry 创建 provider 注册表。
func NewProviderRegistry(logger *zap.SugaredLogger) *ProviderRegistry {
	return &ProviderRegistry{
		entries: make(map[string]*ProviderEntry),
		cache:   make(map[string]MemoryProvider),
		logger:  logger.With("component", "provider_registry"),
	}
}

// Register 注册一个 provider 工厂。
// 如果同名工厂已存在，覆盖旧的。
func (r *ProviderRegistry) Register(name string, entry ProviderEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e := entry // copy
	r.entries[name] = &e
	r.logger.Debugw("provider factory registered",
		"name", name,
		"priority", entry.Priority,
		"default_enabled", entry.DefaultEnabled)
}

// Unregister 取消注册一个 provider 工厂。
func (r *ProviderRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.entries, name)
	// 清除缓存
	cachePrefix := name + ":"
	for key := range r.cache {
		if len(key) > len(cachePrefix) && key[:len(cachePrefix)] == cachePrefix {
			delete(r.cache, key)
		}
	}
}

// IsRegistered 检查工厂是否已注册。
func (r *ProviderRegistry) IsRegistered(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.entries[name]
	return ok
}

// Names 返回所有已注册的工厂名称。
func (r *ProviderRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	return names
}

// Create 从工厂创建一个 provider 实例。
// 使用缓存避免重复创建同一配置的 provider。
func (r *ProviderRegistry) Create(name string, config ProviderFactoryConfig) (MemoryProvider, error) {
	r.mu.RLock()
	entry, ok := r.entries[name]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("provider registry: unknown provider %q", name)
	}

	// 构建缓存键
	cacheKey := r.cacheKey(name, config)

	// 检查缓存
	r.mu.RLock()
	if cached, ok := r.cache[cacheKey]; ok {
		r.mu.RUnlock()
		return cached, nil
	}
	r.mu.RUnlock()

	// 确保配置中的 name 一致
	if config.Name == "" {
		config.Name = name
	}
	if config.Logger == nil {
		config.Logger = r.logger
	}

	// 调用工厂
	provider, err := entry.Factory(config)
	if err != nil {
		return nil, fmt.Errorf("provider registry: factory %q failed: %w", name, err)
	}

	// 缓存实例
	r.mu.Lock()
	r.cache[cacheKey] = provider
	r.mu.Unlock()

	r.logger.Infow("provider created via factory",
		"name", name,
		"platform", config.Platform,
		"bot_id", config.BotID)

	return provider, nil
}

// CreateAndInitialize 创建并初始化 provider。
func (r *ProviderRegistry) CreateAndInitialize(
	ctx context.Context,
	name string,
	config ProviderFactoryConfig,
	sessionID string,
	opts ...ProviderOption,
) (MemoryProvider, error) {
	provider, err := r.Create(name, config)
	if err != nil {
		return nil, err
	}

	if err := provider.Initialize(ctx, sessionID, opts...); err != nil {
		return nil, fmt.Errorf("provider registry: initialize %q failed: %w", name, err)
	}

	return provider, nil
}

// CreateDefaultEnabled 创建所有默认启用的 provider。
// 按 Priority 降序排列。
func (r *ProviderRegistry) CreateDefaultEnabled(config ProviderFactoryConfig) []MemoryProvider {
	r.mu.RLock()
	var entries []*ProviderEntry
	var names []string
	for name, entry := range r.entries {
		if entry.DefaultEnabled {
			entries = append(entries, entry)
			names = append(names, name)
		}
	}
	r.mu.RUnlock()

	// 按 Priority 降序排列
	type nameEntry struct {
		name  string
		entry *ProviderEntry
	}
	var sorted []nameEntry
	for i, name := range names {
		sorted = append(sorted, nameEntry{name, entries[i]})
	}
	// 简单插入排序（数量通常很少）
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].entry.Priority > sorted[j-1].entry.Priority; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	var providers []MemoryProvider
	for _, ne := range sorted {
		cfg := config
		cfg.Name = ne.name
		provider, err := r.Create(ne.name, cfg)
		if err != nil {
			r.logger.Warnw("failed to create default-enabled provider",
				"name", ne.name, "err", err)
			continue
		}
		providers = append(providers, provider)
	}

	return providers
}

// BestAvailable 返回优先级最高的可用 provider（isAvailable 返回 true）。
// 用于自动选择场景（如配置中未指定具体 provider）。
func (r *ProviderRegistry) BestAvailable(config ProviderFactoryConfig) (MemoryProvider, error) {
	r.mu.RLock()
	type nameEntry struct {
		name  string
		entry *ProviderEntry
	}
	var sorted []nameEntry
	for name, entry := range r.entries {
		sorted = append(sorted, nameEntry{name, entry})
	}
	r.mu.RUnlock()

	// 按 Priority 降序
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].entry.Priority > sorted[j-1].entry.Priority; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	for _, ne := range sorted {
		cfg := config
		cfg.Name = ne.name
		provider, err := r.Create(ne.name, cfg)
		if err != nil {
			continue
		}
		if provider.IsAvailable() {
			return provider, nil
		}
	}

	return nil, fmt.Errorf("provider registry: no available provider")
}

// ClearCache 清除所有缓存的 provider 实例。
// 缓存的 provider 不会被 Shutdown（调用方需自行管理生命周期）。
func (r *ProviderRegistry) ClearCache() {
	r.mu.Lock()
	r.cache = make(map[string]MemoryProvider)
	r.mu.Unlock()
}

// cacheKey 构建缓存键。
func (r *ProviderRegistry) cacheKey(name string, config ProviderFactoryConfig) string {
	return fmt.Sprintf("%s:%s:%s:%s", name, config.Platform, config.BotID, config.UserID)
}

// HealthCheck 检查所有已注册工厂的可达性。
// 返回每个 provider 的健康状态。
func (r *ProviderRegistry) HealthCheck(ctx context.Context) map[string]bool {
	r.mu.RLock()
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	r.mu.RUnlock()

	results := make(map[string]bool, len(names))
	for _, name := range names {
		// 尝试创建并检查可用性
		provider, err := r.Create(name, ProviderFactoryConfig{
			Name:   name,
			Logger: r.logger,
		})
		if err != nil {
			results[name] = false
			continue
		}
		results[name] = provider.IsAvailable()
	}

	return results
}
