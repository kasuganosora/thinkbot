package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Engine — 消息处理生命周期引擎
//
// Deprecated: Engine 是早期原型实现，功能已被 bot.Bot 完全覆盖。
// Bot 在 Engine 基础上增加了 EventBus 旁路事件、NoteHandler、CallbackHandler、
// SilentHandler、多 Channel 管理、panic recovery 等能力。
// 新代码应直接使用 bot.Bot + bot.BotManager。
// 此类型将在未来版本中移除。
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

// Engine 是消息处理的顶层编排器。
//
// Deprecated: 请使用 bot.Bot 替代。Engine 将在未来版本中移除。
// 详见 bot.Bot 的文档了解完整功能。
//
// 它从 Ingress 消费 Envelope，经过 Pipeline 加工，最后由 Dispatcher 派发。
//
// Engine 不管理输入端的生命周期。各 channel（webhook handler、ws handler 等）
// 自行管理启停，只需调用 Ingress.Receive() 注入消息即可。
//
// 处理流程：
//  1. N 个 worker goroutine 从 Ingress.C() 取 Envelope
//  2. 每个 Envelope 经过 Pipeline Stage 链加工
//  3. Pipeline 产出的 Action 交给 Dispatcher 派发
//  4. ctx 取消时优雅关闭：关闭 Ingress → 排空 → 等待 worker 退出
type Engine struct {
	ingress    *inbound.Ingress
	pipeline   *pipeline.Pipeline
	dispatcher outbound.Dispatcher
	config     EngineConfig
	logger     *zap.SugaredLogger
	tracer     trace.Tracer

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewEngine 创建 Engine 实例。
//
// Deprecated: 请使用 bot.New 替代。
func NewEngine(
	ingress *inbound.Ingress,
	p *pipeline.Pipeline,
	d outbound.Dispatcher,
	cfg EngineConfig,
	logger *zap.SugaredLogger,
	tp trace.TracerProvider,
) *Engine {
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultEngineConfig().Workers
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = DefaultEngineConfig().ShutdownTimeout
	}

	return &Engine{
		ingress:    ingress,
		pipeline:   p,
		dispatcher: d,
		config:     cfg,
		logger:     logger,
		tracer:     tp.Tracer("github.com/kasuganosora/thinkbot/agent"),
	}
}

// Run 启动 Engine 的消息处理循环。
// 该方法会阻塞直到 ctx 被取消或 Stop 被调用。
func (e *Engine) Run(ctx context.Context) error {
	ctx, e.cancel = context.WithCancel(ctx)

	e.logger.Infow("engine starting",
		"workers", e.config.Workers,
		"stages", e.pipeline.StageNames())

	// 启动 worker goroutine
	for i := 0; i < e.config.Workers; i++ {
		e.wg.Add(1)
		go e.worker(ctx, i)
	}

	e.logger.Infow("engine running")

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
		return fmt.Errorf("engine: shutdown timeout: %w", shutdownCtx.Err())
	}
}

// Stop 触发 Engine 优雅关闭。
func (e *Engine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
}

// Ingress 返回 Engine 使用的 Ingress 实例。
// 外部 channel 可通过此方法获取 Ingress 来注入消息。
func (e *Engine) Ingress() *inbound.Ingress {
	return e.ingress
}

// worker 是消息处理 goroutine。
// 每个 worker 从 Ingress.C() 取 Envelope → Pipeline.Execute → Dispatcher.Dispatch。
func (e *Engine) worker(ctx context.Context, id int) {
	defer e.wg.Done()

	e.logger.Debugw("worker started", "worker_id", id)

	for env := range e.ingress.C() {
		e.processEnvelope(ctx, id, env)
	}

	e.logger.Debugw("worker stopped", "worker_id", id)
}

// processEnvelope 处理单个消息信封的完整生命周期。
func (e *Engine) processEnvelope(ctx context.Context, workerID int, env *core.Envelope) {
	// 将 Envelope 的 Trace ID 注入 context —— 全链路日志和 span 的基础
	traceID := env.Message.TraceID
	ctx = traceid.WithTraceID(ctx, traceID)

	// 创建携带 trace_id 的 logger，后续本方法内所有日志自动带上
	logger := e.logger.With("trace_id", traceID)

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

	// Pipeline 执行
	result, err := e.pipeline.Execute(ctx, env)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		logger.Errorw("pipeline execution failed",
			"worker_id", workerID,
			"message_id", env.Message.ID,
			"err", err,
			"duration", time.Since(start))
		return
	}

	// Pipeline 返回 nil → 消息被丢弃
	if result == nil {
		span.SetAttributes(attribute.Bool("message.dropped", true))
		logger.Debugw("message dropped by pipeline",
			"worker_id", workerID,
			"message_id", env.Message.ID)
		return
	}

	// 收集 Action 并派发
	actions := result.Actions()
	if len(actions) == 0 {
		span.SetAttributes(attribute.Bool("message.no_actions", true))
		logger.Debugw("no actions to dispatch",
			"worker_id", workerID,
			"message_id", env.Message.ID)
		return
	}

	span.SetAttributes(attribute.Int("actions.count", len(actions)))

	if dispErr := e.dispatcher.Dispatch(ctx, actions); dispErr != nil {
		span.SetStatus(codes.Error, "dispatch failed")
		span.RecordError(dispErr)
		logger.Errorw("dispatch failed",
			"worker_id", workerID,
			"message_id", env.Message.ID,
			"actions", len(actions),
			"err", dispErr,
			"duration", time.Since(start))
		return
	}

	logger.Debugw("message processed",
		"worker_id", workerID,
		"message_id", env.Message.ID,
		"actions", len(actions),
		"duration", time.Since(start))
}
