package stages

import (
	"context"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// ============================================================================
// EnricherStage — 消息富化 Stage
// ============================================================================

// EnrichFunc 富化函数签名。
// 接收 context 和 envelope，可以向 envelope 添加元数据或 KV 值。
type EnrichFunc func(ctx context.Context, env *core.Envelope) error

// EnricherStage 为消息附加额外信息（用户画像、会话上下文、权限标记等）。
type EnricherStage struct {
	name     string
	enrichFn EnrichFunc
	logger   *zap.SugaredLogger
}

// NewEnricherStage 创建富化 Stage。
func NewEnricherStage(name string, fn EnrichFunc, logger *zap.SugaredLogger) *EnricherStage {
	if name == "" {
		name = "enricher"
	}
	return &EnricherStage{
		name:     name,
		enrichFn: fn,
		logger:   logger,
	}
}

// Name 返回 Stage 名称。
func (s *EnricherStage) Name() string { return s.name }

// Process 执行富化逻辑。
func (s *EnricherStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	if err := s.enrichFn(ctx, env); err != nil {
		s.logger.Warnw("enricher failed",
			"enricher", s.name,
			"message_id", env.Message.ID,
			"err", err)
		return env, &core.PipelineError{
			Stage:   s.name,
			Message: "enrichment failed",
			Cause:   err,
		}
	}
	return env, nil
}
