package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// MemoryProvider — 可插拔记忆后端抽象
//
// MemoryProvider 接口 + ProviderManager 编排器设计：
// 将记忆后端的实现与使用方解耦，允许通过统一接口接入不同的存储/检索引擎。
//
// 与 Store/Retriever 的区别：
//   - Store/Retriever 是低层接口（纯数据存取）
//   - Provider 是高层接口（含生命周期、系统提示、prefetch、工具注册）
//
// 生命周期：
//   1. Initialize()   — 会话启动，连接后端、预热
//   2. Prefetch()     — 每轮对话前，检索相关记忆
//   3. SyncTurn()     — 每轮对话后，持久化本轮内容
//   4. OnSessionEnd() — 会话结束，执行提取/总结
//   5. Shutdown()     — 关闭连接
//
// 同时只允许一个外部 Provider 激活（防止工具冲突）。
// ============================================================================

// MemoryProvider 定义记忆后端的高层接口。
type MemoryProvider interface {
	// Name 返回提供者标识（如 "builtin"、"sqlite"、"honcho"）。
	Name() string

	// IsAvailable 检查提供者是否已配置且可用（不发起网络请求）。
	IsAvailable() bool

	// Initialize 初始化会话。
	Initialize(ctx context.Context, sessionID string, opts ...ProviderOption) error

	// SystemPromptBlock 返回注入系统提示的静态文本（提供者说明、记忆快照等）。
	SystemPromptBlock() string

	// Prefetch 在每轮对话前检索相关记忆。
	// 返回格式化后的上下文文本，空字符串表示无相关内容。
	Prefetch(ctx context.Context, query string, sessionID string) (string, error)

	// QueuePrefetch 异步预取下一轮可能需要的记忆。
	// 结果缓存在内存中，下次 Prefetch 调用时返回。
	QueuePrefetch(ctx context.Context, query string, sessionID string)

	// SyncTurn 持久化一轮对话（用户消息 + 助手回复）。
	// 应为非阻塞操作——实现方应使用后台队列。
	SyncTurn(ctx context.Context, userContent, assistantContent, sessionID string) error

	// OnSessionEnd 会话结束时调用（提取/总结/刷新队列）。
	OnSessionEnd(ctx context.Context, sessionID string)

	// Shutdown 关闭提供者（刷新队列、关闭连接）。
	Shutdown() error
}

// ProviderOption 配置 Provider 初始化。
type ProviderOption func(*ProviderInitConfig)

// ProviderInitConfig 传递给 Initialize 的配置。
type ProviderInitConfig struct {
	// Platform 平台标识（"cli", "telegram", "discord", "cron" 等）。
	Platform string
	// BotID Bot 标识。
	BotID string
	// UserID 平台用户标识。
	UserID string
	// HomeDir 数据目录路径。
	HomeDir string
	// AgentContext agent 上下文类型（"primary", "subagent", "cron"）。
	AgentContext string
}

// ============================================================================
// ProviderManager — 记忆提供者编排器
//
// MemoryManager 设计：
// 管理 builtin + 最多一个外部 provider，统一调度生命周期。
// 单一集成点，一个 provider 失败不阻塞其他。
// ============================================================================

// ProviderManager 编排记忆提供者。
type ProviderManager struct {
	mu          sync.RWMutex
	providers   []MemoryProvider
	toolIndex   map[string]MemoryProvider // tool name -> provider
	hasExternal bool

	// 后台同步执行器
	syncExecutor *SyncExecutor
	prefetchMgr  *PrefetchManager
	logger       *zap.SugaredLogger
}

// NewProviderManager 创建记忆提供者编排器。
func NewProviderManager(logger *zap.SugaredLogger) *ProviderManager {
	executor := NewSyncExecutor(16)
	executor.SetLogger(logger)
	return &ProviderManager{
		toolIndex:    make(map[string]MemoryProvider),
		syncExecutor: executor,
		prefetchMgr:  NewPrefetchManager(logger),
		logger:       logger.With("component", "memory_provider_manager"),
	}
}

// AddProvider 注册一个记忆提供者。
// builtin 提供者始终接受；外部提供者只允许一个。
func (m *ProviderManager) AddProvider(provider MemoryProvider) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	isBuiltin := provider.Name() == "builtin"
	if !isBuiltin {
		if m.hasExternal {
			m.logger.Warnw("rejected external memory provider — one already registered",
				"provider", provider.Name())
			return false
		}
		m.hasExternal = true
	}

	m.providers = append(m.providers, provider)
	m.logger.Infow("memory provider registered",
		"provider", provider.Name(),
		"total", len(m.providers))
	return true
}

// Providers 返回所有已注册的提供者。
func (m *ProviderManager) Providers() []MemoryProvider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]MemoryProvider{}, m.providers...)
}

// BuildSystemPrompt 收集所有提供者的系统提示块。
func (m *ProviderManager) BuildSystemPrompt() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var blocks []string
	for _, p := range m.providers {
		block := p.SystemPromptBlock()
		if block != "" {
			blocks = append(blocks, block)
		}
	}

	if len(blocks) == 0 {
		return ""
	}
	if len(blocks) == 1 {
		return blocks[0]
	}
	// 用双换行拼接
	var sb strings.Builder
	sb.WriteString(blocks[0])
	for _, b := range blocks[1:] {
		sb.WriteString("\n\n")
		sb.WriteString(b)
	}
	return sb.String()
}

// PrefetchAll 从所有提供者收集 prefetch 上下文。
// 一个提供者失败不阻塞其他。
func (m *ProviderManager) PrefetchAll(ctx context.Context, query, sessionID string) string {
	m.mu.RLock()
	providers := append([]MemoryProvider{}, m.providers...)
	m.mu.RUnlock()

	var parts []string
	for _, p := range providers {
		// 先检查 prefetch 缓存
		cached := m.prefetchMgr.Get(query)
		if cached != "" {
			parts = append(parts, cached)
			continue
		}

		result, err := p.Prefetch(ctx, query, sessionID)
		if err != nil {
			m.logger.Debugw("provider prefetch failed (non-fatal)",
				"provider", p.Name(), "err", err)
			continue
		}
		if result != "" {
			parts = append(parts, result)
		}
	}

	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	var sb strings.Builder
	sb.WriteString(parts[0])
	for _, p := range parts[1:] {
		sb.WriteString("\n\n")
		sb.WriteString(p)
	}
	return sb.String()
}

// QueuePrefetchAll 为所有提供者排队下一轮的 prefetch。
func (m *ProviderManager) QueuePrefetchAll(ctx context.Context, query, sessionID string) {
	if query == "" {
		return
	}
	m.mu.RLock()
	providers := append([]MemoryProvider{}, m.providers...)
	m.mu.RUnlock()

	for _, p := range providers {
		p.QueuePrefetch(ctx, query, sessionID)
	}
}

// SyncAll 将一轮对话同步到所有提供者。
// 后台执行，不阻塞调用方。
func (m *ProviderManager) SyncAll(userContent, assistantContent, sessionID string) {
	m.mu.RLock()
	providers := append([]MemoryProvider{}, m.providers...)
	m.mu.RUnlock()

	if len(providers) == 0 {
		return
	}

	m.syncExecutor.Submit(func() {
		ctx := traceid.NewContext(context.Background())
		for _, p := range providers {
			if err := p.SyncTurn(ctx, userContent, assistantContent, sessionID); err != nil {
				logger := traceid.WithLoggerFrom(ctx, m.logger)
				logger.Warnw("provider sync_turn failed",
					"provider", p.Name(), "err", err)
			}
		}
	})
}

// InitializeAll 初始化所有提供者。
func (m *ProviderManager) InitializeAll(ctx context.Context, sessionID string, opts ...ProviderOption) {
	m.mu.RLock()
	providers := append([]MemoryProvider{}, m.providers...)
	m.mu.RUnlock()

	for _, p := range providers {
		if err := p.Initialize(ctx, sessionID, opts...); err != nil {
			m.logger.Warnw("provider initialize failed",
				"provider", p.Name(), "err", err)
		}
	}
}

// OnSessionEndAll 通知所有提供者会话结束。
func (m *ProviderManager) OnSessionEndAll(ctx context.Context, sessionID string) {
	m.mu.RLock()
	providers := append([]MemoryProvider{}, m.providers...)
	m.mu.RUnlock()

	for _, p := range providers {
		p.OnSessionEnd(ctx, sessionID)
	}
}

// ShutdownAll 关闭所有提供者（逆序）。
func (m *ProviderManager) ShutdownAll() {
	// 先 flush 后台任务
	m.syncExecutor.Shutdown(5 * time.Second)
	m.prefetchMgr.Shutdown()

	m.mu.RLock()
	providers := append([]MemoryProvider{}, m.providers...)
	m.mu.RUnlock()

	// 逆序关闭
	for i := len(providers) - 1; i >= 0; i-- {
		if err := providers[i].Shutdown(); err != nil {
			m.logger.Warnw("provider shutdown failed",
				"provider", providers[i].Name(), "err", err)
		}
	}
}

// FlushPending 阻塞等待所有排队任务完成。
func (m *ProviderManager) FlushPending(timeout time.Duration) {
	// 用 sentinel channel 确保所有之前的任务已完成，避免 time.Sleep 竞态
	sentinel := make(chan struct{})
	m.syncExecutor.Submit(func() {
		close(sentinel)
	})
	select {
	case <-sentinel:
	case <-time.After(timeout):
	}
}
