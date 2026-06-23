package inbound

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/errs"
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

	// selfIDs 记录 Bot 在各平台上的自身用户 ID。
	// Channel 在 Start() 时通过 RegisterSelfUserID 注册，
	// Ingress 在 Receive 时检查并丢弃 Bot 自己发出的消息，
	// 防止 Bot 回复自己形成无限循环。
	//
	// selfIDs 是一个 *SelfIDSet，可与 Engagement 层的 SelfExclusionRule 共享，
	// 确保两层防线引用同一份数据，无需时序协调。
	selfIDs     *SelfIDSet
	selfIDsOnce sync.Once
}

// IngressConfig 控制 Ingress 行为参数。
type IngressConfig struct {
	// BufferSize 内部缓冲区大小（默认 256）。
	// 控制输入端和 Pipeline worker 之间的背压。
	BufferSize int

	// SelfIDSet 可选的外部 SelfIDSet。
	// 如果提供，Ingress 将使用它来存储和检查 Bot 自身用户 ID，
	// 允许 Engagement 层等外部组件共享同一份数据。
	// 如果为 nil，Ingress 会创建一个内部的 SelfIDSet。
	SelfIDSet *SelfIDSet
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

	selfIDs := cfg.SelfIDSet
	if selfIDs == nil {
		// 在构造时初始化，避免 lazyInitSelfIDs 的并发安全问题
		selfIDs = NewSelfIDSet()
	}

	return &Ingress{
		ch:      make(chan *core.Envelope, cfg.BufferSize),
		tracer:  tp.Tracer("github.com/kasuganosora/thinkbot/agent/inbound"),
		logger:  logger,
		selfIDs: selfIDs,
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

	// 自消息过滤——防止 Bot 回复自己的消息形成无限循环。
	// Channel 层通常已有过滤（如 Misskey 在 streaming 中排除自帖），
	// 这里是中央化的第二道防线，确保即使新增 Channel 也不会遗漏。
	if g.isSelfMessage(msg.UserID) {
		return nil
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
	// 使用闭包 + recover 安全发送，防止 Close() 与 Receive() 的竞态导致 panic
	sent := false
	func() {
		defer func() { _ = recover() }()
		select {
		case g.ch <- env:
			sent = true
		case <-ctx.Done():
		}
	}()
	if !sent {
		if g.closed.Load() {
			return fmt.Errorf("ingress: closed")
		}
		return errs.Wrap(ctx.Err(), "ingress: context cancelled")
	}
	g.logger.Debugw("message received",
		"trace_id", msg.TraceID,
		"message_id", msg.ID,
		"source", msg.Source,
		"channel", msg.Channel)
	return nil
}

// TryReceive 尝试非阻塞地注入一条消息。
// 如果缓冲区已满，返回 false。
func (g *Ingress) TryReceive(msg core.Message) bool {
	if g.closed.Load() {
		return false
	}

	// 自消息过滤（同 Receive）
	if g.isSelfMessage(msg.UserID) {
		return true // 静默丢弃，返回 true 表示"已处理"
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

	sent := false
	func() {
		defer func() { _ = recover() }()
		select {
		case g.ch <- env:
			sent = true
		default:
		}
	}()
	return sent
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

// ============================================================================
// 自消息过滤——防止 Bot 回复自己
// ============================================================================

// RegisterSelfUserID 注册 Bot 在某平台上的自身用户 ID。
//
// Channel 在 Start() 中发现自身身份后（如 Misskey 的 getSelf、Telegram 的 getMe）
// 应立即调用此方法注册，然后才开始接收消息。
//
// 注册后，任何 UserID 匹配的入站消息都会被 Ingress 静默丢弃，
// 确保即使 Channel 层的过滤逻辑有遗漏，也不会形成 Bot 回复自己的无限循环。
//
// userID 为空时忽略（安全无操作）。
func (g *Ingress) RegisterSelfUserID(userID string) {
	g.lazyInitSelfIDs()
	g.selfIDs.Add(userID)
}

// UnregisterSelfUserID 移除已注册的自身用户 ID。
// Channel 在 Stop() 时可调用以清理状态。
func (g *Ingress) UnregisterSelfUserID(userID string) {
	if g.selfIDs != nil {
		g.selfIDs.Remove(userID)
	}
}

// IsSelfMessage 检查消息发送者是否是 Bot 自身。
// 这是公开方法，供 Engagement 层等外部组件共享使用。
func (g *Ingress) IsSelfMessage(userID string) bool {
	g.lazyInitSelfIDs()
	return g.selfIDs.Contains(userID)
}

// SelfIDs 返回内部 SelfIDSet 的引用。
// 调用方可以在 Ingress 创建后获取此引用并传递给 Engagement 层，
// 使两层共享同一份自消息过滤数据。
func (g *Ingress) SelfIDs() *SelfIDSet {
	g.lazyInitSelfIDs()
	return g.selfIDs
}

// lazyInitSelfIDs 在首次使用时创建 SelfIDSet（如果未通过 IngressConfig 注入）。
func (g *Ingress) lazyInitSelfIDs() {
	g.selfIDsOnce.Do(func() {
		if g.selfIDs == nil {
			g.selfIDs = NewSelfIDSet()
		}
	})
}

// isSelfMessage 检查消息发送者是否是 Bot 自身。
func (g *Ingress) isSelfMessage(userID string) bool {
	if g.selfIDs == nil {
		return false
	}
	return g.selfIDs.Contains(userID)
}
