package workflow

import (
	"time"

	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/subagent"
)

// ============================================================================
// Wire — 组合根
//
// 对齐 bot.SetupSkills 模式，提供统一的 Setup 函数。
// 上层（如 agent/bot/bot.go）只需调用 Setup + RegisterTools。
// ============================================================================

// WireConfig 是 Setup 的配置参数。
type WireConfig struct {
	// Provider LLM Provider（从主 Agent 的 LLMBundle 继承）。
	Provider llm.Provider

	// Model 模型名称（如 "glm-5.2"）。
	Model string

	// DB 数据库实例（可为 nil，则使用纯内存模式）。
	DB *gorm.DB

	// Logger 日志器（可为 nil，则使用 noop logger）。
	Logger *zap.SugaredLogger

	// TracerProvider OpenTelemetry TracerProvider（可为 nil，则使用 noop）。
	TracerProvider trace.TracerProvider

	// MaxParallel 默认最大并行度（可选，默认 3）。
	// 已废弃：改由 config.Store 管理，此处保留向后兼容。
	// 当 Store 为 nil 时，MaxParallel > 0 才生效。
	MaxParallel int

	// SAOpts SubAgent 默认选项（可选）。
	SAOpts []subagent.Option

	// Store 全局配置中心（可为 nil，则使用 config.DefaultWorkflowConfig()）。
	Store *config.Store

	// EventBus 旁路事件总线（可为 nil，则不发布事件）。
	// Web SSE 订阅端通过 workflow_id 订阅实时进度事件。
	EventBus outbound.EventBus
}

// EngineConfig 是从 config.Store 解析出的引擎运行时配置。
// 由 Setup() 内部创建，传递给 Analyzer / Scheduler / Executor。
type EngineConfig struct {
	MaxParallel         int
	MaxRetries          int
	MaxIterations       int
	RetryInitial        time.Duration
	RetryMax            time.Duration
	ScheduleInterval    time.Duration
	AnalyzerTemperature float64
	AnalyzerMaxTokens   int
}

// Setup 创建并装配工作流引擎的所有组件。
//
// 反嵌套设计：此函数创建的 SubAgentManager 是 workflow 引擎私有的，
// 不经过主 Agent 的 ToolManager，因此 workflow 内部的 SubAgent 无法
// 访问 workflow 工具，避免无限嵌套。主 Agent 的 ToolManager 通过
// RegisterTools 注册的是工具入口，调用 Submit 后进入异步执行管道，
// 而执行管道内的 SubAgent 是隔离的。
//
// 返回：
//   - *Manager: 工作流管理器（统一入口）
//   - *subagent.SubAgentManager: SubAgent 管理器（调用方需在适当时机调用 CloseAll）
//
// 使用示例：
//
//	wfMgr, saMgr := workflow.Setup(workflow.WireConfig{
//	    Provider:       bundle.Main,
//	    Model:          bundle.MainDef.Model,
//	    DB:             gormDB,
//	    Logger:         logger,
//	    TracerProvider: tp,
//	    Store:          configStore,
//	})
//	defer saMgr.CloseAll()
//	workflow.RegisterTools(toolMgr, wfMgr)
func Setup(cfg WireConfig) (*Manager, *subagent.SubAgentManager) {
	tp := cfg.TracerProvider
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}

	// 从 config.Store 读取引擎配置，Store 为 nil 时使用默认值
	ec := resolveEngineConfig(cfg.Store, cfg.MaxParallel)

	// 1. SubAgent 管理器
	saMgr := subagent.NewSubAgentManager(cfg.Provider, cfg.Model, cfg.SAOpts...)

	// 2. 持久化仓储
	repo := NewRepository(cfg.DB, cfg.Logger)

	// 3. 需求分析器
	analyzer := NewAnalyzer(saMgr, tp, ec, cfg.Logger)

	// 4. 节点执行器
	executor := NewExecutor(saMgr, tp, cfg.Logger)

	// 5. 工作流管理器
	manager := NewManager(repo, analyzer, executor, tp, ec, cfg.Logger, cfg.EventBus)

	return manager, saMgr
}

// resolveEngineConfig 从 config.Store 构建 EngineConfig。
// store 为 nil 时使用全部默认值；maxParallelFallback > 0 时覆盖 MaxParallel（向后兼容）。
func resolveEngineConfig(store *config.Store, maxParallelFallback int) EngineConfig {
	if store == nil {
		ec := engineConfigFromWorkflowConfig(config.DefaultWorkflowConfig())
		if maxParallelFallback > 0 {
			ec.MaxParallel = maxParallelFallback
		}
		return ec
	}

	wc := config.NewBuilder(store, nil).GetWorkflowConfig()
	ec := engineConfigFromWorkflowConfig(wc)
	if maxParallelFallback > 0 {
		ec.MaxParallel = maxParallelFallback
	}
	return ec
}

func engineConfigFromWorkflowConfig(wc config.WorkflowConfig) EngineConfig {
	return EngineConfig{
		MaxParallel:         wc.MaxParallel,
		MaxRetries:          wc.MaxRetries,
		MaxIterations:       wc.MaxIterations,
		RetryInitial:        time.Duration(wc.RetryInitialMS) * time.Millisecond,
		RetryMax:            time.Duration(wc.RetryMaxMS) * time.Millisecond,
		ScheduleInterval:    time.Duration(wc.ScheduleIntervalMS) * time.Millisecond,
		AnalyzerTemperature: wc.AnalyzerTemperature,
		AnalyzerMaxTokens:   wc.AnalyzerMaxTokens,
	}
}
