package retry

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/kasuganosora/thinkbot/util/log"
)

// Config 重试配置。
type Config struct {
	// MaxRetries 最大重试次数。
	//   -1 = 无限重试（直到成功或 context 取消）
	//    0 = 不重试（只执行一次）
	//    N = 最多重试 N 次（总共执行 N+1 次）
	MaxRetries int

	// Backoff 退避配置。为 nil 时使用固定 200ms 间隔。
	Backoff *Backoff

	// FixedInterval 无退避配置时的固定间隔，默认 200ms。
	// 仅当 Backoff 为 nil 时生效。
	FixedInterval time.Duration

	// RecoverPanic 是否自动 recover panic 并视为可重试错误，默认 true。
	RecoverPanic *bool

	// ShouldRetry 决定某个错误是否应该重试（可选）。
	//
	// 在每次 fn 返回非 nil error 后调用（context 取消除外，取消会立即返回）。
	//   - 返回 true：继续重试（受 MaxRetries 限制）
	//   - 返回 false：立即返回错误（不包装 "retry exhausted"）
	//
	// 如果为 nil，所有错误都会被重试（受 MaxRetries 限制）。
	//
	// 典型用法：
	//   - HTTP 客户端：仅对 5xx/429 重试
	//   - 流式连接：仅对看门狗超时重试，且只在未收到数据时重试
	ShouldRetry func(attempt int, err error) bool

	// OnRetry 每次重试前的回调（不包含首次执行）。
	OnRetry func(attempt int, err error, wait time.Duration)

	// GetRetryDelay 返回服务端建议的重试延迟（可选）。
	// 在每次 fn 返回错误后、计算退避时间时调用。
	// 如果返回值 > 计算出的退避时间，则使用返回值作为等待时间。
	// 典型用途：解析 HTTP 429 响应中的 Retry-After 头。
	GetRetryDelay func(err error) time.Duration

	// OnPanic 捕获到 panic 时的回调（可选）。
	OnPanic func(attempt int, r interface{}, stack []byte)
}

// DefaultConfig 返回默认配置：最多 3 次重试，固定 200ms 间隔，自动 recover panic。
func DefaultConfig() Config {
	return Config{
		MaxRetries:    3,
		FixedInterval: 200 * time.Millisecond,
	}
}

// sentinelPanicError 包装 panic 值为 error。
type panicError struct {
	value interface{}
	stack []byte
}

func (e *panicError) Error() string {
	return fmt.Sprintf("panic: %v", e.value)
}

// Result 记录重试执行的结果。
type Result struct {
	// Err 最终错误（成功时为 nil）。
	Err error
	// Attempts 总尝试次数（含首次执行，最小为 1）。
	Attempts int
	// Panics 发生的 panic 次数。
	Panics int
	// TotalElapsed 总耗时。
	TotalElapsed time.Duration
}

// Do 执行 fn，按 cfg 配置自动重试。
// name 用于日志标识。ctx 取消后立即停止重试并返回 ctx.Err()。
func Do(ctx context.Context, name string, cfg Config, fn func(ctx context.Context) error) Result {
	start := time.Now()

	backoff := DefaultBackoff()
	if cfg.Backoff != nil {
		backoff = *cfg.Backoff
	}

	recoverPanic := true
	if cfg.RecoverPanic != nil {
		recoverPanic = *cfg.RecoverPanic
	}

	var lastErr error //nolint:staticcheck // 保留以供未来扩展
	attempts := 0
	panics := 0

	for {
		attempts++

		// --- 执行 fn（带 panic recovery）---
		err := safeExec(ctx, name, recoverPanic, fn, attempts, cfg.OnPanic, &panics)
		lastErr = err
		_ = lastErr

		if err == nil {
			// 成功
			elapsed := time.Since(start)
			if attempts > 1 {
				log.Logger.Infow("retry succeeded",
					"name", name, "attempts", attempts, "panics", panics, "elapsed", elapsed)
			}
			return Result{
				Attempts:     attempts,
				Panics:       panics,
				TotalElapsed: elapsed,
			}
		}

		// context 取消，立即返回
		if ctxErr := ctx.Err(); ctxErr != nil {
			elapsed := time.Since(start)
			log.Logger.Warnw("retry aborted (context canceled)",
				"name", name, "attempts", attempts, "ctx_err", ctxErr, "last_err", err)
			return Result{
				Err:          fmt.Errorf("%w (last error: %v)", ctxErr, err),
				Attempts:     attempts,
				Panics:       panics,
				TotalElapsed: elapsed,
			}
		}

		// --- ShouldRetry 回调判断 ---
		if cfg.ShouldRetry != nil && !cfg.ShouldRetry(attempts, err) {
			elapsed := time.Since(start)
			log.Logger.Debugw("retry skipped by ShouldRetry",
				"name", name, "attempt", attempts, "err", err)
			return Result{
				Err:          err,
				Attempts:     attempts,
				Panics:       panics,
				TotalElapsed: elapsed,
			}
		}

		// --- 判断是否继续重试 ---
		if !shouldRetry(cfg.MaxRetries, attempts) {
			elapsed := time.Since(start)
			log.Logger.Errorw("retry exhausted",
				"name", name, "attempts", attempts, "panics", panics, "elapsed", elapsed, "err", err)
			return Result{
				Err:          fmt.Errorf("retry exhausted after %d attempts: %w", attempts, err),
				Attempts:     attempts,
				Panics:       panics,
				TotalElapsed: elapsed,
			}
		}

		// --- 计算退避时间 ---
		var wait time.Duration
		if cfg.Backoff != nil {
			wait = backoff.Calc(attempts)
		} else {
			wait = cfg.FixedInterval
			if wait <= 0 {
				wait = 200 * time.Millisecond
			}
		}

		// 如果服务端建议了更长的延迟（如 Retry-After），取较大值
		if cfg.GetRetryDelay != nil {
			if serverDelay := cfg.GetRetryDelay(err); serverDelay > wait {
				wait = serverDelay
			}
		}

		log.Logger.Warnw("retry scheduled",
			"name", name, "attempt", attempts, "wait", wait, "err", err)

		// 回调
		if cfg.OnRetry != nil {
			cfg.OnRetry(attempts, err, wait)
		}

		// --- 等待（可被 context 取消打断）---
		select {
		case <-ctx.Done():
			elapsed := time.Since(start)
			log.Logger.Warnw("retry wait interrupted (context canceled)",
				"name", name, "attempt", attempts)
			return Result{
				Err:          fmt.Errorf("%w (during retry wait)", ctx.Err()),
				Attempts:     attempts,
				Panics:       panics,
				TotalElapsed: elapsed,
			}
		case <-time.After(wait):
		}
	}
}

// DoSimple 是 Do 的快捷版本，使用默认配置。
func DoSimple(ctx context.Context, name string, fn func(ctx context.Context) error) error {
	res := Do(ctx, name, DefaultConfig(), fn)
	return res.Err
}

// safeExec 安全执行 fn，捕获 panic。
func safeExec(
	ctx context.Context,
	name string,
	recoverPanic bool,
	fn func(ctx context.Context) error,
	attempt int,
	onPanic func(int, interface{}, []byte),
	panicCount *int,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			*panicCount++

			log.Logger.Errorw("panic recovered during retry",
				"name", name, "attempt", attempt, "panic", r, "stack", string(stack))

			if onPanic != nil {
				onPanic(attempt, r, stack)
			}

			if recoverPanic {
				err = &panicError{value: r, stack: stack}
			} else {
				// 不 recover，重新 panic
				panic(r)
			}
		}
	}()

	return fn(ctx)
}

// shouldRetry 判断是否应该继续重试。
//
//	maxRetries = -1 → 无限重试
//	maxRetries =  0 → 不重试
//	maxRetries =  N → attempts <= N 时可重试（即第 N+1 次执行后不再重试）
func shouldRetry(maxRetries, attempts int) bool {
	if maxRetries < 0 {
		return true // 无限
	}
	return attempts <= maxRetries
}
