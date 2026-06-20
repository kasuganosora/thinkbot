package sandbox

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// SandboxManager — 会话级工作空间管理器
//
// 按 SessionKey（BotID+Channel+UserID）维度管理工作空间：
//   - 惰性创建：首次请求时创建，后续复用
//   - 生命周期管理：CloseAll 统一销毁
//   - 闲置清理：后台 goroutine 定期清理长时间未使用的工作空间
// ============================================================================

// SessionKey 唯一标识一个会话级工作空间。
type SessionKey struct {
	BotID    string
	Channel  string
	UserID   string
}

// String 返回 SessionKey 的字符串表示，用作 map key。
func (k SessionKey) String() string {
	return fmt.Sprintf("%s:%s:%s", k.BotID, k.Channel, k.UserID)
}

// managedWorkspace 包装一个 Workspace 及其元数据。
type managedWorkspace struct {
	ws       Workspace
	lastUsed atomic.Int64 // unix nano timestamp
}

// idleCleanupInterval 后台清理检查间隔。
const idleCleanupInterval = 5 * time.Minute

// defaultIdleTTL 工作空间默认闲置过期时间。
const defaultIdleTTL = 30 * time.Minute

// SandboxManager 管理会话级工作空间池。
type SandboxManager struct {
	sb     Sandbox
	logger *zap.SugaredLogger

	mu         sync.RWMutex
	workspaces map[string]*managedWorkspace

	idleTTL time.Duration

	stopCh chan struct{}
	closeOnce sync.Once
	wg     sync.WaitGroup
}

// NewSandboxManager 创建工作空间管理器。
//
// idleTTL 指定工作空间闲置多久后自动清理。零值使用默认值 30 分钟。
// 调用 Close() 后台清理 goroutine 自动停止。
func NewSandboxManager(sb Sandbox, logger *zap.SugaredLogger, idleTTL time.Duration) *SandboxManager {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	logger = logger.With("component", "sandbox_manager")

	if idleTTL <= 0 {
		idleTTL = defaultIdleTTL
	}

	m := &SandboxManager{
		sb:         sb,
		logger:     logger,
		workspaces: make(map[string]*managedWorkspace),
		idleTTL:    idleTTL,
		stopCh:     make(chan struct{}),
	}

	// 启动后台清理 goroutine
	m.wg.Add(1)
	go m.cleanupLoop()

	return m
}

// Backend 返回底层后端类型标识。
func (m *SandboxManager) Backend() string {
	return m.sb.Backend()
}

// GetOrCreate 获取或创建指定会话的工作空间。
// 如果该会话已有工作空间则复用，否则惰性创建。
func (m *SandboxManager) GetOrCreate(key SessionKey) (Workspace, error) {
	k := key.String()

	// 快速路径：读锁（lastUsed 用 atomic 写，无需写锁）
	m.mu.RLock()
	if entry, ok := m.workspaces[k]; ok {
		entry.lastUsed.Store(time.Now().UnixNano())
		m.mu.RUnlock()
		return entry.ws, nil
	}
	m.mu.RUnlock()

	// 慢路径：写锁
	m.mu.Lock()
	defer m.mu.Unlock()

	// double-check
	if entry, ok := m.workspaces[k]; ok {
		entry.lastUsed.Store(time.Now().UnixNano())
		return entry.ws, nil
	}

	// 创建新工作空间
	wsID := idgen.New("ws")
	ws, err := m.sb.Create(wsID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UnixNano()
	entry := &managedWorkspace{ws: ws}
	entry.lastUsed.Store(now)
	m.workspaces[k] = entry

	m.logger.Debugw("workspace created for session",
		"session", k, "ws_id", wsID, "backend", m.sb.Backend())

	return ws, nil
}

// CloseWorkspace 销毁指定会话的工作空间（如果存在）。
func (m *SandboxManager) CloseWorkspace(key SessionKey) {
	k := key.String()

	m.mu.Lock()
	entry, ok := m.workspaces[k]
	delete(m.workspaces, k)
	m.mu.Unlock()

	if ok {
		if err := entry.ws.Close(); err != nil {
			m.logger.Warnw("failed to close workspace",
				"session", k, "err", err)
		}
	}
}

// CloseAll 销毁所有工作空间。
func (m *SandboxManager) CloseAll() {
	m.mu.Lock()
	saved := m.workspaces
	m.workspaces = make(map[string]*managedWorkspace)
	m.mu.Unlock()

	for k, entry := range saved {
		if err := entry.ws.Close(); err != nil {
			m.logger.Warnw("failed to close workspace during CloseAll",
				"session", k, "err", err)
		}
	}
	m.logger.Infow("all workspaces closed", "count", len(saved))
}

// Close 停止管理器：停止后台 goroutine、销毁所有工作空间、关闭后端。
// 可安全多次调用。
func (m *SandboxManager) Close() error {
	var err error
	m.closeOnce.Do(func() {
		close(m.stopCh)
		m.wg.Wait()
		m.CloseAll()
		err = m.sb.Close()
	})
	return err
}

// Count 返回当前活跃的工作空间数量。
func (m *SandboxManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.workspaces)
}

// HealthCheckAll 检查所有活跃工作空间的健康状态。
// 返回 sessionKey → HealthStatus 的映射。
func (m *SandboxManager) HealthCheckAll(ctx context.Context) map[string]HealthStatus {
	m.mu.RLock()
	keys := make([]string, 0, len(m.workspaces))
	for k, entry := range m.workspaces {
		_ = entry
		keys = append(keys, k)
	}
	m.mu.RUnlock()

	result := make(map[string]HealthStatus, len(keys))
	for _, k := range keys {
		m.mu.RLock()
		entry, ok := m.workspaces[k]
		m.mu.RUnlock()
		if !ok {
			result[k] = HealthStatus{
				Healthy: false, Backend: m.sb.Backend(), Status: "evicted",
				Message: "workspace was removed",
			}
			continue
		}
		result[k] = entry.ws.HealthCheck(ctx)
	}
	return result
}

// HealthCheck 检查指定会话工作空间的健康状态。
func (m *SandboxManager) HealthCheck(ctx context.Context, key SessionKey) HealthStatus {
	k := key.String()
	m.mu.RLock()
	entry, ok := m.workspaces[k]
	m.mu.RUnlock()
	if !ok {
		return HealthStatus{
			Healthy: false,
			Backend: m.sb.Backend(),
			Status:  "not-created",
			Message: fmt.Sprintf("session workspace %q has not been created yet", k),
		}
	}
	return entry.ws.HealthCheck(ctx)
}

// cleanupLoop 定期清理闲置工作空间。
func (m *SandboxManager) cleanupLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(idleCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.cleanupIdle()
		}
	}
}

// cleanupIdle 清理超过 idleTTL 未使用的工作空间。
func (m *SandboxManager) cleanupIdle() {
	now := time.Now().UnixNano()

	m.mu.Lock()
	var toClose []*managedWorkspace
	for k, entry := range m.workspaces {
		if now-entry.lastUsed.Load() > int64(m.idleTTL) {
			toClose = append(toClose, entry)
			delete(m.workspaces, k)
		}
	}
	m.mu.Unlock()

	for _, entry := range toClose {
		if err := entry.ws.Close(); err != nil {
			m.logger.Warnw("failed to close idle workspace", "err", err)
		}
	}

	if len(toClose) > 0 {
		m.logger.Infow("idle workspaces cleaned up", "count", len(toClose))
	}
}
