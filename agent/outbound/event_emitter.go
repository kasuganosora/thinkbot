package outbound

import (
	"context"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// EventEmitter — Pipeline 事件发射器
// ============================================================================

// EventEmitter 是一个便捷的事件发射器，封装 EventBus 的 Publish 调用。
// 设计为嵌入 Bot/Stage 使用，提供类型化的事件发射方法。
//
// 所有方法都是非阻塞的——如果 EventBus 为 nil（旁路未启用），所有调用静默返回。
type EventEmitter struct {
	bus   EventBus
	botID string
}

// NewEventEmitter 创建事件发射器。
// bus 为 nil 时所有操作静默（NoOp 模式）。
func NewEventEmitter(bus EventBus, botID string) *EventEmitter {
	return &EventEmitter{bus: bus, botID: botID}
}

// Enabled 返回事件发射器是否启用（bus 非 nil）。
func (e *EventEmitter) Enabled() bool {
	return e.bus != nil
}

// Emit 发射一个通用事件。
func (e *EventEmitter) Emit(ctx context.Context, eventType EventType, traceID string, data map[string]any) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      eventType,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data:      data,
	})
}

// EmitStage 发射一个 Stage 相关事件。
func (e *EventEmitter) EmitStage(ctx context.Context, eventType EventType, traceID, stageName string, data map[string]any) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      eventType,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Stage:     stageName,
		Data:      data,
	})
}

// --- 消息生命周期事件 ---

// EmitMessageReceived 发射消息接收事件。
func (e *EventEmitter) EmitMessageReceived(ctx context.Context, msg core.Message) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventMessageReceived,
		TraceID:   msg.TraceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"message_id": msg.ID,
			"source":     msg.Source,
			"channel":    msg.Channel,
			"user_id":    msg.UserID,
			"chat_type":  msg.ChatType,
			"text_len":   len(msg.Text),
		},
	})
}

// EmitMessageDone 发射消息处理完成事件。
func (e *EventEmitter) EmitMessageDone(ctx context.Context, traceID string, actionCount int, duration time.Duration) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventMessageDone,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"action_count": actionCount,
			"duration_ms":  duration.Milliseconds(),
		},
	})
}

// EmitMessageDropped 发射消息丢弃事件。
func (e *EventEmitter) EmitMessageDropped(ctx context.Context, traceID, droppedBy string) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventMessageDropped,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"dropped_by": droppedBy,
		},
	})
}

// EmitMessageError 发射消息处理错误事件。
func (e *EventEmitter) EmitMessageError(ctx context.Context, traceID string, err error) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventMessageError,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"error": err.Error(),
		},
	})
}

// --- Stage 事件 ---

// EmitStageEnter 发射进入 Stage 事件。
func (e *EventEmitter) EmitStageEnter(ctx context.Context, traceID, stageName string) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventStageEnter,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Stage:     stageName,
	})
}

// EmitStageExit 发射离开 Stage 事件。
func (e *EventEmitter) EmitStageExit(ctx context.Context, traceID, stageName string, duration time.Duration, err error) {
	if e.bus == nil {
		return
	}
	data := map[string]any{
		"duration_ms": duration.Milliseconds(),
	}
	if err != nil {
		data["error"] = err.Error()
	}
	e.bus.Publish(ctx, Event{
		Type:      EventStageExit,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Stage:     stageName,
		Data:      data,
	})
}

// --- LLM 事件 ---

// EmitLLMStart 发射 LLM 调用开始事件。
func (e *EventEmitter) EmitLLMStart(ctx context.Context, traceID, provider, model string) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventLLMStart,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"provider": provider,
			"model":    model,
		},
	})
}

// EmitLLMTextDelta 发射 LLM 文本增量事件。
func (e *EventEmitter) EmitLLMTextDelta(ctx context.Context, traceID, text string) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventLLMTextDelta,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"text": text,
		},
	})
}

// EmitLLMReasonDelta 发射 LLM 推理增量事件。
func (e *EventEmitter) EmitLLMReasonDelta(ctx context.Context, traceID, text string) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventLLMReasonDelta,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"text": text,
		},
	})
}

// EmitLLMToolCall 发射 LLM 工具调用事件。
func (e *EventEmitter) EmitLLMToolCall(ctx context.Context, traceID, toolName, toolCallID string) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventLLMToolCall,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"tool_name":    toolName,
			"tool_call_id": toolCallID,
		},
	})
}

// EmitLLMToolResult 发射 LLM 工具结果事件。
func (e *EventEmitter) EmitLLMToolResult(ctx context.Context, traceID, toolName, toolCallID string) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventLLMToolResult,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"tool_name":    toolName,
			"tool_call_id": toolCallID,
		},
	})
}

// EmitLLMStepDone 发射 LLM 单步完成事件。
func (e *EventEmitter) EmitLLMStepDone(ctx context.Context, traceID string, step int, finishReason string) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventLLMStepDone,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"step":          step,
			"finish_reason": finishReason,
		},
	})
}

// EmitLLMDone 发射 LLM 生成完成事件。
func (e *EventEmitter) EmitLLMDone(ctx context.Context, traceID string, totalTokens int, finishReason string) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventLLMDone,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"total_tokens":  totalTokens,
			"finish_reason": finishReason,
		},
	})
}

// EmitLLMError 发射 LLM 错误事件。
func (e *EventEmitter) EmitLLMError(ctx context.Context, traceID string, err error) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventLLMError,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"error": err.Error(),
		},
	})
}

// --- 决策事件 ---

// EmitDecision 发射输出决策事件。
func (e *EventEmitter) EmitDecision(ctx context.Context, traceID, decision string, replyLen, noteLen int) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventDecision,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"decision":  decision,
			"reply_len": replyLen,
			"note_len":  noteLen,
		},
	})
}

// --- Dispatch 事件 ---

// EmitDispatchStart 发射开始派发事件。
func (e *EventEmitter) EmitDispatchStart(ctx context.Context, traceID string, actionCount int) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventDispatchStart,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"action_count": actionCount,
		},
	})
}

// EmitDispatchDone 发射派发完成事件。
func (e *EventEmitter) EmitDispatchDone(ctx context.Context, traceID string, actionCount int, duration time.Duration) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventDispatchDone,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"action_count": actionCount,
			"duration_ms":  duration.Milliseconds(),
		},
	})
}

// EmitDispatchError 发射派发错误事件。
func (e *EventEmitter) EmitDispatchError(ctx context.Context, traceID string, err error) {
	if e.bus == nil {
		return
	}
	e.bus.Publish(ctx, Event{
		Type:      EventDispatchError,
		TraceID:   traceID,
		BotID:     e.botID,
		Timestamp: time.Now(),
		Data: map[string]any{
			"error": err.Error(),
		},
	})
}

// ============================================================================
// EmitterFromContext — 从 context 获取 EventEmitter
// ============================================================================

type eventEmitterKey struct{}

// ContextWithEmitter 将 EventEmitter 注入 context。
func ContextWithEmitter(ctx context.Context, emitter *EventEmitter) context.Context {
	return context.WithValue(ctx, eventEmitterKey{}, emitter)
}

// EmitterFromContext 从 context 获取 EventEmitter。
// 如果 context 中没有 emitter，返回一个 NoOp emitter（bus=nil）。
func EmitterFromContext(ctx context.Context) *EventEmitter {
	if e, ok := ctx.Value(eventEmitterKey{}).(*EventEmitter); ok {
		return e
	}
	return &EventEmitter{} // NoOp
}

// TraceIDFromContext 从 context 获取 trace ID（复用 traceid 包）。
func TraceIDFromContext(ctx context.Context) string {
	return traceid.FromContext(ctx)
}
