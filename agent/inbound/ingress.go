package inbound

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// Ingress — 消息入口网关
// ============================================================================

// Ingress 是 Inbound 层的公共入口。
// 任何 channel（Webhook handler、WebSocket handler、Polling loop、测试等）
// 都通过调用 Ingress.Receive() 将消息注入 Pipeline。
//
// Ingress 负责：
//  1. 消息归一化（填充默认值）
//  2. 封装为 Envelope
//  3. 投递到内部 channel 供 Engine worker 消费
//
// Ingress 不关心消息从哪里来，也不管理输入端的生命周期。
// 输入端自行管理自己的启停（如 HTTP server、WS 连接等）。
type Ingress struct {
	ch     chan *core.Envelope
	tracer trace.Tracer
	logger *zap.SugaredLogger
	closed atomic.Bool
}

// IngressConfig 控制 Ingress 行为参数。
type IngressConfig struct {
	// BufferSize 内部缓冲区大小（默认 256）。
	// 控制输入端和 Pipeline worker 之间的背压。
	BufferSize int
}

// DefaultIngressConfig 返回合理的默认配置。
func DefaultIngressConfig() IngressConfig {
	return IngressConfig{
		BufferSize: 256,
	}
}

// NewIngress 创建消息入口网关。
func NewIngress(
	cfg IngressConfig,
	logger *zap.SugaredLogger,
	tp trace.TracerProvider,
) *Ingress {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = DefaultIngressConfig().BufferSize
	}

	return &Ingress{
		ch:     make(chan *core.Envelope, cfg.BufferSize),
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/inbound"),
		logger: logger,
	}
}

// Receive 将一条消息注入 Pipeline。
// 这是各 channel 输入端调用的唯一公共方法。
//
// Receive 会：
//   - 填充缺失的默认字段（ID、CreatedAt）
//   - 封装为 Envelope
//   - 投递到内部 channel
//
// 如果 Ingress 已关闭，返回错误。
// 如果内部缓冲区已满，Receive 会阻塞直到有空间或 ctx 被取消。
func (g *Ingress) Receive(ctx context.Context, msg core.Message) error {
	if g.closed.Load() {
		return fmt.Errorf("ingress: closed")
	}

	// 归一化：填充缺失的默认字段
	if msg.ID == "" {
		msg.ID = idgen.New("msg")
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	// 自动分配 Trace ID —— 这是全链路可观测性的起点。
	// 如果调用方已设置（如从 HTTP 请求头传入），则保留；否则生成新 ID。
	if msg.TraceID == "" {
		// 优先从 context 中提取（上游 HTTP Middleware 可能已注入）
		if ctxTraceID := traceid.FromContext(ctx); ctxTraceID != "" {
			msg.TraceID = ctxTraceID
		} else {
			msg.TraceID = traceid.New()
		}
	}

	// 将 trace ID 注入 context，供下游 OTel span 和日志使用
	ctx = traceid.WithTraceID(ctx, msg.TraceID)

	// 封装 Envelope
	env := core.NewEnvelope(msg)

	// 追踪
	_, span := g.tracer.Start(ctx, "ingress.receive",
		trace.WithAttributes(
			attribute.String("trace.id", msg.TraceID),
			attribute.String("message.id", msg.ID),
			attribute.String("message.source", msg.Source),
			attribute.String("message.channel", msg.Channel),
		))
	defer span.End()

	// 投递
	select {
	case g.ch <- env:
		g.logger.Debugw("message received",
			"trace_id", msg.TraceID,
			"message_id", msg.ID,
			"source", msg.Source,
			"channel", msg.Channel)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ingress: context cancelled: %w", ctx.Err())
	}
}

// TryReceive 尝试非阻塞地注入一条消息。
// 如果缓冲区已满，返回 false。
func (g *Ingress) TryReceive(msg core.Message) bool {
	if g.closed.Load() {
		return false
	}

	if msg.ID == "" {
		msg.ID = idgen.New("msg")
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}

	// 自动分配 Trace ID
	if msg.TraceID == "" {
		msg.TraceID = traceid.New()
	}

	env := core.NewEnvelope(msg)

	select {
	case g.ch <- env:
		return true
	default:
		return false
	}
}

// C 返回 Envelope 消费通道。
// Engine 的 worker goroutine 从此通道读取消息进行处理。
func (g *Ingress) C() <-chan *core.Envelope {
	return g.ch
}

// Close 关闭 Ingress，停止接收新消息。
// 已在缓冲区中的消息仍可被消费。
func (g *Ingress) Close() {
	if g.closed.CompareAndSwap(false, true) {
		close(g.ch)
		g.logger.Infow("ingress closed")
	}
}

// Len 返回当前缓冲区中待处理的消息数量。
func (g *Ingress) Len() int {
	return len(g.ch)
}
