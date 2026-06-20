package stats

import (
	"context"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// fx Module — stats 依赖注入
// ============================================================================

// StatsParams 是创建 Recorder 所需的依赖。
type StatsParams struct {
	fx.In

	DB     *gorm.DB
	Logger *zap.SugaredLogger
}

// Module 是 stats 的 fx 模块。
//
// 提供：
//   - *stats.Recorder（实现 llm.UsageRecorder 接口）
//   - 注册为 llm.UsageRecorder（供各 Stage 可选注入）
//
// 生命周期：
//   - OnStart: AutoMigrate 表 + 启动后台写入 goroutine
//   - OnStop: 停止后台 goroutine，刷新剩余指标
var Module = fx.Module("stats",
	fx.Provide(NewRecorderModule),
	fx.Invoke(RegisterLifecycle),
)

// NewRecorderModule 创建 Recorder 并注册为 llm.UsageRecorder。
func NewRecorderModule(p StatsParams) (*Recorder, llm.UsageRecorder) {
	r := NewRecorder(p.DB, p.Logger)
	return r, llm.UsageRecorder(r)
}

// LifecycleParams 用于注册生命周期钩子。
type LifecycleParams struct {
	fx.In

	Recorder *Recorder
	Lifecycle fx.Lifecycle
	Logger   *zap.SugaredLogger
}

// RegisterLifecycle 在 fx.Module 中通过 Invoke 调用。
// 也可由上层 app 在 app.Start() 前手动调用。
func RegisterLifecycle(p LifecycleParams) {
	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// AutoMigrate
			if err := p.Recorder.db.AutoMigrate(&dao.UsageDaily{}); err != nil {
				p.Logger.Errorw("stats: migrate failed", "err", err)
				return err
			}
			p.Recorder.Start()
			p.Logger.Infow("stats recorder started")
			return nil
		},
		OnStop: func(ctx context.Context) error {
			p.Recorder.Stop()
			p.Logger.Infow("stats recorder stopped")
			return nil
		},
	})
}
