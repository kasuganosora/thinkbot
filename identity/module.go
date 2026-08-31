package identity

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// fx Module — Identity 子系统依赖注入
// ============================================================================

// DefaultOrder 是 BindStage 在 Pipeline 中的默认执行顺序。
// 设为 3，确保在 CommandStage(5) 之前执行。
const DefaultOrder = 3

// Module 是 Identity 子系统的 fx 模块。
//
// 它提供：
//   - *BindService（授权码 & 身份映射服务）
//   - *BindStage（Pipeline Stage，拦截授权码消息）
//
// 启动时自动执行 AutoMigrate。
var Module = fx.Module("identity",
	fx.Provide(NewBindServiceFromDeps),
	fx.Provide(NewBindStageFromDeps),
	fx.Invoke(registerMigration),
)

// ServiceParams 是 fx 注入 BindService 的参数。
type ServiceParams struct {
	fx.In

	DB     *gorm.DB
	Logger *zap.SugaredLogger
}

// NewBindServiceFromDeps 通过 fx 注入创建 BindService。
func NewBindServiceFromDeps(p ServiceParams) *BindService {
	return New(p.DB, &logAdapter{l: p.Logger})
}

// BindStageParams 是 fx 注入 BindStage 的参数。
type BindStageParams struct {
	fx.In

	BindSvc *BindService
	Logger  *zap.SugaredLogger
	TP      trace.TracerProvider
}

// NewBindStageFromDeps 通过 fx 注入创建 BindStage。
func NewBindStageFromDeps(p BindStageParams) *BindStage {
	tp := p.TP
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	return NewBindStage(p.BindSvc, tp, p.Logger)
}

// migrationParams 用于注册数据库迁移钩子。
type migrationParams struct {
	fx.In

	Lifecycle fx.Lifecycle
	DB        *gorm.DB
	Logger    *zap.SugaredLogger
}

// registerMigration 注册数据库迁移生命周期钩子。
func registerMigration(p migrationParams) {
	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := Migrate(p.DB); err != nil {
				p.Logger.Errorw("identity: migrate failed", "err", err)
				return err
			}
			p.Logger.Infow("identity tables migrated")
			return nil
		},
	})
}

// AsStageInfo 将 *BindStage 包装为 core.StageInfo，使用默认 Order。
func AsStageInfo(stage *BindStage) core.StageInfo {
	return core.StageInfo{
		Stage:   stage,
		Order:   DefaultOrder,
		Enabled: true,
	}
}

// Migrate 执行 identity 子系统的数据库表迁移。
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&dao.BindCode{},
		&dao.IdentityMapping{},
	)
}

// logAdapter 将 *zap.SugaredLogger 适配为 Logger 接口。
type logAdapter struct {
	l *zap.SugaredLogger
}

func (a *logAdapter) Infow(msg string, kv ...any) { a.l.Infow(msg, kv...) }
func (a *logAdapter) Warnw(msg string, kv ...any) { a.l.Warnw(msg, kv...) }
