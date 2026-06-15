package outbound

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// Dispatcher — 消息派发接口
// ============================================================================

// Dispatcher 负责将 Pipeline 产出的 Action 派发到对应的外部渠道。
type Dispatcher interface {
	// Dispatch 执行一批 Action。
	// 实现应尽力确保每个 Action 被投递，并在失败时返回聚合错误。
	Dispatch(ctx context.Context, actions []core.Action) error
}

// ============================================================================
// LogDispatcher — 日志记录派发器（开发/测试用）
// ============================================================================

// LogDispatcher 将所有 Action 记录到日志中，不做实际投递。
// 适用于开发阶段和集成测试。
type LogDispatcher struct {
	logger *zap.SugaredLogger
	tracer trace.Tracer
}

// NewLogDispatcher 创建日志派发器。
func NewLogDispatcher(logger *zap.SugaredLogger, tp trace.TracerProvider) *LogDispatcher {
	return &LogDispatcher{
		logger: logger,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/outbound"),
	}
}

// Dispatch 将每个 Action 记录到日志。
func (d *LogDispatcher) Dispatch(ctx context.Context, actions []core.Action) error {
	ctx, span := d.tracer.Start(ctx, "outbound.dispatch",
		trace.WithAttributes(
			attribute.Int("actions.count", len(actions)),
		))
	defer span.End()

	for i, a := range actions {
		d.logger.Infow("dispatch action",
			"index", i,
			"type", string(a.Type),
			"channel", a.Channel,
			"user_id", a.UserID,
			"payload_type", fmt.Sprintf("%T", a.Payload))

		span.AddEvent("action.dispatched",
			trace.WithAttributes(
				attribute.Int("action.index", i),
				attribute.String("action.type", string(a.Type)),
				attribute.String("action.channel", a.Channel),
			))
	}

	return nil
}

// ============================================================================
// MultiDispatcher — 多路派发器
// ============================================================================

// MultiDispatcher 根据 ActionType 路由到不同的处理器。
type MultiDispatcher struct {
	handlers map[core.ActionType]ActionHandler
	fallback ActionHandler
	logger   *zap.SugaredLogger
	tracer   trace.Tracer
}

// ActionHandler 处理单个 Action。
type ActionHandler interface {
	Handle(ctx context.Context, action core.Action) error
}

// ActionHandlerFunc 将函数适配为 ActionHandler。
type ActionHandlerFunc func(ctx context.Context, action core.Action) error

func (f ActionHandlerFunc) Handle(ctx context.Context, action core.Action) error {
	return f(ctx, action)
}

// NewMultiDispatcher 创建多路派发器。
func NewMultiDispatcher(logger *zap.SugaredLogger, tp trace.TracerProvider) *MultiDispatcher {
	return &MultiDispatcher{
		handlers: make(map[core.ActionType]ActionHandler),
		logger:   logger,
		tracer:   tp.Tracer("github.com/kasuganosora/thinkbot/agent/outbound"),
	}
}

// Register 注册特定 ActionType 的处理器。
func (d *MultiDispatcher) Register(actionType core.ActionType, handler ActionHandler) {
	d.handlers[actionType] = handler
}

// SetFallback 设置兜底处理器（无对应 handler 时使用）。
func (d *MultiDispatcher) SetFallback(handler ActionHandler) {
	d.fallback = handler
}

// Dispatch 按 ActionType 路由到对应处理器。
func (d *MultiDispatcher) Dispatch(ctx context.Context, actions []core.Action) error {
	ctx, span := d.tracer.Start(ctx, "outbound.multi_dispatch",
		trace.WithAttributes(
			attribute.Int("actions.count", len(actions)),
		))
	defer span.End()

	var errs []error
	for _, a := range actions {
		handler, ok := d.handlers[a.Type]
		if !ok {
			handler = d.fallback
		}
		if handler == nil {
			d.logger.Warnw("no handler for action type",
				"type", string(a.Type),
				"channel", a.Channel)
			continue
		}

		if err := handler.Handle(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("dispatch %s to %s: %w", a.Type, a.Channel, err))
			d.logger.Errorw("action dispatch failed",
				"type", string(a.Type),
				"channel", a.Channel,
				"err", err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("dispatch errors: %d/%d actions failed", len(errs), len(actions))
	}
	return nil
}
