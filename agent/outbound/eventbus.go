package outbound

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// ============================================================================
// EventBus — Pipeline 旁路事件总线
// ============================================================================

// EventType 标识事件类型。
type EventType string

const (
	// 消息生命周期事件
	EventMessageReceived EventType = "message.received"    // 消息进入 Pipeline
	EventMessageDropped  EventType = "message.dropped"     // 消息被丢弃
	EventMessageDone     EventType = "message.done"        // 消息处理完成（所有 Action 已派发）
	EventMessageError    EventType = "message.error"       // 消息处理出错

	// Pipeline Stage 事件
	EventStageEnter EventType = "stage.enter" // 进入某个 Stage
	EventStageExit  EventType = "stage.exit"  // 离开某个 Stage（含耗时）
	EventStageSkip  EventType = "stage.skip"  // Stage 被跳过
	EventStageError EventType = "stage.error" // Stage 出错（非致命，Pipeline 继续）

	// LLM 流式事件（桥接 llm.StreamPartType）
	EventLLMStart         EventType = "llm.start"          // LLM 调用开始
	EventLLMTextDelta     EventType = "llm.text_delta"     // LLM 输出文本增量
	EventLLMReasonDelta   EventType = "llm.reason_delta"   // LLM 推理文本增量
	EventLLMToolCall      EventType = "llm.tool_call"      // LLM 调用工具
	EventLLMToolResult    EventType = "llm.tool_result"    // 工具执行结果
	EventLLMStepDone      EventType = "llm.step_done"      // 单步完成
	EventLLMDone          EventType = "llm.done"           // LLM 生成完成
	EventLLMError         EventType = "llm.error"          // LLM 错误

	// 决策事件
	EventDecision EventType = "decision" // ReplyDecider 输出决策

	// Dispatch 事件
	EventDispatchStart EventType = "dispatch.start" // 开始派发 Actions
	EventDispatchDone  EventType = "dispatch.done"  // 派发完成
	EventDispatchError EventType = "dispatch.error" // 派发出错

	// Workflow 事件（DAG 工作流引擎）
	EventWorkflowSubmitted EventType = "workflow.submitted"       // 工作流已提交（正在分析）
	EventWorkflowAnalyzed  EventType = "workflow.analyzed"        // 分析完成，DAG 已生成
	EventWorkflowCompleted EventType = "workflow.completed"       // 工作流全部完成
	EventWorkflowFailed    EventType = "workflow.failed"          // 工作流失败
	EventWorkflowTerminated EventType = "workflow.terminated"     // 工作流被终止
	EventWorkflowRecovered EventType = "workflow.recovered"       // 工作流崩溃恢复

	EventWorkflowNodeStarted   EventType = "workflow.node.started"    // 节点开始执行
	EventWorkflowNodeCompleted EventType = "workflow.node.completed"  // 节点完成
	EventWorkflowNodeFailed    EventType = "workflow.node.failed"     // 节点失败
	EventWorkflowNodeReviewing EventType = "workflow.node.reviewing"  // 节点进入审查
	EventWorkflowNodeRetrying  EventType = "workflow.node.retrying"   // 节点重试
)

// Event 是旁路事件总线中传递的事件。
// Web 端通过 SSE 订阅后收到的 JSON payload 即对应此结构。
type Event struct {
	// Type 事件类型。
	Type EventType `json:"type"`
	// TraceID 关联的消息追踪 ID（与 Envelope.Message.TraceID 一致）。
	// 订阅方通过 TraceID 筛选感兴趣的消息流。
	TraceID string `json:"trace_id"`
	// BotID 产生事件的 Bot ID。
	BotID string `json:"bot_id,omitempty"`
	// Timestamp 事件产生时间。
	Timestamp time.Time `json:"timestamp"`
	// Stage 当前 Stage 名称（Stage 事件使用）。
	Stage string `json:"stage,omitempty"`
	// Data 事件载荷（各事件类型不同）。
	Data map[string]any `json:"data,omitempty"`
}

// ============================================================================
// EventBus 接口
// ============================================================================

// EventBus 是旁路事件发布/订阅总线。
// 设计目标：
//   - 允许 Web SSE handler 按 trace_id（或全量）订阅消息处理进度
//   - Pipeline/Bot 在处理过程中通过 Publish 发射事件
//   - 非阻塞：Publish 不应阻塞 Pipeline 执行
//   - 多消费者：同一 trace_id 可被多个 SSE 连接订阅
//
// 典型使用场景：
//
//	// SSE Handler 中：
//	sub := bus.Subscribe(traceID)
//	defer bus.Unsubscribe(sub)
//	for event := range sub.C() { ... send SSE event ... }
//
//	// Pipeline/Bot 中：
//	bus.Publish(ctx, Event{Type: EventStageEnter, TraceID: traceID, ...})
type EventBus interface {
	// Publish 发布事件。非阻塞——如果订阅者的 channel 已满则丢弃。
	Publish(ctx context.Context, event Event)

	// Subscribe 订阅事件流。
	// traceID 为空时订阅所有事件（管理/调试用）。
	// 返回的 Subscription 必须在使用完毕后调用 Unsubscribe 释放。
	Subscribe(traceID string) *Subscription

	// SubscribeBot 订阅指定 Bot 的所有事件。
	// 返回的 Subscription 必须在使用完毕后调用 Unsubscribe 释放。
	SubscribeBot(botID string) *Subscription

	// Unsubscribe 取消订阅并关闭 channel。
	Unsubscribe(sub *Subscription)

	// Close 关闭 EventBus，关闭所有活跃订阅。
	Close()
}

// Subscription 代表一个事件订阅。
type Subscription struct {
	// ID 订阅唯一标识。
	ID string
	// TraceID 订阅的目标 trace（空表示订阅全部）。
	TraceID string
	// BotID 订阅的目标 Bot（空表示不限 Bot）。
	BotID string
	// ch 事件接收通道。
	ch chan Event
	// closed 标记是否已关闭。
	closed bool
	mu     sync.Mutex
}

// C 返回事件接收通道。通道关闭表示订阅已结束。
func (s *Subscription) C() <-chan Event {
	return s.ch
}

// send 非阻塞发送事件。channel 满时丢弃并返回 false。
func (s *Subscription) send(event Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.ch <- event:
		return true
	default:
		// channel 满，丢弃事件（旁路不应影响主流程）
		return false
	}
}

// close 关闭订阅通道。
func (s *Subscription) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

// ============================================================================
// MemoryEventBus — 内存实现
// ============================================================================

// MemoryEventBusConfig 配置内存事件总线。
type MemoryEventBusConfig struct {
	// SubscriptionBufferSize 每个订阅的 channel buffer 大小（默认 64）。
	SubscriptionBufferSize int
	// MaxSubscriptions 最大订阅数量（防止泄漏，默认 1000）。
	MaxSubscriptions int
}

// DefaultMemoryEventBusConfig 返回默认配置。
func DefaultMemoryEventBusConfig() MemoryEventBusConfig {
	return MemoryEventBusConfig{
		SubscriptionBufferSize: 64,
		MaxSubscriptions:       1000,
	}
}

// MemoryEventBus 是基于内存的 EventBus 实现。
// 适用于单机部署场景。多实例部署时需要替换为 Redis Pub/Sub 等实现。
type MemoryEventBus struct {
	config MemoryEventBusConfig
	logger *zap.SugaredLogger

	mu          sync.RWMutex
	subscribers map[string]*Subscription // id -> subscription
	nextID      uint64
	closed      bool

	// metrics（原子计数器，无需加锁）
	eventsPublished atomic.Int64 // 成功投递的事件总数（每个订阅者一计）
	eventsDropped   atomic.Int64 // 因 channel 满而丢弃的事件总数
}

// NewMemoryEventBus 创建内存事件总线。
func NewMemoryEventBus(config MemoryEventBusConfig, logger *zap.SugaredLogger) *MemoryEventBus {
	if config.SubscriptionBufferSize <= 0 {
		config.SubscriptionBufferSize = DefaultMemoryEventBusConfig().SubscriptionBufferSize
	}
	if config.MaxSubscriptions <= 0 {
		config.MaxSubscriptions = DefaultMemoryEventBusConfig().MaxSubscriptions
	}
	return &MemoryEventBus{
		config:      config,
		logger:      logger,
		subscribers: make(map[string]*Subscription),
	}
}

// Publish 发布事件到所有匹配的订阅者。
func (b *MemoryEventBus) Publish(_ context.Context, event Event) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}

	for _, sub := range b.subscribers {
		if b.matches(sub, event) {
			if sub.send(event) {
				b.eventsPublished.Add(1)
			} else {
				b.eventsDropped.Add(1)
				b.logger.Debugw("eventbus: event dropped (channel full)",
					"sub_id", sub.ID,
					"trace_id", event.TraceID,
					"event_type", string(event.Type))
			}
		}
	}
}

// Subscribe 订阅指定 trace_id 的事件（空字符串 = 全部事件）。
func (b *MemoryEventBus) Subscribe(traceID string) *Subscription {
	return b.subscribe(traceID, "")
}

// SubscribeBot 订阅指定 Bot 的所有事件。
func (b *MemoryEventBus) SubscribeBot(botID string) *Subscription {
	return b.subscribe("", botID)
}

func (b *MemoryEventBus) subscribe(traceID, botID string) *Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		// 已关闭，返回一个已关闭的订阅
		sub := &Subscription{
			ID:      "closed",
			TraceID: traceID,
			BotID:   botID,
			ch:      make(chan Event),
			closed:  true,
		}
		close(sub.ch)
		return sub
	}

	// 检查订阅数量限制
	if len(b.subscribers) >= b.config.MaxSubscriptions {
		b.logger.Warnw("eventbus: max subscriptions reached, rejecting",
			"max", b.config.MaxSubscriptions,
			"current", len(b.subscribers))
		sub := &Subscription{
			ID:      "rejected",
			TraceID: traceID,
			BotID:   botID,
			ch:      make(chan Event),
			closed:  true,
		}
		close(sub.ch)
		return sub
	}

	b.nextID++
	id := fmt.Sprintf("sub-%d", b.nextID)

	sub := &Subscription{
		ID:      id,
		TraceID: traceID,
		BotID:   botID,
		ch:      make(chan Event, b.config.SubscriptionBufferSize),
	}

	b.subscribers[id] = sub

	b.logger.Debugw("eventbus: new subscription",
		"sub_id", id,
		"trace_id", traceID,
		"bot_id", botID,
		"active_subs", len(b.subscribers))

	return sub
}

// Unsubscribe 取消订阅并关闭 channel。
func (b *MemoryEventBus) Unsubscribe(sub *Subscription) {
	if sub == nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.subscribers, sub.ID)
	sub.close()

	b.logger.Debugw("eventbus: unsubscribed",
		"sub_id", sub.ID,
		"active_subs", len(b.subscribers))
}

// Close 关闭 EventBus，关闭所有活跃订阅。
func (b *MemoryEventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed = true

	for id, sub := range b.subscribers {
		sub.close()
		delete(b.subscribers, id)
	}

	b.logger.Infow("eventbus: closed")
}

// ActiveSubscriptions 返回当前活跃订阅数量（用于监控）。
func (b *MemoryEventBus) ActiveSubscriptions() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

// Metrics 返回 EventBus 的运行指标快照。
// 可用于接入 OTel metric callback 或健康检查端点。
type EventBusMetrics struct {
	ActiveSubscriptions int   `json:"active_subscriptions"`
	EventsPublished     int64 `json:"events_published"`
	EventsDropped       int64 `json:"events_dropped"`
}

// Metrics 返回当前指标快照。
func (b *MemoryEventBus) Metrics() EventBusMetrics {
	return EventBusMetrics{
		ActiveSubscriptions: b.ActiveSubscriptions(),
		EventsPublished:     b.eventsPublished.Load(),
		EventsDropped:       b.eventsDropped.Load(),
	}
}

// matches 判断事件是否匹配订阅。
func (b *MemoryEventBus) matches(sub *Subscription, event Event) bool {
	// TraceID 匹配：空 = 全部匹配
	if sub.TraceID != "" && sub.TraceID != event.TraceID {
		return false
	}
	// BotID 匹配：空 = 全部匹配
	if sub.BotID != "" && sub.BotID != event.BotID {
		return false
	}
	return true
}


