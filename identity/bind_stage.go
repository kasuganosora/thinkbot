package identity

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// BindStage — 授权码绑定拦截 Stage
//
// 在 Pipeline 最前端（Order=3）拦截授权码格式的消息，
// 自动完成跨平台身份绑定。
//
// 当消息匹配 TB-XXXX-XXXX 格式时：
//  1. 消费授权码（验证有效性 + 创建身份映射）
//  2. 回复绑定结果
//  3. 中止 Pipeline（不发给 LLM）
//
// 不匹配时正常放行。
// ============================================================================

// BindStage 在 Pipeline 中拦截授权码消息。
type BindStage struct {
	bindSvc *BindService
	tracer  trace.Tracer
	logger  *zap.SugaredLogger
}

// NewBindStage 创建授权码绑定拦截 Stage。
func NewBindStage(bindSvc *BindService, tp trace.TracerProvider, logger *zap.SugaredLogger) *BindStage {
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	return &BindStage{
		bindSvc: bindSvc,
		tracer:  tp.Tracer("github.com/kasuganosora/thinkbot/identity"),
		logger:  logger.With("component", "bind_stage"),
	}
}

// Name 返回 Stage 名称。
func (s *BindStage) Name() string { return "bind" }

// Process 执行授权码拦截逻辑。
func (s *BindStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	if s.bindSvc == nil {
		return env, nil
	}

	code := NormalizeCode(env.Message.Text)
	if code == "" {
		// 不是授权码，正常放行
		return env, nil
	}

	ctx, span := s.tracer.Start(ctx, "stage.bind.process",
		trace.WithAttributes(
			attribute.String("bind.code", code),
			attribute.String("bind.source", env.Message.Source),
			attribute.String("bind.platform_user_id", env.Message.UserID),
			attribute.String("trace.id", traceid.FromContext(ctx)),
		))
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, s.logger)

	platform := extractPlatform(env.Message.Source)
	result, err := s.bindSvc.ConsumeCode(ctx, code, platform, env.Message.UserID)
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.Bool("bind.success", false))
		reply := formatBindError(err)
		logger.Infow("bind code consumption failed",
			"code", code,
			"platform", platform,
			"platform_user_id", env.Message.UserID,
			"err", err)
		s.reply(env, reply)
		env.Abort(nil)
		return env, nil
	}

	span.SetAttributes(
		attribute.Bool("bind.success", true),
		attribute.String("bind.username", result.Username),
	)

	logger.Infow("bind code consumed successfully",
		"code", code,
		"platform", platform,
		"platform_user_id", env.Message.UserID,
		"internal_user", result.Username)

	s.reply(env, fmt.Sprintf(
		"✅ 绑定成功！\n\n平台: %s\n账号: %s\n已绑定用户: %s\n\n你现在可以使用管理员命令了（如 /clear、/compact）。",
		platform, env.Message.UserID, result.Username,
	))

	// 中止 Pipeline，不发给 LLM
	env.Abort(nil)
	return env, nil
}

// reply 添加回复 Action 到 Envelope。
func (s *BindStage) reply(env *core.Envelope, text string) {
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
			"bind":           true,
		},
	})
}

// formatBindError 将绑定错误格式化为用户友好的消息。
func formatBindError(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "not found"):
		return "❌ 授权码不存在，请检查输入或重新生成。"
	case strings.Contains(s, "expired"):
		return "❌ 授权码已过期（有效期为 5 分钟），请重新生成。"
	case strings.Contains(s, "already used"):
		return "❌ 授权码已被使用，每个码只能使用一次。请重新生成。"
	case strings.Contains(s, "already bound"):
		return "❌ 该平台账号已绑定其他用户。如需重新绑定，请先在 Web 页面解绑。"
	default:
		return fmt.Sprintf("❌ 绑定失败: %v", err)
	}
}
