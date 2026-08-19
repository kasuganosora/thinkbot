package bot

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/engagement"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/mcp"
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Bot — 独立的消息处理单元
// ============================================================================

// Bot 是平台中一个独立的机器人实例。
// 它组合了 Engine（轻量级内核）并在其上叠加：
//   - 多 Channel 管理（输入端启停 + 输出端 Sender 自动桥接）
//   - EventBus 旁路事件（SSE 实时状态推送）
//   - 内建 Handler 自动注册（Reply/Forward/Broadcast/Note/Callback/Silent）
//   - Bot 级别配置（LLM 参数、system prompt 等）
//   - 自适应 Engagement（Bot 自我画像 + 动态参数调整）
//
// Bot 通过 EngineHook 机制扩展 Engine 的处理流程，
// 在消息处理各阶段注入事件发射和 context 增强，无需复制 Engine 代码。
//
// 消息流转路径（完整双向）：
//
//	[Inbound] Channel.onMessage()
//	  → msg.BotID = channel.BotID()
//	  → bot.Ingress().Receive(ctx, msg)
//	  → Engine worker 从 ingress.C() 消费
//	  → pipeline.Execute(ctx, env)
//	  → dispatcher.Dispatch(ctx, actions)
//
//	[Outbound] Dispatcher 路由 Action 到对应 Handler：
//	  ActionReply/ActionForward/ActionBroadcast → ChannelReplyHandler → Sender.Send()
//	  ActionNote → NoteHandler → memory.Store.Append()
//	  ActionCallback → CallbackHandler → CallbackRegistry.Invoke()
//	  ActionSilent → SilentHandler → 仅记录 trace/log
//
//	[Output 决策模式]（Pipeline Stage 可组合产出以下 Action）
//	  1. 正常回复：ActionReply → 发送到 Channel
//	  2. 回复 + 备注：ActionReply + ActionNote → 发送 + 记录
//	  3. 只备注不回复：ActionNote → 只记录，不发送任何消息
//	  4. 执行回调：ActionCallback → 将结果回传给父 Agent/任务发起方
//	  5. 主动静默：ActionSilent → 什么都不做，仅记录决策
type Bot struct {
	// ID Bot 唯一标识（如 "customer-service"、"code-review"）。
	ID string
	// Name Bot 显示名称。
	Name string
	// Config Bot 级别配置。
	Config BotConfig

	engine          *agent.Engine                 // 轻量级内核
	replyHandler    *outbound.ChannelReplyHandler // 内建的 Channel 回写处理器
	noteHandler     *outbound.NoteHandler         // 内建的备注处理器
	callbackHandler *outbound.CallbackHandler     // 内建的回调处理器
	silentHandler   *outbound.SilentHandler       // 内建的静默处理器
	emitter         *outbound.EventEmitter        // 旁路事件发射器（可选，nil=禁用）
	channels        []Channel
	logger          *zap.SugaredLogger

	// 持久化工作空间（Bot 创建时自动创建，重启后文件不丢失）
	workspaceMgr *sandbox.BotWorkspaceManager // 工作空间管理器（nil=未启用）
	workspaceDir string                       // 当前 Bot 的工作空间目录绝对路径
	soulLoader   *prompt.SoulLoader           // SOUL.md 加载器（nil=未启用）

	// 梦境巩固（nil=未启用，默认禁用）
	dreamScheduler *cron.Scheduler // 梦境巩固的 cron 调度器

	// 自适应 Engagement（可选）
	adaptiveSyncer *engagement.AdaptiveEngagementSyncer // 画像→参数映射器（nil=未启用）

	// 资源管理（Close 时释放）
	ownRegistry bool      // Bot 是否创建了 CallbackRegistry（外部传入的不关）
	closerOnce  sync.Once // 确保 Close 只执行一次

	// 浏览器 MCP 管理器（per-bot，docker 后端 + BrowserEnabled 时创建；nil=未启用）。
	browserMCP *mcp.Manager
	// browserCookieSaver 会话结束后回收容器内 cookie 状态文件时调用。
	browserCookieSaver func(ctx context.Context, stateJSON []byte) error
	// browserCookieStartupRecover 启动期合并回收容器内残留 cookie 状态文件时调用。
	browserCookieStartupRecover func(ctx context.Context, stateJSON []byte) error

	// botMetrics 是 Bot 层额外的指标（Engine 层有自己的基础指标）
	dispatchErrors atomic.Int64

	// 消息级取消链路（可选）：用于将每条消息的 cancel 函数注册到上层（如 API abort 接口）。
	onMessageStart func(botID, traceID string, cancel context.CancelFunc, interruptCh chan string)
	onMessageDone  func(botID, traceID string)
}

// BotParams 是 Bot 构造参数。
type BotParams struct {
	ID               string
	Name             string
	Config           BotConfig
	AgentConfig      AgentConfig // Per-bot agent 行为配置（compaction、工具过滤等）
	Pipeline         *pipeline.Pipeline
	Dispatcher       outbound.Dispatcher
	Channels         []Channel
	MemoryStore      memory.Store              // 可选：记忆写入后端（供 NoteHandler 使用）。nil 时使用内存仓储。
	CallbackRegistry outbound.CallbackRegistry // 可选：回调注册表。nil 时使用 MemoryCallbackRegistry。
	EventBus         outbound.EventBus         // 可选：旁路事件总线。nil 时禁用 SSE 事件推送。

	// OutboundGuard 可选：出站前的「渠道只读」检查。
	// 非 nil 时，ActionReply / ActionForward 等对外动作在发送前会先询问守卫，
	// 被拒绝的动作静默丢弃（记WARN 日志）。用于实现「只看不发」的潜水bot ——
	// 注意 Pipeline 自动回复不经过工具权限，只能在这一层拦。
	OutboundGuard outbound.OutboundGuard

	Logger *zap.SugaredLogger
	TP     trace.TracerProvider

	// --- 持久化工作空间 ---

	// WorkspaceDir bot 工作空间的根目录物理路径（如 "data/workspaces"）。
	// 为空时禁用工作空间（不创建目录、不加载 SOUL.md、不注册工具）。
	// 每个 Bot 在此目录下拥有独立子目录 {WorkspaceDir}/{BotID}/。
	// 文件持久化保存，重启后不丢失。
	WorkspaceDir string

	// PromptRegistry prompt.Registry（可选，用于 SoulLoader 加载 SOUL.md）。
	// 为 nil 时跳过 SoulLoader（但工作空间目录仍会创建）。
	PromptRegistry *prompt.Registry

	// ToolManager 工具管理器（可选，用于注册工作空间文件操作工具）。
	// 为 nil 时跳过工具注册（但工作空间目录仍会创建）。
	ToolManager *tools.ToolManager

	// SandboxConfig 沙箱配置（Backend/Image/Limits 等，可选）。
	// 为空时使用 sandbox.DefaultConfig()。
	SandboxConfig sandbox.Config

	// Mode 装配模式（pipeline.PipelineMode：standard / lurk-only / code）。
	// 仅用于门控「工作空间/代码工具组」的注册：lurk-only 下 bot 不执行任何动作，
	// 跳过 sandbox 工具（sandbox_exec / read_file / run_code 等）的注册；
	// 工作空间目录本身仍会创建（SOUL.md / 工具输出落盘仍可用）。
	// 空值或未知模式按 standard 处理（注册全部工具）。
	Mode pipeline.PipelineMode

	// DreamScheduler 梦境巩固的 cron 调度器（可选，nil=不启用梦境巩固）。
	// 调度器需要在调用方创建并注入，Bot.Run 会自动 Start 它，Bot.Close 会自动 Stop。
	DreamScheduler *cron.Scheduler

	// SelfIDSet Bot 自身用户 ID 的共享集合（可选）。
	// 如果提供，Bot 内部的 Ingress 会使用它来存储和检查自消息，
	// 同时调用方可以将同一个集合传递给 Engagement 层的 SelfExclusionRule，
	// 使两层防线共享同一份数据，无需时序协调。
	// 如果为 nil，Ingress 会创建一个内部的 SelfIDSet。
	SelfIDSet *inbound.SelfIDSet

	// AdaptiveSyncer 自适应 Engagement 同步器（可选，nil=禁用）。
	// 注入后，Bot 会将此同步器的 DynamicConfigFunc 绑定到 TimingGate，
	// 实现 Bot 自我画像 → Engagement 参数的动态映射。
	AdaptiveSyncer *engagement.AdaptiveEngagementSyncer

	// RejectionDetector 被无视检测器（可选，nil=禁用）。
	// 注入后，Bot 会在发送回复时通知检测器，并在 TimingGate 中考虑自闭模式。
	RejectionDetector *engagement.RejectionDetector

	// BrowserCookieLoader 返回该 bot 当前 cookie 状态文件 JSON（Web 面板管理的 cookie），
	// 用于会话前投递进容器内的浏览器 MCP 进程。返回 nil/空表示无 cookie 或不启用投递。
	// 仅当沙箱配置 BrowserEnabled=true 且为 docker 后端时才会被调用。
	BrowserCookieLoader func(ctx context.Context) ([]byte, error)

	// BrowserCookieSaver 将浏览器会话结束后回写的 cookie 状态文件 JSON 持久化回 DB。
	// 由 Bot.Close 在浏览器 MCP 进程优雅退出后调用，回收容器内浏览器实际产生的 cookie。
	BrowserCookieSaver func(ctx context.Context, stateJSON []byte) error

	// BrowserCookieStartupRecover 启动期合并回收容器内「上一轮残留」的 cookie 状态文件。
	// 在 loader 覆盖写状态文件前调用，修复非优雅关闭（SIGKILL/崩溃）导致 Close 未跑、
	// cookie 永不进 DB 的缺口。采用 upsert 合并，保留 DB 独有项、幂等于优雅重启。
	BrowserCookieStartupRecover func(ctx context.Context, stateJSON []byte) error

	// OnMessageStart 在单条消息开始处理时回调，提供可取消的 message context
	// 以及本消息生命周期内的「用户中途追加」通道（interruptCh，用于 Claude-CLI
	// 风格的边生成边补充）。
	// 典型用途：上层注册 traceID -> cancelFunc，供 /chat/abort 终止本轮执行；
	// 同时把 interruptCh 存起来，供 /chat/append 在生成中向同一轮注入用户补充。
	OnMessageStart func(botID, traceID string, cancel context.CancelFunc, interruptCh chan string)
	// OnMessageDone 在单条消息结束（成功/失败/丢弃/派发失败）时回调。
	// 典型用途：上层注销 traceID -> cancelFunc，避免内存泄漏。
	OnMessageDone func(botID, traceID string)
}

// New 创建一个 Bot 实例。
// 创建后需要调用 Run 启动消息处理循环。
//
// Bot 内部创建一个 Engine 实例并通过 EngineHook 注入事件发射、
// context 增强等行为。
//
// 如果 Dispatcher 是 MultiDispatcher，Bot 会自动注册所有内建 Handler：
// ChannelReplyHandler (Reply/Forward/Broadcast)、NoteHandler、CallbackHandler、SilentHandler。
// Channel 启动后，实现了 Sender 接口的 Channel 会被自动注册到 ChannelReplyHandler。
func New(params BotParams) (*Bot, error) {
	if params.ID == "" {
		return nil, fmt.Errorf("bot: ID is required")
	}
	if params.Pipeline == nil {
		return nil, fmt.Errorf("bot %q: pipeline is required", params.ID)
	}
	if params.Dispatcher == nil {
		return nil, fmt.Errorf("bot %q: dispatcher is required", params.ID)
	}
	if params.Logger == nil {
		return nil, fmt.Errorf("bot %q: logger is required", params.ID)
	}
	if params.TP == nil {
		return nil, fmt.Errorf("bot %q: tracer provider is required", params.ID)
	}

	cfg := DefaultBotConfig().Merge(params.Config)

	if params.Name == "" {
		params.Name = params.ID
	}

	botLogger := params.Logger.With("bot_id", params.ID)

	// 创建 Ingress（每个 Bot 独立）
	ingressCfg := inbound.IngressConfig{
		BufferSize: cfg.IngressBufferSize,
		SelfIDSet:  params.SelfIDSet,
	}
	ingress := inbound.NewIngress(
		ingressCfg,
		botLogger.With("component", "ingress"),
		params.TP,
	)

	// 创建 ChannelReplyHandler
	replyHandler := outbound.NewChannelReplyHandler(botLogger, params.TP)
	// 装上只读守卫（若配置）：Pipeline 自动回复不经过工具权限，
	// 「潜水 bot」只能在出站这一层拦住。
	if params.OutboundGuard != nil {
		replyHandler.SetGuard(params.OutboundGuard)
	}

	// 创建 NoteHandler（写入统一记忆仓储）
	memStore := params.MemoryStore
	if memStore == nil {
		memStore = memory.NewMemoryRepository()
	}
	noteWriter := memory.NewNoteWriterAdapter(memStore)
	noteHandler := outbound.NewNoteHandler(noteWriter, botLogger, params.TP)

	// 创建 CallbackHandler
	callbackRegistry := params.CallbackRegistry
	ownRegistry := false
	if callbackRegistry == nil {
		callbackRegistry = outbound.NewMemoryCallbackRegistry()
		ownRegistry = true
	}
	callbackHandler := outbound.NewCallbackHandler(callbackRegistry, botLogger, params.TP)

	// 创建 SilentHandler
	silentHandler := outbound.NewSilentHandler(botLogger, params.TP)

	// 自动注册 Handler 到 MultiDispatcher
	if multiDisp, ok := params.Dispatcher.(*outbound.MultiDispatcher); ok {
		multiDisp.Register(core.ActionReply, replyHandler)
		multiDisp.Register(core.ActionForward, replyHandler)
		multiDisp.Register(core.ActionBroadcast, replyHandler)
		multiDisp.Register(core.ActionNote, noteHandler)
		multiDisp.Register(core.ActionCallback, callbackHandler)
		multiDisp.Register(core.ActionSilent, silentHandler)
	}

	// 创建 EventEmitter（EventBus 为 nil 时 NoOp 模式）
	emitter := outbound.NewEventEmitter(params.EventBus, params.ID)

	bot := &Bot{
		ID:              params.ID,
		Name:            params.Name,
		Config:          cfg,
		replyHandler:    replyHandler,
		noteHandler:     noteHandler,
		callbackHandler: callbackHandler,
		silentHandler:   silentHandler,
		emitter:         emitter,
		channels:        params.Channels,
		logger:          botLogger,
		ownRegistry:     ownRegistry,
		onMessageStart:  params.OnMessageStart,
		onMessageDone:   params.OnMessageDone,
	}

	// 创建持久化工作空间（文件在宿主文件系统，重启不丢失）
	if params.WorkspaceDir != "" {
		sbCfg := params.SandboxConfig
		if sbCfg.Backend == "" {
			sbCfg.Backend = sandbox.DefaultConfig().Backend
		}
		// 确保时区已设置（调用方可通过 SandboxConfig.Timezone 注入 config.GetTimezone()）
		if sbCfg.Timezone == "" {
			sbCfg.Timezone = sandbox.DefaultConfig().Timezone
		}

		wsMgr, err := sandbox.NewBotWorkspaceManager(params.WorkspaceDir, sbCfg, botLogger)
		if err != nil {
			return nil, errs.Wrapf(err, "bot %q: create workspace manager", params.ID)
		}

		botDir, err := wsMgr.BotDir(params.ID)
		if err != nil {
			return nil, errs.Wrapf(err, "bot %q: create workspace dir", params.ID)
		}

		// 转为绝对路径，确保重启后路径一致
		absDir, err := filepath.Abs(botDir)
		if err != nil {
			absDir = botDir
		}

		bot.workspaceMgr = wsMgr
		bot.workspaceDir = absDir

		botLogger.Infow("workspace created",
			"dir", absDir,
			"backend", wsMgr.Backend())

		// SoulLoader 加载 SOUL.md（人格定义）。
		// 文件 IO 后端选择：
		//   - docker 持久容器模式（DooD）：SOUL.md 真实位于 bot 容器 named volume
		//     的 /data/SOUL.md，主程序侧 {WorkspaceDir}/{botID}/ 是空目录。必须经
		//     sandbox.Workspace 抽象（docker exec）读写，单一数据源，否则 agent 自改
		//     无法被主程序读回（P0：SoulLoader 与 named volume 脱节，方向 A 修复）。
		//   - local 模式：直接走主程序侧宿主路径（OS 文件 IO，零隔离）。
		if params.PromptRegistry != nil {
			soulPath := filepath.Join(params.WorkspaceDir, params.ID, "SOUL.md")
			var soulStore prompt.SoulStore
			if wsMgr.Backend() == "docker" {
				if bw, gErr := wsMgr.GetOrCreate(params.ID); gErr == nil {
					soulStore = NewWorkspaceSoulStore(bw, "SOUL.md")
					soulPath = "SOUL.md"
					botLogger.Infow("soul store: workspace backend (docker named volume)", "botID", params.ID)
				} else {
					botLogger.Warnw("soul store: get workspace failed, fallback to host path", "err", gErr)
				}
			}
			soul := prompt.NewSoulLoader(prompt.SoulLoaderConfig{
				Path:           soulPath,
				BotID:          params.ID,
				SectionName:    "identity",
				Order:          0,
				ReloadInterval: 5 * time.Second,
				Store:          soulStore,
			}, params.PromptRegistry)

			if err := soul.Load(); err != nil {
				botLogger.Warnw("soul load failed, using fallback prompt",
					"path", soulPath, "err", err)
			} else {
				botLogger.Infow("soul loaded",
					"path", soulPath, "loaded", soul.Loaded())
			}
			bot.soulLoader = soul
		}

		// 注册工作空间工具（sandbox_exec/read_file/write_file/run_code 等）。
		// lurk-only 模式下 bot 只学不说、不执行任何动作，跳过整组代码工具注册
		// （GroupCode 关闭）；工作空间目录/SOUL.md/工具输出落盘不受影响。
		if params.ToolManager != nil && params.Mode != pipeline.ModeLurkOnly {
			if err := sandbox.RegisterBotWorkspaceTools(params.ToolManager, wsMgr); err != nil {
				return nil, errs.Wrapf(err, "bot %q: register workspace tools", params.ID)
			}
			botLogger.Debugw("workspace tools registered", "mode", params.Mode)
		} else if params.Mode == pipeline.ModeLurkOnly {
			botLogger.Debugw("workspace tools skipped (lurk-only mode)", "bot_id", params.ID)
		}

		// 接入 per-bot 浏览器 MCP 服务（docker 后端 + BrowserEnabled + 有 ToolManager 时）。
		// 浏览器进程经 `docker exec -i` 运行在该 bot 容器内，工具命名 browser__<tool>，
		// 随 bot 生命周期启停，并与 Web 面板管理的 cookie 双向同步（会话前投递 / 会话后回收）。
		if params.ToolManager != nil && wsMgr.Backend() == "docker" && params.SandboxConfig.BrowserEnabled {
			if err := setupBrowserMCP(bot, params, wsMgr); err != nil {
				botLogger.Warnw("browser mcp setup failed, browser tools disabled",
					"err", err, "bot_id", params.ID)
			} else {
				bot.browserCookieSaver = params.BrowserCookieSaver
				bot.browserCookieStartupRecover = params.BrowserCookieStartupRecover
			}
		}
	}

	// 创建 Engine，注入 Bot 的 hook
	bot.engine = agent.NewEngine(
		ingress,
		params.Pipeline,
		params.Dispatcher,
		agent.EngineConfig{
			Workers:         cfg.Workers,
			ShutdownTimeout: 10 * time.Second,
		},
		botLogger,
		params.TP,
		agent.WithHook(bot),
	)

	// 注入梦境巩固调度器
	bot.dreamScheduler = params.DreamScheduler

	// 注入自适应 Engagement 组件
	bot.adaptiveSyncer = params.AdaptiveSyncer

	return bot, nil
}

// Run 启动 Bot 的消息处理循环。
// 它会：
//  1. 启动所有 Channel（Channel.Start 拿到 Ingress）
//  2. 将实现了 Sender 接口的 Channel 注册到 ChannelReplyHandler
//  3. 启动 Engine（worker pool + 消息处理循环）
//  4. 阻塞直到 ctx 取消
//  5. 优雅关闭：停止 Channel → Engine.Stop
func (b *Bot) Run(ctx context.Context) error {
	b.logger.Infow("bot starting",
		"name", b.Name,
		"channels", len(b.channels))

	// 启动所有 Channel
	for i, ch := range b.channels {
		b.logger.Infow("starting channel",
			"channel_name", ch.Name(),
			"channel_type", ch.Type())

		if err := ch.Start(ctx, b.engine.Ingress()); err != nil {
			b.logger.Errorw("channel start failed, rolling back",
				"channel_name", ch.Name(),
				"err", err)
			// 只停止已成功启动的 channel (0..i-1)
			b.stopChannelsSlice(ctx, b.channels[:i])
			return errs.Wrapf(err, "bot %q: channel %q start failed", b.ID, ch.Name())
		}

		// 如果 Channel 实现了 Sender 接口，注册到 ChannelReplyHandler
		if sender, ok := ch.(Sender); ok {
			b.replyHandler.Register(ch.Name(), sender)
			b.logger.Infow("channel registered as sender",
				"channel_name", ch.Name(),
				"channel_type", ch.Type())
		}
	}

	b.logger.Infow("channels started",
		"senders_registered", b.replyHandler.RegisteredCount())

	// 启动 SoulLoader 热重载（如果配置了工作空间）
	if b.soulLoader != nil {
		b.soulLoader.StartWatcher(ctx)
	}

	// 启动梦境巩固调度器（如果配置了）
	if b.dreamScheduler != nil {
		b.dreamScheduler.Start(ctx)
		b.logger.Infow("dream scheduler started",
			"bot_id", b.ID,
			"tz", b.dreamScheduler.Summary())
	}

	// 启动 Engine（会阻塞直到 ctx 取消）
	err := b.engine.Run(ctx)

	// Engine 停止后，清理 Channel
	for _, ch := range b.channels {
		b.replyHandler.Unregister(ch.Name())
	}
	b.stopChannels(context.Background())

	// 释放 Bot 拥有的后台资源（CallbackRegistry 的 cleanup goroutine 等）
	b.Close()

	return err
}

// Stop 触发 Bot 优雅关闭。
// 停止 Engine 后，Run 方法会自动调用 Close 释放资源。
func (b *Bot) Stop() {
	b.engine.Stop()
}

// Close 释放 Bot 拥有的后台资源（如 CallbackRegistry 的 cleanup goroutine）。
// 此方法是幂等的，可安全多次调用。
// 如果 CallbackRegistry 或 EventBus 是外部传入的（Bot 不拥有），则不会关闭它们。
func (b *Bot) Close() {
	b.closerOnce.Do(func() {
		// 停止梦境巩固调度器
		if b.dreamScheduler != nil {
			b.dreamScheduler.Stop()
		}
		if b.ownRegistry {
			if r, ok := b.callbackHandler.Registry().(interface{ Close() }); ok {
				r.Close()
			}
		}
		// 停止 SoulLoader 热重载
		if b.soulLoader != nil {
			b.soulLoader.Stop()
		}
		// 浏览器会话回收：先让 wrapper 优雅落盘 cookie，再关闭传输层、读回状态文件持久化到 DB。
		// 必须在工作空间管理器关闭（容器销毁）之前完成。
		// 注意：直接 Close() 会 SIGKILL 掉 docker exec 子进程，而 `docker exec -i` 默认不代理信号，
		// wrapper 的 shutdown 钩子不会跑，cookie 将丢。故先调 close 工具触发其 saveState 再回包。
		if b.browserMCP != nil {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 8*time.Second)
			if _, cerr := b.browserMCP.CallTool(closeCtx, "browser", "close", map[string]any{}); cerr != nil {
				b.logger.Warnw("browser graceful close failed, will force close", "err", cerr)
			}
			closeCancel()
			// 浏览器会话回收：先让 wrapper 优雅落盘 cookie，再把状态文件持久化到 DB。
			//
			// 竞态修复（根因）：wrapper 的 close 工具在回包前已完成 saveState 落盘，故应在
			// 关闭传输层【之前】读取状态文件，此时文件必然已含 cookie。若因极端时序未命中，
			// 再关闭传输层触发容器内 wrapper 的 shutdown 落盘（docker exec 连接关闭 → wrapper
			// 收到 stdin EOF → shutdown → saveState），随后轮询读取，确保不丢 cookie。
			// （注意：docker exec 被 SIGKILL 后立即退出，不等容器内 wrapper 结束，故不能“先关再读”。）
			if b.browserCookieSaver != nil && b.workspaceMgr != nil {
				b.recoverBrowserCookies(context.Background())
			}
			if b.browserMCP != nil {
				_ = b.browserMCP.Close()
			}
		}
		// 关闭工作空间管理器（文件持久化，不删除）
		if b.workspaceMgr != nil {
			if err := b.workspaceMgr.Close(); err != nil {
				b.logger.Warnw("workspace manager close failed", "err", err)
			}
		}
	})
}

// Ready 返回一个 channel，该 channel 在 Bot 完成初始化（Channel 已启动、Engine 已就绪）后关闭。
func (b *Bot) Ready() <-chan struct{} {
	return b.engine.Ready()
}

// recoverBrowserCookies 在 bot 关闭时把浏览器会话 cookie 从容器内状态文件回收进 DB。
// 先读一次（close 工具回包前已 saveState 落盘）；未命中则关闭传输层触发 wrapper shutdown 落盘并轮询。
func (b *Bot) recoverBrowserCookies(ctx context.Context) {
	const maxAttempts = 6
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if b.tryRecoverOnce(ctx) {
			return
		}
		// 首次未命中：关闭传输层，触发容器内 wrapper 的 shutdown 落盘（EOF → saveState → exit）。
		if attempt == 1 && b.browserMCP != nil {
			_ = b.browserMCP.Close()
			b.browserMCP = nil // 标记已关闭，避免外层重复关闭
		}
		if attempt < maxAttempts {
			time.Sleep(120 * time.Millisecond)
		}
	}
	b.logger.Warnw("browser cookie recover failed: state file never populated")
}

// tryRecoverOnce 读取一次状态文件并尝试持久化到 DB；成功返回 true。
func (b *Bot) tryRecoverOnce(ctx context.Context) bool {
	if b.workspaceMgr == nil || b.browserCookieSaver == nil {
		return true // 无回收依赖则视为无需处理
	}
	ws, werr := b.workspaceMgr.GetOrCreate(b.ID)
	if werr != nil {
		b.logger.Warnw("browser workspace unavailable", "err", werr)
		return false
	}
	data, rerr := ws.ReadFile(ctx, "/data/.browser-state.json")
	if rerr != nil || len(data) == 0 {
		return false
	}
	if serr := b.browserCookieSaver(ctx, data); serr != nil {
		b.logger.Warnw("browser cookie recover failed", "err", serr)
		return false
	}
	b.logger.Infow("browser cookies recovered from session")
	return true
}

// Ingress 返回 Bot 私有的 Ingress 实例。
func (b *Bot) Ingress() *inbound.Ingress {
	return b.engine.Ingress()
}

// Engine 返回 Bot 内部的 Engine 实例。
func (b *Bot) Engine() *agent.Engine {
	return b.engine
}

// Channels 返回 Bot 拥有的所有 Channel 列表。
func (b *Bot) Channels() []Channel {
	return b.channels
}

// WorkspaceDir 返回 Bot 的持久化工作空间目录绝对路径。
// 如果未启用工作空间，返回空字符串。
func (b *Bot) WorkspaceDir() string {
	return b.workspaceDir
}

// WorkspaceMgr 返回 Bot 的工作空间管理器。
// 如果未启用工作空间，返回 nil。
func (b *Bot) WorkspaceMgr() *sandbox.BotWorkspaceManager {
	return b.workspaceMgr
}

// SoulLoader 返回 Bot 的 SoulLoader。
// 如果未启用 SoulLoader，返回 nil。
func (b *Bot) SoulLoader() *prompt.SoulLoader {
	return b.soulLoader
}

// CallbackRegistry 返回 Bot 的回调注册表。
func (b *Bot) CallbackRegistry() outbound.CallbackRegistry {
	return b.callbackHandler.Registry()
}

// Emitter 返回 Bot 的事件发射器。
func (b *Bot) Emitter() *outbound.EventEmitter {
	return b.emitter
}

// BotMetrics 是 Bot 的运行指标快照（包含 Engine 基础指标 + Bot 附加指标）。
type BotMetrics struct {
	MessagesProcessed int64 `json:"messages_processed"`
	MessagesErrors    int64 `json:"messages_errors"`
	DispatchErrors    int64 `json:"dispatch_errors"`
}

// Metrics 返回 Bot 当前运行指标。
func (b *Bot) Metrics() BotMetrics {
	em := b.engine.Metrics()
	return BotMetrics{
		MessagesProcessed: em.MessagesProcessed,
		MessagesErrors:    em.MessagesErrors,
		DispatchErrors:    b.dispatchErrors.Load(),
	}
}

// SaveNote 将一条内部笔记写入本 bot 的长期记忆（bot 全局 scope）。
// 供心跳等自主场景在决策 DecisionNote 时复用与 ActionNote 完全相同的记忆写入链路，
// 使 bot 自主记下的笔记可跨渠道召回。
func (b *Bot) SaveNote(ctx context.Context, content string) error {
	if b.noteHandler == nil {
		return fmt.Errorf("bot %q: note handler not initialized", b.ID)
	}
	return b.noteHandler.Handle(ctx, core.Action{
		Type:    core.ActionNote,
		Payload: content,
		Metadata: map[string]any{
			"bot_id":   b.ID,
			"category": "heartbeat",
			"source":   "heartbeat",
		},
	})
}

// stopChannels 停止所有 Channel（尽力而为）。
func (b *Bot) stopChannels(ctx context.Context) {
	b.stopChannelsSlice(ctx, b.channels)
}

// stopChannelsSlice 停止给定的 Channel 切片（尽力而为）。
func (b *Bot) stopChannelsSlice(ctx context.Context, channels []Channel) {
	for _, ch := range channels {
		if err := ch.Stop(ctx); err != nil {
			b.logger.Warnw("channel stop error",
				"channel_name", ch.Name(),
				"err", err)
		}
	}
}

// ============================================================================
// EngineHook 实现 — Bot 通过 hook 扩展 Engine 行为
// ============================================================================

type messageCancelCtxKey struct{}

// interruptCtxKey 用于在 message context 中携带「用户中途追加」通道
// （Claude-CLI 风格的「边思考/边输出边补充」）。
type interruptCtxKey struct{}

// WithInterruptChannel 将一条消息生命周期内的「用户中途追加」通道绑定到 ctx。
func WithInterruptChannel(ctx context.Context, ch chan string) context.Context {
	return context.WithValue(ctx, interruptCtxKey{}, ch)
}

// InterruptChannelFromContext 取回绑定到 ctx 的中途追加通道；无则返回 nil。
func InterruptChannelFromContext(ctx context.Context) chan string {
	if v := ctx.Value(interruptCtxKey{}); v != nil {
		if ch, ok := v.(chan string); ok {
			return ch
		}
	}
	return nil
}

// finishMessageLifecycle 清理单条消息的取消函数注册与回调。
func (b *Bot) finishMessageLifecycle(ctx context.Context, traceID string) {
	if v := ctx.Value(messageCancelCtxKey{}); v != nil {
		if cancel, ok := v.(context.CancelFunc); ok && cancel != nil {
			cancel()
		}
	}
	if b.onMessageDone != nil && traceID != "" {
		b.onMessageDone(b.ID, traceID)
	}
}

// OnBeforeProcess 在 Engine 处理 Envelope 之前注入 EventEmitter 和 Bot 配置。
func (b *Bot) OnBeforeProcess(ctx context.Context, env *core.Envelope) context.Context {
	// 注入 EventEmitter 到 context，供 Pipeline Stage（如 ObservableStage）使用
	ctx = outbound.ContextWithEmitter(ctx, b.emitter)

	traceID := env.Message.TraceID
	if b.onMessageStart != nil && traceID != "" {
		msgCtx, cancel := context.WithCancel(ctx)
		// 为本轮创建「用户中途追加」通道（带缓冲，避免上游阻塞）。
		interruptCh := make(chan string, 16)
		b.onMessageStart(b.ID, traceID, cancel, interruptCh)
		ctx = context.WithValue(msgCtx, messageCancelCtxKey{}, cancel)
		ctx = WithInterruptChannel(ctx, interruptCh)
	}

	// 注入 Bot 配置到 Envelope KV，供 Stage 读取
	env.Set("bot.id", b.ID)
	env.Set("bot.config", b.Config)

	// 注入 SOUL.md 人格文本，供潜水观察者模式（lurk-learn）结合人格分析。
	// 仅在 soul 已加载时注入，避免每轮空写；LLMStage 读取后拼到观察者 prompt 前。
	if b.soulLoader != nil && b.soulLoader.Loaded() {
		if sc := b.soulLoader.Content(); sc != "" {
			env.Set(core.KVSoulContent, sc)
		}
	}

	// 旁路事件：消息接收
	b.emitter.EmitMessageReceived(ctx, env.Message)

	return ctx
}

// OnPipelineError 在 Pipeline 执行出错时发射旁路事件。
func (b *Bot) OnPipelineError(ctx context.Context, env *core.Envelope, err error) {
	b.emitter.EmitMessageError(ctx, env.Message.TraceID, err)
	b.finishMessageLifecycle(ctx, env.Message.TraceID)
}

// OnMessageDropped 在消息被 Pipeline 丢弃时发射旁路事件。
func (b *Bot) OnMessageDropped(ctx context.Context, env *core.Envelope) {
	b.emitter.EmitMessageDropped(ctx, env.Message.TraceID, "pipeline")
	b.finishMessageLifecycle(ctx, env.Message.TraceID)
}

// OnBeforeDispatch 在 Dispatcher 派发前发射旁路事件。
func (b *Bot) OnBeforeDispatch(ctx context.Context, env *core.Envelope, actions []core.Action) {
	b.emitter.EmitDispatchStart(ctx, env.Message.TraceID, len(actions))
}

// OnDispatchError 在 Dispatcher 派发失败时发射旁路事件。
func (b *Bot) OnDispatchError(ctx context.Context, env *core.Envelope, err error) {
	b.dispatchErrors.Add(1)
	b.emitter.EmitDispatchError(ctx, env.Message.TraceID, err)
	b.finishMessageLifecycle(ctx, env.Message.TraceID)
}

// OnMessageDone 在消息处理成功完成时发射旁路事件。
func (b *Bot) OnMessageDone(ctx context.Context, env *core.Envelope, actions []core.Action, duration time.Duration) {
	b.emitter.EmitMessageDone(ctx, env.Message.TraceID, len(actions), duration)
	b.finishMessageLifecycle(ctx, env.Message.TraceID)
}
