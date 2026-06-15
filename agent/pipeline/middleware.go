package pipeline

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Middleware — Stage 拦截器
// ============================================================================

// Middleware 是一个函数，接收一个 Stage 并返回一个包装后的 Stage。
// 可以在 Stage 执行前后插入逻辑。
type Middleware func(next core.Stage) core.Stage

// WithMiddleware 将一个或多个 Middleware 应用到 Stage 上。
// Middleware 按顺序从外到内包装（第一个 Middleware 最先执行）。
func WithMiddleware(stage core.Stage, mws ...Middleware) core.Stage {
	result := stage
	// 从后往前包装，使得第一个 middleware 在最外层
	for i := len(mws) - 1; i >= 0; i-- {
		result = mws[i](result)
	}
	return result
}

// ============================================================================
// 内置 Middleware
// ============================================================================

// RecoveryMiddleware 返回一个 panic 恢复中间件。
// 当 Stage panic 时，恢复 panic 并返回 PipelineError。
func RecoveryMiddleware() Middleware {
	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name(),
			Fn: func(ctx context.Context, env *core.Envelope) (result *core.Envelope, retErr error) {
				defer func() {
					if r := recover(); r != nil {
						result = env
						retErr = &core.PipelineError{
							Stage:   next.Name(),
							Message: "panic recovered",
							Cause:   fmt.Errorf("%v", r),
						}
					}
				}()
				return next.Process(ctx, env)
			},
		}
	}
}

// TimeoutMiddleware 返回一个超时控制中间件。
// 如果 Stage 在指定时间内未完成，返回 context.DeadlineExceeded。
func TimeoutMiddleware(d time.Duration) Middleware {
	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name(),
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				ctx, cancel := context.WithTimeout(ctx, d)
				defer cancel()

				type result struct {
					env *core.Envelope
					err error
				}
				ch := make(chan result, 1)
				go func() {
					e, err := next.Process(ctx, env)
					ch <- result{e, err}
				}()

				select {
				case r := <-ch:
					return r.env, r.err
				case <-ctx.Done():
					return env, &core.PipelineError{
						Stage:   next.Name(),
						Message: "timeout exceeded",
						Cause:   ctx.Err(),
					}
				}
			},
		}
	}
}

// LoggingMiddleware 返回一个日志记录中间件。
// 在 Stage 执行前后记录结构化日志。
func LoggingMiddleware(logger *zap.SugaredLogger) Middleware {
	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name(),
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				start := time.Now()
				logger.Debugw("stage starting",
					"stage", next.Name(),
					"message_id", env.Message.ID)

				result, err := next.Process(ctx, env)

				duration := time.Since(start)
				if err != nil {
					logger.Warnw("stage completed with error",
						"stage", next.Name(),
						"message_id", env.Message.ID,
						"duration", duration,
						"err", err)
				} else {
					logger.Debugw("stage completed",
						"stage", next.Name(),
						"message_id", env.Message.ID,
						"duration", duration)
				}
				return result, err
			},
		}
	}
}
