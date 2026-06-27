package memory

import (
	"context"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ============================================================================
// SyncExecutor — 后台同步执行器
//
// SyncExecutor 设计：
// 单 worker 串行执行后台任务（记忆写入、巩固、prefetch），
// 保证 turn N 的写入在 turn N+1 之前完成，且不阻塞对话循环。
//
// 核心设计：
//   - 单 worker：串行执行，避免并发写入竞态
//   - 带超时的 drain：关闭时等待有限时间，不会无限阻塞
//   - inline fallback：executor 不可用时降级为内联执行
// ============================================================================

// SyncExecutor 是单 worker 的后台任务执行器。
type SyncExecutor struct {
	tasks  chan func()
	logger *zap.SugaredLogger // 可选，用于记录 panic
	once   sync.Once
	wg     sync.WaitGroup
}

// NewSyncExecutor 创建后台同步执行器。
// bufferSize 为任务队列缓冲大小。
func NewSyncExecutor(bufferSize int) *SyncExecutor {
	if bufferSize <= 0 {
		bufferSize = 16
	}
	e := &SyncExecutor{
		tasks: make(chan func(), bufferSize),
	}
	e.wg.Add(1)
	go e.worker()
	return e
}

// SetLogger 设置日志记录器（用于 panic 恢复时记录）。
func (e *SyncExecutor) SetLogger(logger *zap.SugaredLogger) {
	e.logger = logger
}

// worker 是后台执行循环。
func (e *SyncExecutor) worker() {
	defer e.wg.Done()
	for task := range e.tasks {
		// 安全执行：panic 不杀死 worker
		func() {
			defer func() {
				if r := recover(); r != nil {
					if e.logger != nil {
						e.logger.Errorw("background task panicked",
							"panic", r, "stack", string(debug.Stack()))
					}
				}
			}()
			task()
		}()
	}
}

// Submit 提交一个后台任务。
// 如果队列已满或 executor 已关闭，降级为内联执行。
func (e *SyncExecutor) Submit(fn func()) bool {
	select {
	case e.tasks <- fn:
		return true
	default:
		// 队列满 → 内联执行
		fn()
		return false
	}
}

// Shutdown 优雅关闭。
// 关闭任务通道，等待 worker 排空所有已提交任务。
// drainTimeout 为等待队列排空的最大时间。
func (e *SyncExecutor) Shutdown(drainTimeout time.Duration) {
	e.once.Do(func() {
		close(e.tasks)
	})

	if drainTimeout > 0 {
		// 等待 worker 排空（有超时）
		waited := make(chan struct{})
		go func() {
			e.wg.Wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-time.After(drainTimeout):
		}
	} else {
		e.wg.Wait()
	}
}

// ============================================================================
// BackgroundSyncManager — 记忆后台同步管理器
//
// 封装 SyncExecutor，提供记忆相关的后台操作接口。
// 在对话循环中使用：每轮结束后异步写入记忆，不阻塞用户响应。
// ============================================================================

// BackgroundSyncManager 协调记忆的后台写入、巩固和 prefetch。
type BackgroundSyncManager struct {
	executor *SyncExecutor
	logger   *zap.SugaredLogger

	mu           sync.Mutex
	lastSyncAt   map[string]time.Time // scope.Key() -> last sync time
	syncDebounce time.Duration
}

// NewBackgroundSyncManager 创建后台同步管理器。
func NewBackgroundSyncManager(logger *zap.SugaredLogger, opts ...BackgroundSyncConfig) *BackgroundSyncManager {
	cfg := DefaultBackgroundSyncConfig()
	if len(opts) > 0 {
		if opts[0].BufferSize > 0 {
			cfg.BufferSize = opts[0].BufferSize
		}
		if opts[0].SyncDebounce > 0 {
			cfg.SyncDebounce = opts[0].SyncDebounce
		}
	}
	executor := NewSyncExecutor(cfg.BufferSize)
	executor.SetLogger(logger)
	return &BackgroundSyncManager{
		executor:     executor,
		logger:       logger.With("component", "memory_sync"),
		lastSyncAt:   make(map[string]time.Time),
		syncDebounce: cfg.SyncDebounce,
	}
}

// BackgroundSyncConfig 配置后台同步管理器。
type BackgroundSyncConfig struct {
	// BufferSize 任务队列大小（默认 16）。
	BufferSize int
	// SyncDebounce 同一 scope 的最小同步间隔（默认 5 秒）。
	SyncDebounce time.Duration
}

// DefaultBackgroundSyncConfig 返回默认配置。
func DefaultBackgroundSyncConfig() BackgroundSyncConfig {
	return BackgroundSyncConfig{
		BufferSize:   16,
		SyncDebounce: 5 * time.Second,
	}
}

// SubmitSync 提交一次后台记忆同步。
// 带 debounce：同一 scope 在 syncDebounce 内的重复请求被跳过。
func (m *BackgroundSyncManager) SubmitSync(scopeKey string, fn func()) {
	m.mu.Lock()
	if last, ok := m.lastSyncAt[scopeKey]; ok && time.Since(last) < m.syncDebounce {
		m.mu.Unlock()
		return // debounce skip
	}
	m.lastSyncAt[scopeKey] = time.Now()
	m.mu.Unlock()

	m.executor.Submit(fn)
}

// Submit 提交任意后台任务（无 debounce）。
func (m *BackgroundSyncManager) Submit(fn func()) {
	m.executor.Submit(fn)
}

// FlushPending 阻塞等待所有排队任务完成（用于 session 结束）。
// timeout 为最大等待时间。
func (m *BackgroundSyncManager) FlushPending(timeout time.Duration) {
	// 提交一个 sentinel 任务，等待它完成
	sentinel := make(chan struct{})
	m.executor.Submit(func() {
		close(sentinel)
	})
	select {
	case <-sentinel:
	case <-time.After(timeout):
	}
}

// Shutdown 关闭后台同步管理器。
func (m *BackgroundSyncManager) Shutdown() {
	m.executor.Shutdown(5 * time.Second)
}

// ============================================================================
// PrefetchManager — 下一轮预取缓存
//
// 预取队列中所有任务：
// 每轮结束后异步预取下一轮可能需要的记忆，
// 结果缓存在内存中，下一轮的 prefetch() 直接返回缓存。
// ============================================================================

// PrefetchManager 管理记忆预取缓存。
type PrefetchManager struct {
	mu       sync.RWMutex
	cache    map[string]string // query -> prefetched context
	executor *SyncExecutor
	logger   *zap.SugaredLogger
	maxSize  int // 缓存最大条目数（防止长时间运行内存泄漏）
}

// NewPrefetchManager 创建预取管理器。
func NewPrefetchManager(logger *zap.SugaredLogger) *PrefetchManager {
	executor := NewSyncExecutor(8)
	executor.SetLogger(logger)
	return &PrefetchManager{
		cache:    make(map[string]string),
		executor: executor,
		logger:   logger.With("component", "memory_prefetch"),
		maxSize:  64, // 限制缓存条目数，防止长时间运行内存泄漏
	}
}

// QueuePrefetch 异步预取记忆。
// 查询结果缓存在内存中，下次 Get() 时返回。
func (p *PrefetchManager) QueuePrefetch(
	ctx context.Context,
	retriever Retriever,
	query string,
	scopes []Scope,
) {
	if query == "" {
		return
	}

	// 异步预取
	p.executor.Submit(func() {
		bgCtx := context.Background()
		entries, err := retriever.Retrieve(bgCtx, Query{
			Scopes: scopes,
			Text:   query,
			Limit:  10,
		})
		if err != nil {
			return
		}

		// 格式化结果
		if len(entries) == 0 {
			return
		}

		var sb strings.Builder
		for _, e := range entries {
			sb.WriteString("- ")
			sb.WriteString(e.Content)
			sb.WriteString("\n")
		}

		p.mu.Lock()
		// 容量限制：超出时随机淘汰旧条目（简单 FIFO 近似）
		if len(p.cache) >= p.maxSize {
			for k := range p.cache {
				delete(p.cache, k)
				break // 只淘汰一个
			}
		}
		p.cache[query] = sb.String()
		p.mu.Unlock()
	})
}

// Get 获取预取缓存的上下文（消费后自动清除）。
func (p *PrefetchManager) Get(query string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	result, ok := p.cache[query]
	if ok {
		delete(p.cache, query)
	}
	return result
}

// Clear 清除所有预取缓存。
func (p *PrefetchManager) Clear() {
	p.mu.Lock()
	p.cache = make(map[string]string)
	p.mu.Unlock()
}

// Shutdown 关闭预取管理器。
func (p *PrefetchManager) Shutdown() {
	p.executor.Shutdown(3 * time.Second)
}
