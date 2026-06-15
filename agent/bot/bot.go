package bot

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
// Bot — 独立的消息处理单元
// ============================================================================

// Bot 是平台中一个独立的机器人实例。
// 每个 Bot 拥有：
//   - 唯一 ID 和显示名
//   - 独立的 BotConfig（LLM 配置、system prompt 等）
//   - 独立的 Ingress（消息入口网关）
//   - 独立的 Pipeline（Stage 链）
//   - 独立的 Dispatcher（输出派发器）
//   - 自己的 Channel 实例（输入端）
//   - 独立的 worker pool
//
// 一个应用可以运行多个 Bot，每个 Bot 处理自己 Channel 收到的消息，
// 互不干扰。
//
// 消息流转路径：
//
//	Channel.onMessage()
//	  → msg.BotID = channel.BotID()  // Channel 天然知道所属 Bot
//	  → bot.ingress.Receive(ctx, msg)
//	  → bot worker 从 ingress.C() 消费
//	  → bot.pipeline.Execute(ctx, env)
//	  → bot.dispatcher.Dispatch(ctx, actions)
type Bot struct {
	// ID Bot 唯一标识（如 "customer-service"、"code-review"）。
	ID string
	// Name Bot 显示名称。
	Name string
	// Config Bot 级别配置。
	Config BotConfig

	ingress    *inbound.Ingress
	pipeline   *pipeline.Pipeline
	dispatcher outbound.Dispatcher
	channels   []Channel
	logger     *zap.SugaredLogger
	tracer     trace.Tracer

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// BotParams 是 Bot 构造参数。
type BotParams struct {
	ID         string
	Name       string
	Config     BotConfig
	Pipeline   *pipeline.Pipeline
	Dispatcher outbound.Dispatcher
	Channels   []Channel
	Logger     *zap.SugaredLogger
	TP         trace.TracerProvider
}

// New 创建一个 Bot 实例。
// 创建后需要调用 Run 启动消息处理循环。
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

	// 每个 Bot 拥有独立的 Ingress
	ingress := inbound.NewIngress(
		inbound.IngressConfig{BufferSize: cfg.IngressBufferSize},
		params.Logger.With("bot_id", params.ID, "component", "ingress"),
		params.TP,
	)

	return &Bot{
		ID:         params.ID,
		Name:       params.Name,
		Config:     cfg,
		ingress:    ingress,
		pipeline:   params.Pipeline,
		dispatcher: params.Dispatcher,
		channels:   params.Channels,
		logger:     params.Logger.With("bot_id", params.ID),
		tracer:     params.TP.Tracer("github.com/kasuganosora/thinkbot/agent/bot/" + params.ID),
	}, nil
}

// Run 启动 Bot 的消息处理循环。
// 它会：
//  1. 启动所有 Channel（Channel.Start 拿到 Ingress）
//  2. 启动 N 个 worker goroutine 从 Ingress 消费 → Pipeline → Dispatch
//  3. 阻塞直到 ctx 取消
//  4. 优雅关闭：停止 Channel → 关闭 Ingress → 排空 worker
func (b *Bot) Run(ctx context.Context) error {
	ctx, b.cancel = context.WithCancel(ctx)

	b.logger.Infow("bot starting",
		"name", b.Name,
		"workers", b.Config.Workers,
		"channels", len(b.channels),
		"pipeline_stages", b.pipeline.StageNames())

	// 启动所有 Channel
	for _, ch := range b.channels {
		b.logger.Infow("starting channel",
			"channel_name", ch.Name(),
			"channel_type", ch.Type())

		if err := ch.Start(ctx, b.ingress); err != nil {
			// 启动失败，停止已启动的 Channel
			b.logger.Errorw("channel start failed, rolling back",
				"channel_name", ch.Name(),
				"err", err)
			b.stopChannels(ctx)
			return fmt.Errorf("bot %q: channel %q start failed: %w", b.ID, ch.Name(), err)
		}
	}

	// 启动 worker goroutine
	for i := 0; i < b.Config.Workers; i++ {
		b.wg.Add(1)
		go b.worker(ctx, i)
	}

	b.logger.Infow("bot running")

	// 阻塞直到 ctx 取消
	<-ctx.Done()

	b.logger.Infow("bot shutting down", "reason", ctx.Err())

	// 停止所有 Channel
	b.stopChannels(context.Background())

	// 关闭 Ingress，worker 会排空后退出
	b.ingress.Close()

	// 等待所有 worker 退出
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		b.logger.Infow("bot stopped gracefully")
		return nil
	case <-time.After(10 * time.Second):
		b.logger.Warnw("bot shutdown timed out")
		return fmt.Errorf("bot %q: shutdown timeout", b.ID)
	}
}

// Stop 触发 Bot 优雅关闭。
func (b *Bot) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
}

// Ingress 返回 Bot 私有的 Ingress 实例。
// Channel 实现通过此方法获取 Ingress（或直接通过 Start 参数）。
func (b *Bot) Ingress() *inbound.Ingress {
	return b.ingress
}

// Channels 返回 Bot 拥有的所有 Channel 列表。
func (b *Bot) Channels() []Channel {
	return b.channels
}

// stopChannels 停止所有 Channel（尽力而为，不因单个失败中止其他）。
func (b *Bot) stopChannels(ctx context.Context) {
	for _, ch := range b.channels {
		if err := ch.Stop(ctx); err != nil {
			b.logger.Warnw("channel stop error",
				"channel_name", ch.Name(),
				"err", err)
		}
	}
}

// worker 是消息处理 goroutine。
func (b *Bot) worker(ctx context.Context, id int) {
	defer b.wg.Done()

	b.logger.Debugw("worker started", "worker_id", id)

	for env := range b.ingress.C() {
		b.processEnvelope(ctx, id, env)
	}

	b.logger.Debugw("worker stopped", "worker_id", id)
}

// processEnvelope 处理单个消息信封的完整生命周期。
func (b *Bot) processEnvelope(ctx context.Context, workerID int, env *core.Envelope) {
	traceID := env.Message.TraceID
	ctx = traceid.WithTraceID(ctx, traceID)

	logger := b.logger.With("trace_id", traceID)

	ctx, span := b.tracer.Start(ctx, "bot.process",
		trace.WithAttributes(
			attribute.String("bot.id", b.ID),
			attribute.String("trace.id", traceID),
			attribute.Int("worker.id", workerID),
			attribute.String("message.id", env.Message.ID),
			attribute.String("message.bot_id", env.Message.BotID),
			attribute.String("message.source", env.Message.Source),
			attribute.String("message.channel", env.Message.Channel),
		))
	defer span.End()

	// 将 BotConfig 注入 Envelope KV，供 Stage 使用
	env.Set("bot.id", b.ID)
	env.Set("bot.config", b.Config)

	start := time.Now()

	// Pipeline 执行
	result, err := b.pipeline.Execute(ctx, env)
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

	if result == nil {
		span.SetAttributes(attribute.Bool("message.dropped", true))
		logger.Debugw("message dropped by pipeline",
			"worker_id", workerID,
			"message_id", env.Message.ID)
		return
	}

	actions := result.Actions()
	if len(actions) == 0 {
		span.SetAttributes(attribute.Bool("message.no_actions", true))
		logger.Debugw("no actions to dispatch",
			"worker_id", workerID,
			"message_id", env.Message.ID)
		return
	}

	span.SetAttributes(attribute.Int("actions.count", len(actions)))

	if dispErr := b.dispatcher.Dispatch(ctx, actions); dispErr != nil {
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
