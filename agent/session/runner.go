package session

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ============================================================================
// SessionRunner — Per-Session 并发控制
//
// Per-Session 并发控制状态机：
// 每个 Session 拥有独立的 Runner，保证同一 Session 内的消息串行处理。
// 多个消息到达同一 Session 时，后来的消息排队等待。
//
// 状态机：
//
//	idle → Run(fn) → busy → fn完成 → idle
//	                       ↓
//	                 cancel → 中断回调
//
// 使用方式：
//
//	runner := mgr.GetOrCreateRunner(sessionID)
//	err := runner.Run(ctx, func(ctx context.Context) error {
//	    // 串行处理此 session 的消息
//	    return processMessage(ctx)
//	})
// ============================================================================

// RunnerState 表示 Runner 的状态。
type RunnerState int

const (
	RunnerStateIdle RunnerState = iota // 空闲，可接受新任务
	RunnerStateBusy                    // 正在执行任务
)

// String 返回状态的可读表示。
func (s RunnerState) String() string {
	switch s {
	case RunnerStateIdle:
		return "idle"
	case RunnerStateBusy:
		return "busy"
	default:
		return "unknown"
	}
}

// ErrSessionBusy 当 Session 正在处理另一条消息且设置了非阻塞模式时返回。
var ErrSessionBusy = errors.New("session: runner is busy")

// ErrSessionCancelled 当 Runner 被取消时返回。
var ErrSessionCancelled = errors.New("session: runner cancelled")

// SessionRunner 保证同一个 Session 内的操作串行执行。
//
// 主要特性：
//   - 串行执行：同一 Session 的消息排队处理，不会并发
//   - 可取消：支持取消正在执行的操作
//   - 非阻塞模式：可选地在 busy 时直接返回 ErrSessionBusy
//   - 超时支持：通过 context 控制执行时长
//   - 等待队列：排队等待的消息有最大数量限制
type SessionRunner struct {
	mu sync.Mutex

	state RunnerState
	cond  *sync.Cond

	// 当前执行的 context 取消函数
	currentCancel context.CancelFunc

	// 等待队列长度
	queueDepth    int
	maxQueueDepth int

	// 统计
	totalRuns int64
	lastRunAt time.Time
}

// RunnerConfig 配置 SessionRunner。
type RunnerConfig struct {
	// MaxQueueDepth 最大排队等待数量。
	// 超过此数量的请求返回 ErrSessionBusy。
	// 0 = 不限制（默认）。
	MaxQueueDepth int

	// DefaultTimeout 默认执行超时。
	// 0 = 不设置超时（使用传入的 context）。
	DefaultTimeout time.Duration
}

// DefaultRunnerConfig 返回默认 Runner 配置。
func DefaultRunnerConfig() RunnerConfig {
	return RunnerConfig{
		MaxQueueDepth: 32,
	}
}

// NewSessionRunner 创建 Session Runner。
func NewSessionRunner(config RunnerConfig) *SessionRunner {
	r := &SessionRunner{
		maxQueueDepth: config.MaxQueueDepth,
	}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// State 返回当前状态。
func (r *SessionRunner) State() RunnerState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// QueueDepth 返回当前排队等待数。
func (r *SessionRunner) QueueDepth() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.queueDepth
}

// Run 串行执行 fn。
//
// 如果 Runner 正忙，调用者阻塞等待直到前面的任务完成。
// fn 在执行期间可以通过 ctx.Done() 感知取消。
func (r *SessionRunner) Run(ctx context.Context, fn func(context.Context) error) error {
	// 进入临界区排队
	if err := r.acquire(ctx); err != nil {
		return err
	}
	defer r.release()

	// 创建可取消的 context
	execCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.currentCancel = cancel
	r.mu.Unlock()
	defer cancel()

	return fn(execCtx)
}

// TryRun 尝试执行 fn，如果 Runner 正忙则立即返回 ErrSessionBusy。
func (r *SessionRunner) TryRun(ctx context.Context, fn func(context.Context) error) error {
	r.mu.Lock()
	if r.state == RunnerStateBusy {
		r.mu.Unlock()
		return ErrSessionBusy
	}
	r.state = RunnerStateBusy
	execCtx, cancel := context.WithCancel(ctx)
	r.currentCancel = cancel
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.state = RunnerStateIdle
		r.currentCancel = nil
		r.cond.Signal()
		r.mu.Unlock()
		cancel()
	}()

	return fn(execCtx)
}

// Cancel 取消当前正在执行的操作。
func (r *SessionRunner) Cancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.currentCancel != nil {
		r.currentCancel()
	}
}

// IsBusy 返回 Runner 是否正在执行任务。
func (r *SessionRunner) IsBusy() bool {
	return r.State() == RunnerStateBusy
}

// acquire 获取执行锁（阻塞等待前面的任务完成）。
func (r *SessionRunner) acquire(ctx context.Context) error {
	r.mu.Lock()

	// 检查 context 是否已取消
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}

	// 等待空闲
	for r.state == RunnerStateBusy {
		// 检查队列深度限制
		if r.maxQueueDepth > 0 && r.queueDepth >= r.maxQueueDepth {
			r.mu.Unlock()
			return ErrSessionBusy
		}

		r.queueDepth++

		// 使用 goroutine + channel 实现 context 感知的 cond.Wait
		done := make(chan struct{})
		go func() {
			r.mu.Lock()
			r.cond.Wait()
			r.queueDepth--
			close(done)
			r.mu.Unlock()
		}()

		select {
		case <-ctx.Done():
			// context 取消，唤醒所有等待者
			r.mu.Lock()
			r.cond.Broadcast()
			r.mu.Unlock()
			return ctx.Err()
		case <-done:
			// cond 被唤醒，继续检查状态
			r.mu.Lock()
		}
	}

	r.state = RunnerStateBusy
	execCtx, cancel := context.WithCancel(ctx)
	r.currentCancel = cancel
	r.totalRuns++
	r.lastRunAt = time.Now()
	r.mu.Unlock()

	// 将 execCtx 传递给调用者需要特殊处理
	// 这里我们通过返回 cancel 来确保后续可以取消
	_ = execCtx
	_ = cancel
	return nil
}

// release 释放执行锁。
func (r *SessionRunner) release() {
	r.mu.Lock()
	r.state = RunnerStateIdle
	if r.currentCancel != nil {
		r.currentCancel()
		r.currentCancel = nil
	}
	r.cond.Signal()
	r.mu.Unlock()
}

// ============================================================================
// SessionRunnerManager — 管理 per-session Runner 实例
// ============================================================================

// SessionRunnerManager 为每个 Session ID 维护独立的 SessionRunner。
type SessionRunnerManager struct {
	mu      sync.RWMutex
	runners map[string]*SessionRunner
	config  RunnerConfig
}

// NewSessionRunnerManager 创建 Runner 管理器。
func NewSessionRunnerManager(config RunnerConfig) *SessionRunnerManager {
	return &SessionRunnerManager{
		runners: make(map[string]*SessionRunner),
		config:  config,
	}
}

// GetOrCreate 获取或创建指定 Session 的 Runner。
func (m *SessionRunnerManager) GetOrCreate(sessionID string) *SessionRunner {
	m.mu.RLock()
	runner, ok := m.runners[sessionID]
	m.mu.RUnlock()
	if ok {
		return runner
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double check
	if runner, ok := m.runners[sessionID]; ok {
		return runner
	}

	runner = NewSessionRunner(m.config)
	m.runners[sessionID] = runner
	return runner
}

// Get 获取指定 Session 的 Runner（不存在返回 nil）。
func (m *SessionRunnerManager) Get(sessionID string) *SessionRunner {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.runners[sessionID]
}

// Delete 删除指定 Session 的 Runner。
func (m *SessionRunnerManager) Delete(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runners, sessionID)
}

// ActiveRunners 返回活跃 Runner 数量。
func (m *SessionRunnerManager) ActiveRunners() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.runners)
}

// BusyRunners 返回正在执行的 Runner 数量。
func (m *SessionRunnerManager) BusyRunners() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, r := range m.runners {
		if r.IsBusy() {
			count++
		}
	}
	return count
}

// Cleanup 清理空闲的 Runner。
func (m *SessionRunnerManager) Cleanup() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for id, r := range m.runners {
		if !r.IsBusy() && r.QueueDepth() == 0 {
			delete(m.runners, id)
			removed++
		}
	}
	return removed
}
