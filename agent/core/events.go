package core

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// EventSink — append-only 活动轨迹（借鉴 deepseek-harness 的 session event stream）
// ============================================================================
//
// 设计对应 harness 的 SessionEvent<T>：一份按 source 记录「发生的一切」的
// append-only 事件流，与「真正进模型的 surface」分离。thinkbot 的
// llm/tool_truncate.go 已通过落盘指针实现了工具结果的 surface/event 分离
// （完整原文落工作空间、上下文只留预览+指针），EventSink 补齐的是
// 「活动级」事件（stage 边界、工具调用/结果、LLM 请求/响应、子 Agent 调度、
// 上下文注入）的统一记录，用于：可观测性、HITL 续跑锚点、记忆回灌去扫库。
//
// 默认 NoopSink 零侵入；可注入 MemorySink（有界环，供进程内回放/自检）
// 或日志/DB 实现。事件由 sink 内部分配单调递增 seq，调用方无需关心。

// EventKind 标识事件类型（对应 harness 的 SessionEvent.type）。
type EventKind string

const (
	// EventStageStart / EventStageEnd：pipeline 单个 stage 的进入/退出边界。
	EventStageStart EventKind = "stage/start"
	EventStageEnd   EventKind = "stage/end"

	// EventToolCall / EventToolResult：工具调用的发起与返回（含 tool name、参数摘要、结果摘要）。
	EventToolCall   EventKind = "tool/call"
	EventToolResult EventKind = "tool/result"

	// EventLLMRequest / EventLLMResponse：一次 LLM 请求与响应（含模型、token 用量、CoT 摘要）。
	EventLLMRequest  EventKind = "llm/request"
	EventLLMResponse EventKind = "llm/response"

	// EventContextInject：向 system prompt / 上下文注入内容（如记忆召回、SOUL.md）。
	EventContextInject EventKind = "context/inject"

	// EventSubAgentSpawn：派生子 Agent（subagent / workflow）。
	EventSubAgentSpawn EventKind = "subagent/spawn"

	// EventHitlDeferred / EventHitlResumed：HITL 工具审批被 defer（等待人类确认）
	// 与续跑恢复（人类决策已注入、重新编排）。对应 harness 的「审批 pending / resolved」。
	EventHitlDeferred EventKind = "hitl/deferred"
	EventHitlResumed EventKind = "hitl/resumed"
)

// Event 是轨迹流中的一条记录。
type Event struct {
	// Seq 由 sink 内部分配，单调递增，调用方无需设置。
	Seq int64 `json:"seq"`
	// Kind 事件类型。
	Kind EventKind `json:"kind"`
	// Source 来源标识，如 "pipeline"/"llm"/"tool:<name>"/"subagent:<id>"/"memory-recall"。
	Source string `json:"source"`
	// Time 事件发生时间。
	Time time.Time `json:"time"`
	// Ignorable 前向兼容标志：未知类型可安全跳过（对应 harness SessionEvent.ignorable），
	// 未来新增事件类型不会因旧消费者不认识而崩溃。
	Ignorable bool `json:"ignorable,omitempty"`
	// Surface 是否为「进入模型上下文」的事件（对应 harness 的 surface 事件）。
	// false 表示 log-only（如 chunk / usage / 子 Agent 调度），不占上下文。
	Surface bool `json:"surface"`
	// Payload 事件载荷（任意结构化数据，建议小且可序列化）。
	Payload any `json:"payload,omitempty"`
}

// EventSink 接收并持久化事件。
type EventSink interface {
	// Emit 记录一条事件。实现须线程安全（pipeline 可能被并发调用）。
	Emit(ctx context.Context, e Event)
}

// ----------------------------------------------------------------------------
// NoopSink — 零侵入默认实现
// ----------------------------------------------------------------------------

type noopSink struct{}

// NoopSink 丢弃所有事件，用于不需要轨迹收集的路径。
var NoopSink EventSink = &noopSink{}

func (noopSink) Emit(_ context.Context, _ Event) {}

// ----------------------------------------------------------------------------
// MemorySink — 有界环，进程内自检/回放（非持久）
// ----------------------------------------------------------------------------

// MemorySink 保留最近 N 条事件的有界环形缓冲，供单元测试与运行期自检。
// 线程安全。
type MemorySink struct {
	mu    sync.Mutex
	seq   atomic.Int64
	cap   int
	items []Event
}

// NewMemorySink 创建容量为 cap 的内存 sink（cap<=0 时取默认 1024）。
func NewMemorySink(cap int) *MemorySink {
	if cap <= 0 {
		cap = 1024
	}
	return &MemorySink{cap: cap, items: make([]Event, 0, cap)}
}

// Emit 记录事件并分配单调递增 seq。
func (s *MemorySink) Emit(_ context.Context, e Event) {
	e.Seq = s.seq.Add(1)
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	s.mu.Lock()
	s.items = append(s.items, e)
	if len(s.items) > s.cap {
		// 丢弃最旧的一半，避免每次都切片拷贝。
		drop := len(s.items) - s.cap
		if drop < s.cap/2 {
			drop = s.cap / 2
		}
		s.items = s.items[drop:]
	}
	s.mu.Unlock()
}

// Snapshot 返回当前缓冲的副本（按 seq 升序）。
func (s *MemorySink) Snapshot() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.items))
	copy(out, s.items)
	return out
}

// Len 返回当前缓冲条数。
func (s *MemorySink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// ----------------------------------------------------------------------------
// context 传递 — 让深层调用（tool / llm / subagent）能取到当前 sink
// ----------------------------------------------------------------------------

type eventSinkKey struct{}

// WithEventSink 将 sink 注入 context，供深层调用 Emit 事件。
func WithEventSink(ctx context.Context, sink EventSink) context.Context {
	if sink == nil {
		sink = NoopSink
	}
	return context.WithValue(ctx, eventSinkKey{}, sink)
}

// EventSinkFromContext 取出 sink；未设置时返回 NoopSink（永不 nil）。
func EventSinkFromContext(ctx context.Context) EventSink {
	if ctx != nil {
		if s, ok := ctx.Value(eventSinkKey{}).(EventSink); ok && s != nil {
			return s
		}
	}
	return NoopSink
}
