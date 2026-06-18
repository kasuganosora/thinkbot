package outbound

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

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
	mu       sync.RWMutex
	handlers map[core.ActionType]ActionHandler
	fallback ActionHandler
	logger   *zap.SugaredLogger
	tracer   trace.Tracer

	// metrics（原子计数器，无需额外同步）
	actionsDispatched atomic.Int64 // 成功派发的 Action 总数
	actionsErrors     atomic.Int64 // 派发失败的 Action 总数
	actionsNoHandler  atomic.Int64 // 无 handler 的 Action 总数
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
// 线程安全：可在运行时调用（如 Bot.Run 中动态注册 Channel Sender）。
func (d *MultiDispatcher) Register(actionType core.ActionType, handler ActionHandler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[actionType] = handler
}

// MustRegister 注册特定 ActionType 的处理器。
// 如果相同 ActionType 已有处理器注册，panic。
// 适用于启动阶段的强制校验。
func (d *MultiDispatcher) MustRegister(actionType core.ActionType, handler ActionHandler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.handlers[actionType]; exists {
		panic(fmt.Sprintf("multi_dispatcher: handler already registered for action type %q", actionType))
	}
	d.handlers[actionType] = handler
}

// SetFallback 设置兜底处理器（无对应 handler 时使用）。
func (d *MultiDispatcher) SetFallback(handler ActionHandler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fallback = handler
}

// Validate 验证指定的 ActionType 是否都已注册了处理器。
// 通常在启动阶段调用，确保不会在运行时因缺少 handler 而静默丢弃 Action。
// 返回未注册的 ActionType 列表，空切片表示全部通过。
func (d *MultiDispatcher) Validate(requiredTypes ...core.ActionType) []core.ActionType {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var missing []core.ActionType
	for _, t := range requiredTypes {
		if _, ok := d.handlers[t]; !ok {
			missing = append(missing, t)
		}
	}
	return missing
}

// RegisteredTypes 返回所有已注册的 ActionType 列表。
func (d *MultiDispatcher) RegisteredTypes() []core.ActionType {
	d.mu.RLock()
	defer d.mu.RUnlock()
	types := make([]core.ActionType, 0, len(d.handlers))
	for t := range d.handlers {
		types = append(types, t)
	}
	return types
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
		d.mu.RLock()
		handler, ok := d.handlers[a.Type]
		if !ok {
			handler = d.fallback
		}
		d.mu.RUnlock()
		if handler == nil {
			d.actionsNoHandler.Add(1)
			d.logger.Warnw("no handler for action type",
				"type", string(a.Type),
				"channel", a.Channel)
			continue
		}

		if err := handler.Handle(ctx, a); err != nil {
			d.actionsErrors.Add(1)
			errs = append(errs, fmt.Errorf("dispatch %s to %s: %w", a.Type, a.Channel, err))
			d.logger.Errorw("action dispatch failed",
				"type", string(a.Type),
				"channel", a.Channel,
				"err", err)
		} else {
			d.actionsDispatched.Add(1)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("dispatch errors: %d/%d actions failed", len(errs), len(actions))
	}
	return nil
}

// DispatcherMetrics 是 MultiDispatcher 的运行指标快照。
type DispatcherMetrics struct {
	ActionsDispatched int64 `json:"actions_dispatched"`
	ActionsErrors     int64 `json:"actions_errors"`
	ActionsNoHandler  int64 `json:"actions_no_handler"`
	RegisteredTypes   int   `json:"registered_types"`
}

// Metrics 返回当前指标快照。
func (d *MultiDispatcher) Metrics() DispatcherMetrics {
	d.mu.RLock()
	registeredTypes := len(d.handlers)
	d.mu.RUnlock()
	return DispatcherMetrics{
		ActionsDispatched: d.actionsDispatched.Load(),
		ActionsErrors:     d.actionsErrors.Load(),
		ActionsNoHandler:  d.actionsNoHandler.Load(),
		RegisteredTypes:   registeredTypes,
	}
}
