package stages

import (
	"context"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// FilterStage — 消息过滤 Stage
// ============================================================================

// FilterAction 过滤器动作。
type FilterAction string

const (
	// FilterPass 匹配时放行（不匹配时丢弃）。
	FilterPass FilterAction = "pass"
	// FilterDrop 匹配时丢弃（不匹配时放行）。
	FilterDrop FilterAction = "drop"
)

// FilterStage 根据 Predicate 过滤消息。
type FilterStage struct {
	name      string
	predicate core.Predicate
	action    FilterAction
	logger    *zap.SugaredLogger
}

// NewFilterStage 创建过滤 Stage。
//
// action 语义：
//   - FilterPass: predicate 匹配 → 放行，不匹配 → 丢弃（返回 nil）
//   - FilterDrop: predicate 匹配 → 丢弃（返回 nil），不匹配 → 放行
func NewFilterStage(name string, predicate core.Predicate, action FilterAction, logger *zap.SugaredLogger) *FilterStage {
	if name == "" {
		name = "filter"
	}
	return &FilterStage{
		name:      name,
		predicate: predicate,
		action:    action,
		logger:    logger,
	}
}

// Name 返回 Stage 名称。
func (s *FilterStage) Name() string { return s.name }

// Process 执行过滤逻辑。
func (s *FilterStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	matched := s.predicate.Match(env)
	logger := traceid.WithLoggerFrom(ctx, s.logger)

	switch s.action {
	case FilterPass:
		if !matched {
			logger.Debugw("filter: message dropped (no match)",
				"filter", s.name,
				"message_id", env.Message.ID)
			return nil, nil // 丢弃
		}
	case FilterDrop:
		if matched {
			logger.Debugw("filter: message dropped (matched drop rule)",
				"filter", s.name,
				"message_id", env.Message.ID)
			return nil, nil // 丢弃
		}
	}

	return env, nil
}
