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
)

// ============================================================================
// Engine — 消息处理生命周期引擎
// ============================================================================

// EngineConfig 控制 Engine 行为参数。
type EngineConfig struct {
	// Workers 并发 worker 数量（默认 4）。
	Workers int
	// InboundBuffer 共享输入通道缓冲区大小（默认 128）。
	InboundBuffer int
	// ShutdownTimeout 优雅关闭超时时间（默认 10s）。
	ShutdownTimeout time.Duration
}

// DefaultEngineConfig 返回合理的默认配置。
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{
		Workers:         4,
		InboundBuffer:   128,
		ShutdownTimeout: 10 * time.Second,
	}
}

// Engine 是消息处理的顶层编排器。
// 它串联 Inbound Sources → Pipeline → Outbound Dispatcher 的完整生命周期：
//  1. 启动所有 Source，消息统一写入共享 inCh
//  2. N 个 worker goroutine 从 inCh 取 Envelope，执行 Pipeline
//  3. Pipeline 产出的 Action 交给 Dispatcher 派发
//  4. ctx 取消时优雅关闭：停止 Source → 排空 inCh → 等待 worker 退出
type Engine struct {
	sources    []inbound.Source
	pipeline   *pipeline.Pipeline
	dispatcher outbound.Dispatcher
	config     EngineConfig
	logger     *zap.SugaredLogger
	tracer     trace.Tracer

	inCh   chan *core.Envelope
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewEngine 创建 Engine 实例。
func NewEngine(
	sources []inbound.Source,
	p *pipeline.Pipeline,
	d outbound.Dispatcher,
	cfg EngineConfig,
	logger *zap.SugaredLogger,
	tp trace.TracerProvider,
) *Engine {
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultEngineConfig().Workers
	}
	if cfg.InboundBuffer <= 0 {
		cfg.InboundBuffer = DefaultEngineConfig().InboundBuffer
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = DefaultEngineConfig().ShutdownTimeout
	}

	return &Engine{
		sources:    sources,
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
	e.inCh = make(chan *core.Envelope, e.config.InboundBuffer)

	e.logger.Infow("engine starting",
		"sources", len(e.sources),
		"workers", e.config.Workers,
		"buffer", e.config.InboundBuffer,
		"stages", e.pipeline.StageNames())

	// 1. 启动所有 Source
	for _, src := range e.sources {
		if err := src.Start(ctx, e.inCh); err != nil {
			// Source 启动失败 → 回滚已启动的 Source
			e.logger.Errorw("source start failed, rolling back",
				"source", src.Name(), "err", err)
			e.stopSources(ctx)
			return fmt.Errorf("engine: start source %q: %w", src.Name(), err)
		}
		e.logger.Infow("source started", "source", src.Name())
	}

	// 2. 启动 worker goroutine
	for i := 0; i < e.config.Workers; i++ {
		e.wg.Add(1)
		go e.worker(ctx, i)
	}

	e.logger.Infow("engine running")

	// 3. 阻塞直到 ctx 取消
	<-ctx.Done()

	e.logger.Infow("engine shutting down", "reason", ctx.Err())

	// 4. 停止所有 Source
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), e.config.ShutdownTimeout)
	defer shutdownCancel()

	e.stopSources(shutdownCtx)

	// 5. 关闭 inCh，让 worker 排空后退出
	close(e.inCh)

	// 6. 等待所有 worker 退出
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

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

// worker 是消息处理 goroutine。
// 每个 worker 从 inCh 取 Envelope → Pipeline.Execute → Dispatcher.Dispatch。
func (e *Engine) worker(ctx context.Context, id int) {
	defer e.wg.Done()

	e.logger.Debugw("worker started", "worker_id", id)

	for env := range e.inCh {
		e.processEnvelope(ctx, id, env)
	}

	e.logger.Debugw("worker stopped", "worker_id", id)
}

// processEnvelope 处理单个消息信封的完整生命周期。
func (e *Engine) processEnvelope(ctx context.Context, workerID int, env *core.Envelope) {
	ctx, span := e.tracer.Start(ctx, "engine.process",
		trace.WithAttributes(
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
		e.logger.Errorw("pipeline execution failed",
			"worker_id", workerID,
			"message_id", env.Message.ID,
			"err", err,
			"duration", time.Since(start))
		// AbortError 等已由 Pipeline 处理，这里不再重复
		return
	}

	// Pipeline 返回 nil → 消息被丢弃
	if result == nil {
		span.SetAttributes(attribute.Bool("message.dropped", true))
		e.logger.Debugw("message dropped by pipeline",
			"worker_id", workerID,
			"message_id", env.Message.ID)
		return
	}

	// 收集 Action 并派发
	actions := result.Actions()
	if len(actions) == 0 {
		span.SetAttributes(attribute.Bool("message.no_actions", true))
		e.logger.Debugw("no actions to dispatch",
			"worker_id", workerID,
			"message_id", env.Message.ID)
		return
	}

	span.SetAttributes(attribute.Int("actions.count", len(actions)))

	if dispErr := e.dispatcher.Dispatch(ctx, actions); dispErr != nil {
		span.SetStatus(codes.Error, "dispatch failed")
		span.RecordError(dispErr)
		e.logger.Errorw("dispatch failed",
			"worker_id", workerID,
			"message_id", env.Message.ID,
			"actions", len(actions),
			"err", dispErr,
			"duration", time.Since(start))
		return
	}

	e.logger.Debugw("message processed",
		"worker_id", workerID,
		"message_id", env.Message.ID,
		"actions", len(actions),
		"duration", time.Since(start))
}

// stopSources 停止所有已启动的 Source。
func (e *Engine) stopSources(ctx context.Context) {
	for _, src := range e.sources {
		if err := src.Stop(ctx); err != nil {
			e.logger.Warnw("source stop error",
				"source", src.Name(),
				"err", err)
		} else {
			e.logger.Infow("source stopped", "source", src.Name())
		}
	}
}
