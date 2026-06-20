package command

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/agent/session"
)

// ============================================================================
// fx Module — Command 依赖注入
// ============================================================================

// DefaultOrder 是 CommandStage 在 Pipeline 中的默认执行顺序。
// 设为 5，确保在所有其他 Stage（Session=50, Memory=100, Prompt=200, LLM=500）之前执行。
const DefaultOrder = 5

// DefaultKeepRecent 是 /compact 命令默认保留的最近消息数。
const DefaultKeepRecent = 3

// Module 是 Command 子系统的 fx 模块。
//
// 它提供：
//   - *Registry（命令注册表）
//   - AdminChecker（默认拒绝所有 AdminOnly 命令，上层应通过 fx.Replace 覆盖）
//   - *CommandStage（Pipeline Stage，已注册内建命令）
//
// 上层应用示例：
//
//	app := fx.New(
//	    command.Module,
//	    // 覆盖默认 AdminChecker，接入实际认证系统
//	    fx.Provide(func(authSvc *auth.AuthService) command.AdminChecker {
//	        return command.AdminCheckerFunc(func(ctx context.Context, userID string) bool {
//	            user, err := authSvc.GetUserByUsername(ctx, userID)
//	            if err != nil {
//	                return false
//	            }
//	            return user.Role == auth.RoleAdmin
//	        })
//	    }),
//	    // 如果有 SessionManager，覆盖 SessionAccessor
//	    fx.Provide(func(mgr *session.SessionManager, resolver session.SessionResolver) command.SessionAccessor {
//	        return &command.SessionManagerAccessor{Mgr: mgr, Resolver: resolver}
//	    }),
//	    // 注册到 pipeline（非 fx 模式可直接使用 NewCommandStageWithBuiltins）
//	)
var Module = fx.Module("command",
	// 提供 Registry
	fx.Provide(NewRegistry),

	// 提供默认 AdminChecker（默认拒绝所有 AdminOnly 命令）
	fx.Provide(func() AdminChecker {
		return AdminCheckerFunc(func(_ context.Context, _, _ string) bool {
			return false
		})
	}),

	// 提供 CommandStage
	fx.Provide(NewCommandStageFromDeps),
)

// CommandStageParams 是 fx 注入 CommandStage 的参数。
type CommandStageParams struct {
	fx.In

	Registry *Registry
	Checker  AdminChecker
	Logger   *zap.SugaredLogger
	TP       trace.TracerProvider

	// SessionAccessor 可选。提供时会自动注册 /clear、/compact、/status 命令。
	Accessor *SessionManagerAccessor `optional:"true"`
}

// NewCommandStageFromDeps 通过 fx 注入创建 CommandStage 并注册内建命令。
func NewCommandStageFromDeps(p CommandStageParams) *CommandStage {
	tp := p.TP
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}

	stage := NewCommandStage("command", p.Registry, p.Checker, tp, p.Logger)

	// 注册内建命令
	RegisterBuiltins(p.Registry, p.Accessor, DefaultKeepRecent)

	return stage
}

// ============================================================================
// 便捷构造函数（非 fx 场景）
// ============================================================================

// NewCommandStageWithBuiltins 创建一个配置完整的 CommandStage，包含所有内建命令。
//
// 参数：
//   - checker: 管理员权限检查器（nil 时所有 AdminOnly 命令被拒绝）
//   - sessionMgr: Session 管理器（nil 时跳过 session 相关命令）
//   - resolver: Session 解析器（配合 sessionMgr 使用）
//   - keepRecent: /compact 默认保留消息数（<=0 时用默认值 3）
//   - tp: TracerProvider
//   - logger: 日志器
//
// 返回的 *CommandStage 已注册 /help、/clear、/compact、/status 命令，
// 可以直接作为 core.StageInfo 加入 Pipeline：
//
//	stages := []core.StageInfo{
//	    {Stage: cmdStage, Order: command.DefaultOrder, Enabled: true},
//	    {Stage: llmStage, Order: 100, Enabled: true},
//	}
func NewCommandStageWithBuiltins(
	checker AdminChecker,
	sessionMgr *session.SessionManager,
	resolver session.SessionResolver,
	keepRecent int,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *CommandStage {
	registry := NewRegistry()

	var accessor SessionAccessor
	if sessionMgr != nil {
		accessor = &SessionManagerAccessor{
			Mgr:      sessionMgr,
			Resolver: resolver,
		}
	}

	RegisterBuiltins(registry, accessor, keepRecent)

	return NewCommandStage("command", registry, checker, tp, logger)
}

// ============================================================================
// StageInfo 便捷函数
// ============================================================================

// AsStageInfo 将 *CommandStage 包装为 core.StageInfo，使用默认 Order。
func AsStageInfo(stage *CommandStage) core.StageInfo {
	return core.StageInfo{
		Stage:   stage,
		Order:   DefaultOrder,
		Enabled: true,
	}
}

// AsStageInfoWithOrder 将 *CommandStage 包装为 core.StageInfo，使用指定 Order。
func AsStageInfoWithOrder(stage *CommandStage, order int) core.StageInfo {
	return core.StageInfo{
		Stage:   stage,
		Order:   order,
		Enabled: true,
	}
}

// ============================================================================
// fx 辅助：注册 CommandStage 到 pipeline_stages 分组
// ============================================================================

// ProvideStage 注册 CommandStage 到 fx 的 "pipeline_stages" 分组。
// 用于 fx 应用自动将 CommandStage 加入 Pipeline。
//
// 用法：
//
//	fx.New(
//	    command.Module,
//	    command.ProvideStage(5),  // order=5
//	)
func ProvideStage(order int) fx.Option {
	if order <= 0 {
		order = DefaultOrder
	}
	return pipeline.ProvideStage(func(stage *CommandStage) *CommandStage {
		return stage
	}, order)
}
