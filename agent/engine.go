package agent

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Engine — 消息处理生命周期引擎（轻量级内核）
//
// Engine 是 thinkbot agent 的核心运行时，负责 Ingress → Pipeline → Dispatcher
// 的基础消息处理流水线。它足够轻量，可以被不同层级的上层组件复用：
//
//   - bot.Bot：全功能 Bot，在 Engine 上叠加 EventBus、Channel 管理、Handler 自动注册等
//   - SubAgent（计划中）：轻量级子代理，只需要 Engine 的流水线能力
//
// Engine 不管理 Channel 生命周期。Channel（webhook、ws 等）自行管理启停，
// 通过 Ingress.Receive() 注入消息即可。
//
// 处理流程：
//  1. N 个 worker goroutine 从 Ingress.C() 取 Envelope
//  2. 每个 Envelope 经过 Pipeline Stage 链加工
//  3. Pipeline 产出的 Action 交给 Dispatcher 派发
//  4. ctx 取消时优雅关闭：关闭 Ingress → 排空 → 等待 worker 退出
//
// 扩展机制：
// 通过 EngineHook 在消息处理的关键节点注入自定义行为（事件发射、metrics、
// context 增强等），无需子类化或复制代码。
// ============================================================================

// EngineConfig 控制 Engine 行为参数。
type EngineConfig struct {
	// Workers 并发 worker 数量（默认 4）。
	Workers int
	// ShutdownTimeout 优雅关闭超时时间（默认 10s）。
	ShutdownTimeout time.Duration
}

// DefaultEngineConfig 返回合理的默认配置。
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		Workers:         4,
		ShutdownTimeout: 10 * time.Second,
	}
}

// EngineHook 定义 Engine 处理消息时的生命周期钩子。
// 上层组件（如 Bot、SubAgent）通过实现此接口在消息处理各阶段注入行为。
//
// 所有方法都是可选的 —— 使用 NoopEngineHook 作为基础嵌入即可只实现需要的方法。
type EngineHook interface {
	// OnBeforeProcess 在处理 Envelope 之前调用。
	// 返回的 context 将用于后续处理流程。
	// 可用于注入 EventEmitter、KV 值等到 context 或 Envelope 中。
	OnBeforeProcess(ctx context.Context, env *core.Envelope) context.Context

	// OnPipelineError 在 Pipeline 执行返回错误时调用。
	OnPipelineError(ctx context.Context, env *core.Envelope, err error)

	// OnMessageDropped 在 Pipeline 返回 nil（消息被丢弃）时调用。
	OnMessageDropped(ctx context.Context, env *core.Envelope)

	// OnBeforeDispatch 在 Dispatcher 派发 Action 之前调用。
	OnBeforeDispatch(ctx context.Context, env *core.Envelope, actions []core.Action)

	// OnDispatchError 在 Dispatcher 派发失败时调用。
	OnDispatchError(ctx context.Context, env *core.Envelope, err error)

	// OnMessageDone 在消息处理成功完成时调用。
	OnMessageDone(ctx context.Context, env *core.Envelope, actions []core.Action, duration time.Duration)
}

// NoopEngineHook 是 EngineHook 的空实现，上层组件可以嵌入它只覆盖需要的方法。
type NoopEngineHook struct{}

func (NoopEngineHook) OnBeforeProcess(ctx context.Context, _ *core.Envelope) context.Context {
	return ctx
}
func (NoopEngineHook) OnPipelineError(context.Context, *core.Envelope, error)          {}
func (NoopEngineHook) OnMessageDropped(context.Context, *core.Envelope)                {}
func (NoopEngineHook) OnBeforeDispatch(context.Context, *core.Envelope, []core.Action) {}
func (NoopEngineHook) OnDispatchError(context.Context, *core.Envelope, error)          {}
func (NoopEngineHook) OnMessageDone(context.Context, *core.Envelope, []core.Action, time.Duration) {
}

// Engine 是消息处理的轻量级内核引擎。
// 从 Ingress 消费 Envelope，经 Pipeline 加工，由 Dispatcher 派发。
type Engine struct {
	ingress    *inbound.Ingress
	pipeline   *pipeline.Pipeline
	dispatcher outbound.Dispatcher
	config     EngineConfig
	hook       EngineHook
	logger     *zap.SugaredLogger
	tracer     trace.Tracer

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	readyCh chan struct{} // close 后表示 Engine 已完成初始化（worker 已启动）

	// metrics（原子计数器）
	messagesProcessed atomic.Int64
	messagesErrors    atomic.Int64
}

// EngineOption 是 Engine 的可选配置函数。
type EngineOption func(*Engine)

// WithHook 设置 Engine 的生命周期钩子。
func WithHook(hook EngineHook) EngineOption {
	return func(e *Engine) {
		e.hook = hook
	}
}

// NewEngine 创建 Engine 实例。
func NewEngine(
	ingress *inbound.Ingress,
	p *pipeline.Pipeline,
	d outbound.Dispatcher,
	cfg EngineConfig,
	logger *zap.SugaredLogger,
	tp trace.TracerProvider,
	opts ...EngineOption,
) *Engine {
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultEngineConfig().Workers
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = DefaultEngineConfig().ShutdownTimeout
	}

	e := &Engine{
		ingress:    ingress,
		pipeline:   p,
		dispatcher: d,
		config:     cfg,
		hook:       NoopEngineHook{},
		logger:     logger,
		tracer:     tp.Tracer("github.com/kasuganosora/thinkbot/agent"),
		readyCh:    make(chan struct{}),
	}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

// Run 启动 Engine 的消息处理循环。
// 该方法会阻塞直到 ctx 被取消或 Stop 被调用。
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	ctx, e.cancel = context.WithCancel(ctx)
	e.mu.Unlock()

	e.logger.Infow("engine starting",
		"workers", e.config.Workers,
		"stages", e.pipeline.StageNames())

	// 启动 worker goroutine
	for i := 0; i < e.config.Workers; i++ {
		e.wg.Add(1)
		go e.worker(ctx, i)
	}

	e.logger.Infow("engine running")

	// 标记 Engine 已就绪
	close(e.readyCh)

	// 阻塞直到 ctx 取消
	<-ctx.Done()

	e.logger.Infow("engine shutting down", "reason", ctx.Err())

	// 关闭 Ingress，停止接收新消息
	e.ingress.Close()

	// 等待所有 worker 排空并退出
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), e.config.ShutdownTimeout)
	defer shutdownCancel()

	select {
	case <-done:
		e.logger.Infow("engine stopped gracefully")
		return nil
	case <-shutdownCtx.Done():
		e.logger.Warnw("engine shutdown timed out, some workers may not have finished")
		return errs.Wrap(shutdownCtx.Err(), "engine: shutdown timeout")
	}
}

// Stop 触发 Engine 优雅关闭。
func (e *Engine) Stop() {
	e.mu.Lock()
	cancel := e.cancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Ready 返回一个 channel，该 channel 在 Engine 完成初始化（worker 已启动）后关闭。
func (e *Engine) Ready() <-chan struct{} {
	return e.readyCh
}

// Ingress 返回 Engine 使用的 Ingress 实例。
// 外部 channel 可通过此方法获取 Ingress 来注入消息。
func (e *Engine) Ingress() *inbound.Ingress {
	return e.ingress
}

// Pipeline 返回 Engine 使用的 Pipeline 实例。
func (e *Engine) Pipeline() *pipeline.Pipeline {
	return e.pipeline
}

// Dispatcher 返回 Engine 使用的 Dispatcher 实例。
func (e *Engine) Dispatcher() outbound.Dispatcher {
	return e.dispatcher
}

// EngineMetrics 是 Engine 的运行指标快照。
type EngineMetrics struct {
	MessagesProcessed int64 `json:"messages_processed"`
	MessagesErrors    int64 `json:"messages_errors"`
}

// Metrics 返回 Engine 当前运行指标。
func (e *Engine) Metrics() EngineMetrics {
	return EngineMetrics{
		MessagesProcessed: e.messagesProcessed.Load(),
		MessagesErrors:    e.messagesErrors.Load(),
	}
}

// worker 是消息处理 goroutine。
func (e *Engine) worker(ctx context.Context, id int) {
	defer e.wg.Done()

	e.logger.Debugw("worker started", "worker_id", id)

	for env := range e.ingress.C() {
		e.safeProcessEnvelope(ctx, id, env)
	}

	e.logger.Debugw("worker stopped", "worker_id", id)
}

// safeProcessEnvelope 包装 processEnvelope 并添加 panic recovery。
// 单条消息的 panic 不会导致整个 worker goroutine 退出。
func (e *Engine) safeProcessEnvelope(ctx context.Context, workerID int, env *core.Envelope) {
	defer func() {
		if r := recover(); r != nil {
			stack := make([]byte, 4096)
			n := runtime.Stack(stack, false)
			stack = stack[:n]

			e.messagesErrors.Add(1)
			e.logger.Errorw("worker panic recovered",
				"worker_id", workerID,
				"message_id", env.Message.ID,
				"trace_id", env.Message.TraceID,
				"panic", r,
				"stack", string(stack))
			// 通知 hook（如 EventBus 发射 panic 事件）
			e.hook.OnPipelineError(ctx, env, fmt.Errorf("panic: %v", r))
		}
	}()
	e.processEnvelope(ctx, workerID, env)
}

// processEnvelope 处理单个消息信封的完整生命周期。
func (e *Engine) processEnvelope(ctx context.Context, workerID int, env *core.Envelope) {
	traceID := env.Message.TraceID
	ctx = traceid.WithTraceID(ctx, traceID)

	logger := traceid.WithLoggerFrom(ctx, e.logger)

	ctx, span := e.tracer.Start(ctx, "engine.process",
		trace.WithAttributes(
			attribute.String("trace.id", traceID),
			attribute.Int("worker.id", workerID),
			attribute.String("message.id", env.Message.ID),
			attribute.String("message.source", env.Message.Source),
			attribute.String("message.channel", env.Message.Channel),
		))
	defer span.End()

	start := time.Now()

	// Hook: 处理前（注入 context 增强、KV 值等）
	ctx = e.hook.OnBeforeProcess(ctx, env)

	// Pipeline 执行
	messageID := env.Message.ID
	result, err := e.pipeline.Execute(ctx, env)
	if err != nil {
		e.messagesErrors.Add(1)
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		logger.Errorw("pipeline execution failed",
			"worker_id", workerID,
			"message_id", messageID,
			"err", err,
			"duration", time.Since(start))
		e.hook.OnPipelineError(ctx, env, err)
		return
	}

	// Pipeline 返回 nil → 消息被丢弃
	if result == nil {
		span.SetAttributes(attribute.Bool("message.dropped", true))
		logger.Debugw("message dropped by pipeline",
			"worker_id", workerID,
			"message_id", messageID)
		e.hook.OnMessageDropped(ctx, env)
		return
	}

	// 收集 Action 并派发
	actions := result.Actions()
	if len(actions) == 0 {
		span.SetAttributes(attribute.Bool("message.no_actions", true))
		logger.Debugw("no actions to dispatch",
			"worker_id", workerID,
			"message_id", messageID)
		e.hook.OnMessageDone(ctx, env, nil, time.Since(start))
		return
	}

	span.SetAttributes(attribute.Int("actions.count", len(actions)))

	// Hook: 派发前
	e.hook.OnBeforeDispatch(ctx, env, actions)

	if dispErr := e.dispatcher.Dispatch(ctx, actions); dispErr != nil {
		span.SetStatus(codes.Error, "dispatch failed")
		span.RecordError(dispErr)
		logger.Errorw("dispatch failed",
			"worker_id", workerID,
			"message_id", messageID,
			"actions", len(actions),
			"err", dispErr,
			"duration", time.Since(start))
		e.hook.OnDispatchError(ctx, env, dispErr)
		return
	}

	duration := time.Since(start)
	e.messagesProcessed.Add(1)
	logger.Debugw("message processed",
		"worker_id", workerID,
		"message_id", messageID,
		"actions", len(actions),
		"duration", duration)

	e.hook.OnMessageDone(ctx, env, actions, duration)
}
