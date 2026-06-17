package outbound

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// SilentHandler — ActionSilent 处理器
// ============================================================================

// SilentHandler 处理 ActionSilent 类型的 Action。
// 它不执行任何外部 I/O，仅记录 trace 和日志。
//
// 与不注册 handler 导致 MultiDispatcher warn 的区别：
// SilentHandler 显式确认"这是一个有意的静默决策"，并记录到 tracing 系统中，
// 方便后续分析 Bot 的决策分布（多少消息回复了、多少静默了）。
//
// 典型场景：
//   - 群聊中的闲聊，LLM 判定不需要参与
//   - 重复/垃圾消息，已识别但无需回应
//   - 信息量不足，Bot 选择观望等待更多上下文
type SilentHandler struct {
	logger *zap.SugaredLogger
	tracer trace.Tracer
}

// NewSilentHandler 创建静默处理器。
func NewSilentHandler(
	logger *zap.SugaredLogger,
	tp trace.TracerProvider,
) *SilentHandler {
	return &SilentHandler{
		logger: logger.Named("silent_handler"),
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/outbound/silent"),
	}
}

// Handle 处理静默动作——仅记录 trace，不做任何外部输出。
func (h *SilentHandler) Handle(ctx context.Context, action core.Action) error {
	_, span := h.tracer.Start(ctx, "outbound.silent.handle",
		trace.WithAttributes(
			attribute.String("action.type", string(action.Type)),
			attribute.String("action.channel", action.Channel),
			attribute.String("action.user_id", action.UserID),
		))
	defer span.End()

	// 提取可选的静默原因
	reason := "unspecified"
	if action.Metadata != nil {
		if r, ok := action.Metadata["reason"]; ok {
			if s, ok := r.(string); ok && s != "" {
				reason = s
			}
		}
	}

	span.SetAttributes(attribute.String("silent.reason", reason))

	h.logger.Debugw("message silenced (no output)",
		"channel", action.Channel,
		"user_id", action.UserID,
		"reason", reason,
	)

	return nil
}
