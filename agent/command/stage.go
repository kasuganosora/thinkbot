package command

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// CommandStage — Pipeline 命令拦截 Stage
// ============================================================================

// CommandStage 在 LLM 执行之前拦截以 / 开头的命令消息。
// 如果消息不是命令，正常放行给后续 Stage 处理。
//
// 当消息是命令时：
//  1. 从 Registry 中查找对应的 CommandHandler
//  2. 检查管理员权限（如果命令 AdminOnly）
//  3. 执行处理器，将回复添加为 ActionReply
//  4. 调用 env.Abort(nil) 中止 Pipeline（跳过 LLM 等 Stage），但保留 Action 供 Dispatcher 派发
//
// CommandStage 通常放在 Pipeline 最前面（如 Order=5），
// 在所有其他 Stage 之前执行，确保命令不需要任何 LLM 或 session 处理开销。
type CommandStage struct {
	name     string
	registry *Registry
	checker  AdminChecker
	tracer   trace.Tracer
	logger   *zap.SugaredLogger
}

// NewCommandStage 创建命令拦截 Stage。
//
// checker 为 nil 时，所有 AdminOnly 命令都会被拒绝（安全默认）。
func NewCommandStage(
	name string,
	registry *Registry,
	checker AdminChecker,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *CommandStage {
	if name == "" {
		name = "command"
	}
	if registry == nil {
		registry = NewRegistry()
	}
	return &CommandStage{
		name:     name,
		registry: registry,
		checker:  checker,
		tracer:   tp.Tracer("github.com/kasuganosora/thinkbot/agent/command"),
		logger:   logger.With("component", "command_stage"),
	}
}

// Name 返回 Stage 名称。
func (s *CommandStage) Name() string { return s.name }

// Registry 返回命令注册表（便于外部注册自定义命令）。
func (s *CommandStage) Registry() *Registry { return s.registry }

// Process 执行命令拦截逻辑。
func (s *CommandStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	parsed := Parse(env.Message.Text)
	if parsed == nil {
		// 不是命令，正常放行
		return env, nil
	}

	ctx, span := s.tracer.Start(ctx, "stage.command.process",
		trace.WithAttributes(
			attribute.String("command.name", parsed.Name),
			attribute.String("command.args", parsed.Args),
			attribute.String("message.id", env.Message.ID),
			attribute.String("trace.id", traceid.FromContext(ctx)),
		))
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, s.logger)

	// 查找命令处理器
	handler, ok := s.registry.Lookup(parsed.Name)
	if !ok {
		// 未知命令：放行（让 LLM 自然处理）
		logger.Debugw("command not found, passing through",
			"command", parsed.Name,
			"message_id", env.Message.ID)
		return env, nil
	}

	span.SetAttributes(
		attribute.String("command.handler", handler.Name()),
		attribute.Bool("command.admin_only", handler.AdminOnly()),
	)

	// 管理员权限检查
	if handler.AdminOnly() {
		if !s.isAdmin(ctx, env.Message.Source, env.Message.UserID) {
			span.SetAttributes(attribute.Bool("command.denied", true))
			logger.Infow("command denied: not admin",
				"command", parsed.Name,
				"message_id", env.Message.ID,
				"user_id", env.Message.UserID)
			s.reply(env, fmt.Sprintf("⚠️ 命令 /%s 需要管理员权限。", parsed.Name))
			env.Abort(nil)
			return env, nil
		}
	}

	// 执行命令
	result, err := handler.Execute(ctx, env, parsed.Args)
	if err != nil {
		span.RecordError(err)
		logger.Errorw("command execution failed",
			"command", parsed.Name,
			"message_id", env.Message.ID,
			"err", err)
		s.reply(env, fmt.Sprintf("❌ 命令 /%s 执行失败: %v", parsed.Name, err))
		env.Abort(nil)
		return env, nil
	}

	if result != nil && result.Reply != "" {
		s.reply(env, result.Reply)
	}

	span.SetAttributes(attribute.Bool("command.executed", true))

	logger.Infow("command executed",
		"command", parsed.Name,
		"message_id", env.Message.ID,
		"user_id", env.Message.UserID,
		"ok", result != nil && result.OK)

	// 中止 Pipeline（跳过后续 Stage），但保留 Action 供 Dispatcher 派发
	env.Abort(nil)
	return env, nil
}

// isAdmin 检查当前用户是否为管理员。
func (s *CommandStage) isAdmin(ctx context.Context, source, userID string) bool {
	if s.checker == nil {
		return false
	}
	return s.checker.IsAdmin(ctx, source, userID)
}

// reply 添加回复 Action 到 Envelope。
func (s *CommandStage) reply(env *core.Envelope, text string) {
	replyTarget := env.Message.Channel
	if env.Message.Metadata != nil {
		if rt, ok := env.Message.Metadata["reply_target"]; ok {
			if str, ok := rt.(string); ok && str != "" {
				replyTarget = str
			}
		}
	}

	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: replyTarget,
		UserID:  env.Message.UserID,
		Payload: text,
		Metadata: map[string]any{
			"source_channel": env.Message.Source,
			"trace_id":       env.Message.TraceID,
			"command":        true,
		},
	})
}
