package workflow

import (
	"time"

	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"gorm.io/gorm"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
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

	// ModelDef 当前工作流使用的主模型定义（来自 LLM bundle 的 MainDef）。
	// 分析器最大输出 token 的唯一来源：直接取模型自身的 MaxTokens，
	// 换模型即自动跟随，不存在第二处配置。
	// 可为空（nil ModelDef），此时走代码兜底 analyzerMaxTokensFallback。
	ModelDef *config.ModelDef

	// EventBus 旁路事件总线（可为 nil，则不发布事件）。
	// Web SSE 订阅端通过 workflow_id 订阅实时进度事件。
	EventBus outbound.EventBus

	// ToolMgr 主 Agent 的工具解析器（可选）。
	// 非 nil 时，workflow 引擎内部的 SubAgent（需求分析 / 节点执行 / 审查）
	// 将继承主 Agent 在 SubAgent 场景下可用的工具（exec / 读 / 写 / 列目录 / 搜索等），
	// 从而能像主 Agent 的 SubAgent 一样操作工作空间——例如「审查并修复代码」类目标模式
	// 任务可真正读取仓库、运行 go build/vet、落地修改，而非只会「缺少源代码」地空谈。
	// 经 scope 自动排除 workflow 与 spawn 工具，不会形成「子 Agent 再触发工作流 / 再 spawn」
	// 的套娃；记忆工具同为 private/group scope，亦被排除，避免工作流污染长期记忆。
	// nil 表示保持旧行为（纯 LLM，无工具），即本修复前的状态。
	ToolMgr *agenttools.ToolManager

	// ToolBotID 解析工具时使用的 BotID，决定内部 SubAgent 操作哪个 bot 的工作空间。
	// 通常传当前 bot 的 id，使其与主 Agent 共用同一 per-bot 沙箱（同一份仓库/目录）。
	ToolBotID string
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
	// AnalyzerStuckTimeout 需求分析器流式 LLM 调用的卡死看门狗阈值。
	// 0 表示使用 subagent 包默认（180s）。由 DelegateStream 读取，作为「判卡死」阈值；
	// 硬上限 = 该值 ×3（派生，不写死）。看门狗判断真卡死而非固定超时。
	AnalyzerStuckTimeout time.Duration
	// AnalyzerMaxDuration 需求分析阶段「整轮总时长上限」。
	// 兜底防止 GLM 退化时分析器无限重试把「分析中」拖成数十分钟黑洞；
	// 超过该时长分析阶段整体失败（明确报错），前端可立即看到结果而非一直转圈。
	AnalyzerMaxDuration time.Duration
	// GoalMaxIterations 目标模式（闭环循环）全局最大迭代轮数。0 表示代码兜底默认 5。
	GoalMaxIterations int
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
	ec := resolveEngineConfig(cfg.Store, cfg.MaxParallel, cfg.ModelDef)

	// 1. SubAgent 管理器
	// 默认输出上限跟随当前模型配置（如 glm-5.2=128K）；analyzer 显式传的 cap 会覆盖此默认。
	saOpts := cfg.SAOpts
	if cfg.ModelDef != nil && cfg.ModelDef.MaxTokens > 0 {
		saOpts = append(saOpts, subagent.WithMaxTokens(cfg.ModelDef.MaxTokens))
	}
	saMgr := subagent.NewSubAgentManager(cfg.Provider, cfg.Model, saOpts...)

	// 若提供了主 Agent 工具解析器，让 workflow 内部 SubAgent 继承工作空间工具，
	// 使其能像主 Agent 的 SubAgent 一样读文件、跑命令、改代码（如「审查并修复代码」）。
	// workflow 工具（scope=private/group）与 spawn 工具在 IsSubagent 场景被自动排除，
	// 不会形成套娃；记忆工具同为 private/group，亦被排除，避免工作流污染长期记忆。
	if cfg.ToolMgr != nil {
		saMgr.SetToolResolver(cfg.ToolMgr, agenttools.ToolSessionContext{BotID: cfg.ToolBotID})
		// 代码类任务（读多文件 + go build/vet + 多轮修改）放宽单步预算，
		// 且子 Agent 重任务常超默认 120s，放宽委托超时到 10 分钟（对齐主 Agent 子 Agent）。
		saMgr.SetDefaultToolSteps(25)
		saMgr.SetDelegateTimeout(10 * time.Minute)
		cfg.Logger.Debugw("workflow engine: tool resolver attached to internal subagents",
			"botID", cfg.ToolBotID)
	}

	// 2. 持久化仓储
	repo := NewRepository(cfg.DB, cfg.Logger)

	// 3. 需求分析器
	//
	// 显式记录分析器输出预算：该值过小会导致 DAG JSON 被硬截断（表现为
	// "unexpected end of JSON input" 连续重试失败），而排查时若无日志只能翻 DB。
	maxTokensSource := "model:" + modelDefName(cfg.ModelDef)
	if cfg.ModelDef == nil || cfg.ModelDef.MaxTokens <= 0 {
		maxTokensSource = "code-fallback"
	}
	cfg.Logger.Infow("workflow engine: analyzer configured",
		"max_tokens", ec.AnalyzerMaxTokens,
		"max_tokens_source", maxTokensSource,
		"temperature", ec.AnalyzerTemperature,
		"note", "max_tokens follows the bot's selected model; shared by reasoning + JSON body")
	analyzer := NewAnalyzer(saMgr, tp, ec, cfg.Logger)

	// 4. 节点执行器
	executor := NewExecutor(saMgr, tp, cfg.Logger)

	// 5. 工作流管理器
	manager := NewManager(repo, analyzer, executor, tp, ec, cfg.Logger, cfg.EventBus)

	return manager, saMgr
}

// resolveEngineConfig 从 config.Store 构建 EngineConfig。
// store 为 nil 时使用全部默认值；maxParallelFallback > 0 时覆盖 MaxParallel（向后兼容）。
// modelDef 提供当前模型定义，用于推导分析器最大输出 token（见 engineConfigFromWorkflowConfig）。
func resolveEngineConfig(store *config.Store, maxParallelFallback int, modelDef *config.ModelDef) EngineConfig {
	if store == nil {
		ec := engineConfigFromWorkflowConfig(config.DefaultWorkflowConfig(), modelDef)
		if maxParallelFallback > 0 {
			ec.MaxParallel = maxParallelFallback
		}
		return ec
	}

	wc := config.NewBuilder(store, nil).GetWorkflowConfig()
	ec := engineConfigFromWorkflowConfig(wc, modelDef)
	if maxParallelFallback > 0 {
		ec.MaxParallel = maxParallelFallback
	}
	return ec
}

// analyzerMaxTokensFallback 仅在「拿不到模型定义」时兜底（如未指派模型、
// provider 配置缺失）。正常路径一律跟随 bot 所选模型的 MaxTokens。
//
// 该预算由「思考(reasoning) + JSON 正文」共享，而非正文独占：思考型模型
// （GLM-4.6+ 服务端默认开启 thinking）会先产出 reasoning 再写正文，两者
// 都从 max_tokens 里扣。取值过小的直接后果是 DAG JSON 中途被硬截断，
// 解析报 "unexpected end of JSON input" 并耗尽全部重试。
const analyzerMaxTokensFallback = 32768

// analyzerMaxTokens 推导分析器最大输出 token。
//
// 单一来源：bot 所选模型的 MaxTokens（provider.<x>.models[].maxTokens
// → ModelDef.MaxTokens）。此前另有 workflow.analyzer_max_tokens 独立旋钮，
// 且优先级高于模型能力，被播种成 8192 后把 glm-5.2（128K）压到 8192，
// 导致 DAG JSON 被截断——两套配置各说各话，故取消独立旋钮。
// 换模型时预算自动跟随，无需再改第二处配置。
// modelDefName 返回模型名，用于日志标注预算来源。
func modelDefName(modelDef *config.ModelDef) string {
	if modelDef == nil || modelDef.Model == "" {
		return "unknown"
	}
	return modelDef.Model
}

func analyzerMaxTokens(modelDef *config.ModelDef) int {
	if modelDef != nil && modelDef.MaxTokens > 0 {
		return modelDef.MaxTokens
	}
	return analyzerMaxTokensFallback
}

func engineConfigFromWorkflowConfig(wc config.WorkflowConfig, modelDef *config.ModelDef) EngineConfig {
	return EngineConfig{
		MaxParallel:         wc.MaxParallel,
		MaxRetries:          wc.MaxRetries,
		MaxIterations:       wc.MaxIterations,
		RetryInitial:        time.Duration(wc.RetryInitialMS) * time.Millisecond,
		RetryMax:            time.Duration(wc.RetryMaxMS) * time.Millisecond,
		ScheduleInterval:    time.Duration(wc.ScheduleIntervalMS) * time.Millisecond,
		AnalyzerTemperature: wc.AnalyzerTemperature,
		AnalyzerMaxTokens:   analyzerMaxTokens(modelDef),
		AnalyzerStuckTimeout: time.Duration(wc.AnalyzerStuckTimeoutMS) * time.Millisecond,
		AnalyzerMaxDuration:  time.Duration(wc.AnalyzerMaxDurationMS) * time.Millisecond,
		GoalMaxIterations:     wc.GoalMaxIterations,
	}
}
