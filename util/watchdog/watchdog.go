package watchdog

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ErrWatchdogTimeout 是看门狗超时时的 sentinel error。
// 可通过 errors.Is(err, watchdog.ErrWatchdogTimeout) 判断。
var ErrWatchdogTimeout = errors.New("watchdog timeout")

// Watchdog 看门狗定时器。
// 在 timeout 时间内如果没有调用 Feed()，会自动取消其管理的 context。
type Watchdog struct {
	parent  context.Context
	timeout time.Duration
	name    string
	logger  *zap.SugaredLogger // 从 parent context 派生，携带 trace_id

	mu        sync.Mutex
	timer     *time.Timer
	generation uint64 // 递增代际：每次（重）设定时器时 +1，用于忽略已失效的过期触发
	ctx       context.Context
	cancel    context.CancelFunc
	stopped   bool
	timedOut  atomic.Bool
	onTimeout func()
}

// New 创建一个看门狗，继承 parent context。
// timeout 为未被投喂的最大等待时间；parent 为 nil 时使用 context.Background()。
func New(parent context.Context, timeout time.Duration) *Watchdog {
	return NewWithName(parent, timeout, "watchdog")
}

// NewWithName 与 New 相同，但指定名称用于日志标识。
func NewWithName(parent context.Context, timeout time.Duration, name string) *Watchdog {
	if parent == nil {
		parent = context.Background()
	}
	w := &Watchdog{
		parent:  parent,
		timeout: timeout,
		name:    name,
		logger:  traceid.L(parent),
	}
	w.resetCtx()
	w.timer = time.AfterFunc(timeout, func() { w.fire(0) })
	w.logger.Infow("watchdog started", "name", name, "timeout", timeout)
	return w
}

// NewWithCallback 与 New 相同，但额外在超时触发时调用 onTimeout（在 timer goroutine 中执行）。
func NewWithCallback(parent context.Context, timeout time.Duration, onTimeout func()) *Watchdog {
	return NewWithNameAndCallback(parent, timeout, "watchdog", onTimeout)
}

// NewWithNameAndCallback 创建带名称和回调的看门狗。
func NewWithNameAndCallback(parent context.Context, timeout time.Duration, name string, onTimeout func()) *Watchdog {
	if parent == nil {
		parent = context.Background()
	}
	w := &Watchdog{
		parent:    parent,
		timeout:   timeout,
		name:      name,
		logger:    traceid.L(parent),
		onTimeout: onTimeout,
	}
	w.resetCtx()
	w.timer = time.AfterFunc(timeout, func() { w.fire(0) })
	w.logger.Infow("watchdog started", "name", name, "timeout", timeout)
	return w
}

// Context 返回看门狗管理的 context。超时或调用 Stop() 后此 context 会被取消。
func (w *Watchdog) Context() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ctx
}

// Feed 投喂看门狗，重置超时计时器。
func (w *Watchdog) Feed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	// 已超时：不再重设定时器，保持 TimedOut() == true 语义，
	// 避免超时后继续 Feed 导致周期重复触发 fire/onTimeout（4824/4831）。
	if w.timedOut.Load() {
		return
	}
	// 如果 context 因 parent 取消而失效（非超时），则重建 context 重新开始。
	if w.ctx.Err() != nil {
		w.resetCtx()
	}
	w.armTimer(w.timeout)
	w.logger.Debugw("watchdog fed", "name", w.name, "timeout", w.timeout)
}

// FeedWithTimeout 投喂看门狗并动态修改超时时间。
func (w *Watchdog) FeedWithTimeout(timeout time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	old := w.timeout
	w.timeout = timeout
	// 已超时：仅更新超时值，不再重设定时器，保持 TimedOut() == true 语义。
	if w.timedOut.Load() {
		w.logger.Debugw("watchdog fed (timeout updated, but already timed out)",
			"name", w.name, "old_timeout", old, "new_timeout", timeout)
		return
	}
	if w.ctx.Err() != nil {
		w.resetCtx()
	}
	w.armTimer(timeout)
	w.logger.Debugw("watchdog fed (timeout updated)",
		"name", w.name, "old_timeout", old, "new_timeout", timeout)
}

// armTimer 停掉旧定时器并以新的代际重新设定（调用方须持有锁）。
// 通过递增 generation，使任何在重设前已触发/正在触发的旧定时器回调被 fire 忽略，
// 从根本上消除 time.Timer 复用竞态（4824）。
func (w *Watchdog) armTimer(d time.Duration) {
	w.generation++
	gen := w.generation
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(d, func() {
		w.fire(gen)
	})
}

// Stop 停止看门狗并取消 context。
// cancel 为 true 时主动取消 context；为 false 时仅停止定时器、保留 context。
func (w *Watchdog) Stop(cancel bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.timer.Stop()
	if cancel {
		w.cancel()
	}
	w.stopped = true
	w.logger.Infow("watchdog stopped", "name", w.name, "cancel", cancel)
}

// Timeout 返回当前配置的超时时长。
func (w *Watchdog) Timeout() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.timeout
}

// Name 返回看门狗名称。
func (w *Watchdog) Name() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.name
}

// TimedOut 返回看门狗是否已因超时触发（而非被外部 Stop 或 parent context 取消）。
// 一旦触发，值保持为 true 直到 Watchdog 被 Stop。
// 可用于区分 context 被 cancel 的原因：
//   - TimedOut() == true  → 看门狗超时（数据流卡住）
//   - TimedOut() == false → 外部取消（用户主动取消 / parent context 取消）
func (w *Watchdog) TimedOut() bool {
	return w.timedOut.Load()
}

// --- 内部方法 ---

// resetCtx 从 parent 派生新的 context，需在持有锁时调用。
func (w *Watchdog) resetCtx() {
	if w.cancel != nil {
		w.cancel()
	}
	// 重置超时标志：重建 context 后视为"重新开始"
	w.timedOut.Store(false)
	ctx, cancel := context.WithCancel(w.parent)
	w.ctx = ctx
	w.cancel = cancel

	// 将 goroutine 中使用的字段拷贝到局部变量，避免 data race
	name := w.name
	logger := w.logger
	parent := w.parent

	// 监听 parent 取消，自动传播
	go func() {
		select {
		case <-ctx.Done():
		case <-parent.Done():
			cancel()
			logger.Warnw("watchdog parent context canceled",
				"name", name)
		}
	}()
}

// fire 是定时器到期时的回调。gen 为本次定时触发所属代际，
// 与当前 generation 不一致（已被后续 Feed 重设）或已停止时直接忽略，
// 避免 Timer 复用竞态导致的误杀与重复触发（4824/4831）。
func (w *Watchdog) fire(gen uint64) {
	w.mu.Lock()
	if w.stopped || gen != w.generation {
		w.mu.Unlock()
		return
	}
	w.cancel()
	w.timedOut.Store(true)
	cb := w.onTimeout
	name := w.name
	w.mu.Unlock()

	w.logger.Warnw("watchdog timeout! context canceled",
		"name", name)

	if cb != nil {
		cb()
	}
}
