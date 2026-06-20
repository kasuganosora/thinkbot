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
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
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

	// 资源管理（Close 时释放）
	ownRegistry bool      // Bot 是否创建了 CallbackRegistry（外部传入的不关）
	closerOnce  sync.Once // 确保 Close 只执行一次

	// botMetrics 是 Bot 层额外的指标（Engine 层有自己的基础指标）
	dispatchErrors atomic.Int64
}

// BotParams 是 Bot 构造参数。
type BotParams struct {
	ID               string
	Name             string
	Config           BotConfig
	Pipeline         *pipeline.Pipeline
	Dispatcher       outbound.Dispatcher
	Channels         []Channel
	MemoryStore      memory.Store              // 可选：记忆写入后端（供 NoteHandler 使用）。nil 时使用内存仓储。
	CallbackRegistry outbound.CallbackRegistry // 可选：回调注册表。nil 时使用 MemoryCallbackRegistry。
	EventBus         outbound.EventBus         // 可选：旁路事件总线。nil 时禁用 SSE 事件推送。
	Logger           *zap.SugaredLogger
	TP               trace.TracerProvider

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
	ingress := inbound.NewIngress(
		inbound.IngressConfig{BufferSize: cfg.IngressBufferSize},
		botLogger.With("component", "ingress"),
		params.TP,
	)

	// 创建 ChannelReplyHandler
	replyHandler := outbound.NewChannelReplyHandler(botLogger, params.TP)

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
		ID:           params.ID,
		Name:         params.Name,
		Config:       cfg,
		replyHandler:    replyHandler,
		noteHandler:     noteHandler,
		callbackHandler: callbackHandler,
		silentHandler:   silentHandler,
		emitter:         emitter,
		channels:        params.Channels,
		logger:          botLogger,
		ownRegistry:     ownRegistry,
	}

	// 创建持久化工作空间（文件在宿主文件系统，重启不丢失）
	if params.WorkspaceDir != "" {
		sbCfg := params.SandboxConfig
		if sbCfg.Backend == "" {
			sbCfg = sandbox.DefaultConfig()
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

		// SoulLoader 从工作空间目录加载 SOUL.md
		if params.PromptRegistry != nil {
			soulPath := filepath.Join(absDir, "SOUL.md")
			soul := prompt.NewSoulLoader(prompt.SoulLoaderConfig{
				Path:           soulPath,
				BotID:          params.ID,
				SectionName:    "identity",
				Order:          0,
				ReloadInterval: 5 * time.Second,
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

		// 注册工作空间工具（sandbox_exec/read_file/write_file 等）
		if params.ToolManager != nil {
			if err := sandbox.RegisterBotWorkspaceTools(params.ToolManager, wsMgr); err != nil {
				return nil, errs.Wrapf(err, "bot %q: register workspace tools", params.ID)
			}
			botLogger.Debugw("workspace tools registered")
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
	for _, ch := range b.channels {
		b.logger.Infow("starting channel",
			"channel_name", ch.Name(),
			"channel_type", ch.Type())

		if err := ch.Start(ctx, b.engine.Ingress()); err != nil {
			b.logger.Errorw("channel start failed, rolling back",
				"channel_name", ch.Name(),
				"err", err)
			b.stopChannels(ctx)
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
		if b.ownRegistry {
			if r, ok := b.callbackHandler.Registry().(interface{ Close() }); ok {
				r.Close()
			}
		}
		// 停止 SoulLoader 热重载
		if b.soulLoader != nil {
			b.soulLoader.Stop()
		}
		// 关闭工作空间管理器（文件持久化，不删除）
		if b.workspaceMgr != nil {
			b.workspaceMgr.Close()
		}
	})
}

// Ready 返回一个 channel，该 channel 在 Bot 完成初始化（Channel 已启动、Engine 已就绪）后关闭。
func (b *Bot) Ready() <-chan struct{} {
	return b.engine.Ready()
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

// stopChannels 停止所有 Channel（尽力而为）。
func (b *Bot) stopChannels(ctx context.Context) {
	for _, ch := range b.channels {
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

// OnBeforeProcess 在 Engine 处理 Envelope 之前注入 EventEmitter 和 Bot 配置。
func (b *Bot) OnBeforeProcess(ctx context.Context, env *core.Envelope) context.Context {
	// 注入 EventEmitter 到 context，供 Pipeline Stage（如 ObservableStage）使用
	ctx = outbound.ContextWithEmitter(ctx, b.emitter)

	// 注入 Bot 配置到 Envelope KV，供 Stage 读取
	env.Set("bot.id", b.ID)
	env.Set("bot.config", b.Config)

	// 旁路事件：消息接收
	b.emitter.EmitMessageReceived(ctx, env.Message)

	return ctx
}

// OnPipelineError 在 Pipeline 执行出错时发射旁路事件。
func (b *Bot) OnPipelineError(ctx context.Context, env *core.Envelope, err error) {
	b.emitter.EmitMessageError(ctx, env.Message.TraceID, err)
}

// OnMessageDropped 在消息被 Pipeline 丢弃时发射旁路事件。
func (b *Bot) OnMessageDropped(ctx context.Context, env *core.Envelope) {
	b.emitter.EmitMessageDropped(ctx, env.Message.TraceID, "pipeline")
}

// OnBeforeDispatch 在 Dispatcher 派发前发射旁路事件。
func (b *Bot) OnBeforeDispatch(ctx context.Context, env *core.Envelope, actions []core.Action) {
	b.emitter.EmitDispatchStart(ctx, env.Message.TraceID, len(actions))
}

// OnDispatchError 在 Dispatcher 派发失败时发射旁路事件。
func (b *Bot) OnDispatchError(ctx context.Context, env *core.Envelope, err error) {
	b.dispatchErrors.Add(1)
	b.emitter.EmitDispatchError(ctx, env.Message.TraceID, err)
}

// OnMessageDone 在消息处理成功完成时发射旁路事件。
func (b *Bot) OnMessageDone(ctx context.Context, env *core.Envelope, actions []core.Action, duration time.Duration) {
	b.emitter.EmitMessageDone(ctx, env.Message.TraceID, len(actions), duration)
}
