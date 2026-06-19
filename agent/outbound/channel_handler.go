package outbound

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// ChannelSender — 输出端接口（由 Channel 实现）
// ============================================================================

// ChannelSender 定义了 Channel 的回写能力。
// 此接口镜像 bot.Sender（避免 outbound 包引用 bot 包导致循环依赖）。
// Channel 实现只需实现一次 Send 方法，即可同时满足 bot.Sender 和 outbound.ChannelSender。
type ChannelSender interface {
	// Send 执行一个输出动作。
	Send(ctx context.Context, action core.Action) error
}

// ============================================================================
// ChannelReplyHandler — 桥接 Dispatcher → Channel 的回写路径
// ============================================================================

// ChannelReplyHandler 将 Pipeline 产出的 Action 路由到对应 Channel 的 Sender。
//
// 注册到 MultiDispatcher 后，它处理 ActionReply、ActionForward 等需要回写到
// Channel 的 Action 类型。
//
// 路由逻辑：
//  1. Action.Metadata["source_channel"] 指定来源 Channel 名称
//  2. 通过 Channel Name 在注册表中查找对应的 Sender
//  3. 调用 Sender.Send(ctx, action) 完成实际发送
//
// 使用方式：
//
//	handler := outbound.NewChannelReplyHandler(logger, tp)
//	handler.Register("my-tg-bot", tgChannel)  // Channel 同时实现 Sender
//	dispatcher.Register(core.ActionReply, handler)
type ChannelReplyHandler struct {
	mu      sync.RWMutex
	senders map[string]ChannelSender // channelName → Sender
	logger  *zap.SugaredLogger
	tracer  trace.Tracer
}

// NewChannelReplyHandler 创建一个 ChannelReplyHandler。
func NewChannelReplyHandler(logger *zap.SugaredLogger, tp trace.TracerProvider) *ChannelReplyHandler {
	return &ChannelReplyHandler{
		senders: make(map[string]ChannelSender),
		logger:  logger.With("component", "channel_reply_handler"),
		tracer:  tp.Tracer("github.com/kasuganosora/thinkbot/agent/outbound/channel_reply"),
	}
}

// Register 注册一个 Channel Sender。
// channelName 是 Channel.Name()，sender 是实现了 Sender 接口的 Channel。
func (h *ChannelReplyHandler) Register(channelName string, sender ChannelSender) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.senders[channelName] = sender
	h.logger.Infow("channel sender registered", "channel_name", channelName)
}

// Unregister 注销一个 Channel Sender。
func (h *ChannelReplyHandler) Unregister(channelName string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.senders, channelName)
	h.logger.Infow("channel sender unregistered", "channel_name", channelName)
}

// Handle 处理一个 Action，将其路由到对应 Channel 的 Sender。
//
// 路由策略：
//  1. 优先使用 Action.Metadata["source_channel"] 作为路由键
//  2. 找到对应 Sender 后，调用 Sender.Send(ctx, action)
//  3. 找不到 Sender 时记录警告（不返回错误，避免阻塞其他 Action）
//
// 实现了 ActionHandler 接口。
func (h *ChannelReplyHandler) Handle(ctx context.Context, action core.Action) error {
	ctx, span := h.tracer.Start(ctx, "channel_reply.handle",
		trace.WithAttributes(
			attribute.String("action.type", string(action.Type)),
			attribute.String("action.channel", action.Channel),
		))
	defer span.End()

	// 提取来源 Channel 名称
	sourceChannel := h.resolveSourceChannel(action)
	if sourceChannel == "" {
		span.SetStatus(codes.Error, "no source channel")
		h.logger.Warnw("cannot route action: no source_channel in metadata",
			"action_type", action.Type,
			"action_channel", action.Channel)
		return fmt.Errorf("channel_reply: no source_channel in action metadata")
	}

	span.SetAttributes(attribute.String("source_channel", sourceChannel))

	// 查找 Sender
	h.mu.RLock()
	sender, ok := h.senders[sourceChannel]
	h.mu.RUnlock()

	if !ok {
		span.SetStatus(codes.Error, "sender not found")
		h.logger.Warnw("no sender registered for channel",
			"source_channel", sourceChannel,
			"action_type", action.Type,
			"registered_channels", h.registeredChannelNames())
		return fmt.Errorf("channel_reply: no sender for channel %q", sourceChannel)
	}

	// 执行发送
	h.logger.Debugw("routing action to channel sender",
		"source_channel", sourceChannel,
		"action_type", action.Type,
		"action_channel", action.Channel)

	if err := sender.Send(ctx, action); err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		h.logger.Errorw("channel send failed",
			"source_channel", sourceChannel,
			"action_type", action.Type,
			"action_channel", action.Channel,
			"err", err)
		return errs.Wrapf(err, "channel_reply: send via %q failed", sourceChannel)
	}

	span.SetStatus(codes.Ok, "sent")
	h.logger.Debugw("action sent via channel",
		"source_channel", sourceChannel,
		"action_type", action.Type,
		"action_channel", action.Channel)

	return nil
}

// resolveSourceChannel 从 Action 中提取来源 Channel 名称。
// 优先级：
//  1. Action.Metadata["source_channel"]（Pipeline Stage 显式设置）
func (h *ChannelReplyHandler) resolveSourceChannel(action core.Action) string {
	if action.Metadata != nil {
		if sc, ok := action.Metadata["source_channel"]; ok {
			if name, ok := sc.(string); ok && name != "" {
				return name
			}
		}
	}
	return ""
}

// registeredChannelNames 返回所有已注册的 Channel 名称（用于调试日志）。
func (h *ChannelReplyHandler) registeredChannelNames() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	names := make([]string, 0, len(h.senders))
	for name := range h.senders {
		names = append(names, name)
	}
	return names
}

// RegisteredCount 返回已注册的 Sender 数量。
func (h *ChannelReplyHandler) RegisteredCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.senders)
}
