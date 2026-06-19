package stages

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// MultimodalStage — 多模态附件转写
//
// Pipeline 位置：Order=30（在 Filter(20) 之后、Engagement(40) 之前）。
//
// 工作原理：
//  1. 检查消息是否包含多模态附件（image/audio/video）
//  2. 如果主力模型支持多模态 → 跳过（主 LLM 能直接处理）
//  3. 如果主力模型不支持多模态 && 配置了 Vision 辅助模型 → 调用辅助模型转写
//  4. 将转写结果追加到 Message.Text，使主 LLM 能感知附件内容
//
// 触发条件（全部满足才转写）：
//   - 消息有多模态附件（core.HasMultimodalAttachments）
//   - 主力模型不支持多模态（mainMultimodal=false）
//   - visionProvider 非 nil
// ============================================================================

// DefaultMultimodalPrompt 是调用辅助模型时的默认系统提示词。
const DefaultMultimodalPrompt = `你是一个多模态内容描述助手。请用简洁准确的中文描述用户提供的图片、音频或视频内容。要求：
- 图片：描述画面内容、文字、场景、人物等关键信息
- 音频：描述语音内容、音乐风格、环境声等
- 视频：描述画面、动作、对话等关键信息
- 直接输出描述，不要添加"这是一张图片"之类的元描述
- 如果内容不清晰或无法识别，说明情况`

// MultimodalConfig 配置 MultimodalStage。
type MultimodalConfig struct {
	// VisionProvider 多模态辅助 LLM Provider（必须支持图片/音频输入）。
	VisionProvider llm.Provider

	// VisionModel 辅助模型（如 ChatModel("gpt-4o")）。
	VisionModel *llm.Model

	// MainMultimodal 主力模型是否支持多模态。
	// 为 true 时跳过转写（主 LLM 直接处理多模态内容）。
	MainMultimodal bool

	// SystemPrompt 调用辅助模型时的系统提示词。
	// 为空时使用 DefaultMultimodalPrompt。
	SystemPrompt string

	// MaxTokens 辅助模型最大输出 token。
	// 为 nil 时使用 1024。
	MaxTokens *int

	// Temperature 辅助模型温度。
	// 为 nil 时使用 0.3。
	Temperature *float64
}

// MultimodalStage 是多模态附件转写 Stage。
type MultimodalStage struct {
	name   string
	config MultimodalConfig
	tracer trace.Tracer
	logger *zap.SugaredLogger
}

// NewMultimodalStage 创建 MultimodalStage。
func NewMultimodalStage(name string, config MultimodalConfig, tp trace.TracerProvider, logger *zap.SugaredLogger) *MultimodalStage {
	if name == "" {
		name = "multimodal"
	}
	if config.SystemPrompt == "" {
		config.SystemPrompt = DefaultMultimodalPrompt
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &MultimodalStage{
		name:   name,
		config: config,
		tracer: tp.Tracer("github.com/kasuganosora/thinkbot/agent/stages/multimodal"),
		logger: logger.With("component", "multimodal_stage"),
	}
}

// Name 返回 Stage 名称。
func (s *MultimodalStage) Name() string { return s.name }

// ShouldProcess 检查消息是否需要多模态转写。
// 返回 true 的条件：有附件 && 主模型不支持多模态 && 有辅助模型。
func (s *MultimodalStage) ShouldProcess(msg *core.Message) bool {
	if s.config.MainMultimodal {
		return false // 主模型已支持多模态，无需转写
	}
	if s.config.VisionProvider == nil {
		return false // 未配置辅助模型
	}
	return core.HasMultimodalAttachments(msg)
}

// Process 处理消息。
// 如果消息包含多模态附件且需要转写，调用辅助模型生成描述并追加到消息文本。
func (s *MultimodalStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	if !s.ShouldProcess(&env.Message) {
		return env, nil
	}

	ctx, span := s.tracer.Start(ctx, "stage.multimodal.transcribe",
		trace.WithAttributes(
			attribute.String("message.id", env.Message.ID),
			attribute.String("trace.id", traceid.FromContext(ctx)),
		))
	defer span.End()

	logger := traceid.WithLoggerFrom(ctx, s.logger)

	attachments := core.GetAttachments(&env.Message)
	logger.Debugw("multimodal stage: processing attachments",
		"message_id", env.Message.ID,
		"attachment_count", len(attachments))

	// 逐个转写多模态附件
	var descriptions []string
	transcribedCount := 0
	for i, att := range attachments {
		if !core.IsMultimodalType(att.Type) {
			continue
		}

		desc, err := s.transcribeAttachment(ctx, att, env.Message.Text)
		if err != nil {
			logger.Warnw("multimodal stage: failed to transcribe attachment",
				"message_id", env.Message.ID,
				"attachment_index", i,
				"attachment_type", att.Type,
				"err", err)
			span.RecordError(err)
			continue
		}

		label := att.Type
		if att.Filename != "" {
			label = fmt.Sprintf("%s (%s)", att.Type, att.Filename)
		}
		descriptions = append(descriptions, fmt.Sprintf("[%s 内容描述] %s", label, desc))
		transcribedCount++
	}

	if transcribedCount == 0 {
		logger.Debugw("multimodal stage: no attachments transcribed",
			"message_id", env.Message.ID)
		return env, nil
	}

	// 将描述追加到消息文本
	originalText := env.Message.Text
	descriptionBlock := strings.Join(descriptions, "\n")

	if originalText != "" {
		env.Message.Text = originalText + "\n\n" + descriptionBlock
	} else {
		env.Message.Text = descriptionBlock
	}

	span.SetAttributes(
		attribute.Int("multimodal.transcribed_count", transcribedCount),
		attribute.Int("multimodal.description_length", len(descriptionBlock)),
	)

	logger.Infow("multimodal stage: transcribed",
		"message_id", env.Message.ID,
		"transcribed_count", transcribedCount,
		"description_length", len(descriptionBlock))

	return env, nil
}

// transcribeAttachment 调用辅助模型转写单个附件。
func (s *MultimodalStage) transcribeAttachment(ctx context.Context, att core.Attachment, userText string) (string, error) {
	dataURI := att.DataURI()
	if dataURI == "" {
		return "", fmt.Errorf("attachment has no URL or Data")
	}

	// 构建多模态消息
	var parts []llm.MessagePart

	// 图片用 ImagePart
	if core.IsImageType(att.Type) {
		parts = append(parts, llm.ImagePart{
			Image:     dataURI,
			MediaType: att.MimeType,
		})
	} else {
		// 音频/视频用 FilePart（部分 Provider 支持）
		parts = append(parts, llm.FilePart{
			Data:      dataURI,
			MediaType: att.MimeType,
			Filename:  att.Filename,
		})
	}

	// 用户原始文本作为上下文
	contextText := userText
	if contextText == "" {
		contextText = "请描述这个内容"
	}
	parts = append(parts, llm.TextPart{Text: contextText})

	msg := llm.Message{
		Role:    llm.MessageRoleUser,
		Content: parts,
	}

	temp := 0.3
	if s.config.Temperature != nil {
		temp = *s.config.Temperature
	}
	maxTokens := 1024
	if s.config.MaxTokens != nil {
		maxTokens = *s.config.MaxTokens
	}

	params := llm.GenerateParams{
		Model:       s.config.VisionModel,
		System:      s.config.SystemPrompt,
		Messages:    []llm.Message{msg},
		Temperature: &temp,
		MaxTokens:   &maxTokens,
	}

	result, err := s.config.VisionProvider.DoGenerate(ctx, params)
	if err != nil {
		return "", fmt.Errorf("vision provider generate: %w", err)
	}

	return result.Text, nil
}
