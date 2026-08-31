package stages

import (
	"context"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// LoggerStage — 结构化日志 Stage
// ============================================================================

// LoggerStage 记录每条消息的关键信息，用于审计和调试。
type LoggerStage struct {
	name   string
	logger *zap.SugaredLogger
	// LogPayload 是否记录消息文本（生产环境可能需要关闭）。
	LogPayload bool
}

// NewLoggerStage 创建日志 Stage。
func NewLoggerStage(name string, logger *zap.SugaredLogger, logPayload bool) *LoggerStage {
	if name == "" {
		name = "logger"
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &LoggerStage{
		name:       name,
		logger:     logger,
		LogPayload: logPayload,
	}
}

// Name 返回 Stage 名称。
func (s *LoggerStage) Name() string { return s.name }

// Process 记录消息信息。
func (s *LoggerStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	fields := []any{
		"message_id", env.Message.ID,
		"source", env.Message.Source,
		"channel", env.Message.Channel,
		"user_id", env.Message.UserID,
		"media_type", env.Message.MediaType,
	}

	if s.LogPayload && env.Message.Text != "" {
		// 使用 rune 安全的截断，防止切断多字节 UTF-8 字符
		var text string
		if utf8.RuneCountInString(env.Message.Text) > 500 {
			text = strutil.Truncate(env.Message.Text, 500) + "...(truncated)"
		} else {
			text = env.Message.Text
		}
		fields = append(fields, "text", text)
	}

	if len(env.Message.Metadata) > 0 {
		fields = append(fields, "metadata_keys", metadataKeys(env.Message.Metadata))
	}

	logger := traceid.WithLoggerFrom(ctx, s.logger)
	logger.Infow("message received", fields...)

	return env, nil
}

// metadataKeys 提取 metadata 的 key 列表（避免日志中泄露 value）。
func metadataKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
