package tools

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	noop_trace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// ToolsStage — Pipeline 工具预热 Stage（可选）
// ============================================================================

// ToolsStage 是一个可选的诊断 Stage，在 Pipeline 中提前解析工具列表用于 tracing/logging。
//
// 注意：工具注入到 LLM function calling 不依赖此 Stage。
// LLMConfig.ToolResolver 会在 LLM 调用时自动解析工具。
// 此 Stage 仅用于提前记录可用工具数量，便于在 trace 中观察。
//
// 执行位置：Order=150（在 PromptStage(200) 之前）
type ToolsStage struct {
	name    string
	manager *ToolManager
	tracer  trace.Tracer
	logger  *zap.SugaredLogger
}

// NewToolsStage 创建工具预热 Stage（可选）。
func NewToolsStage(
	name string,
	manager *ToolManager,
	tp trace.TracerProvider,
	logger *zap.SugaredLogger,
) *ToolsStage {
	if name == "" {
		name = "tools"
	}
	if tp == nil {
		tp = noop_trace.NewTracerProvider()
	}
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &ToolsStage{
		name:    name,
		manager: manager,
		tracer:  tp.Tracer("github.com/kasuganosora/thinkbot/agent/tools"),
		logger:  logger.With("component", "tools_stage"),
	}
}

// Name 返回 Stage 名称。
func (s *ToolsStage) Name() string { return s.name }

// Process 提前解析工具列表并记录到 trace（诊断用途）。
func (s *ToolsStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	ctx, span := s.tracer.Start(ctx, "stage.tools.resolve",
		trace.WithAttributes(
			attribute.String("message.id", env.Message.ID),
		))
	defer span.End()

	sctx := envelopeToSessionContext(env)

	tools, err := s.manager.ResolveTools(ctx, sctx)
	if err != nil {
		span.RecordError(err)
		s.logger.Errorw("tool resolution failed",
			"message_id", env.Message.ID,
			"err", err)
		return env, nil
	}

	span.SetAttributes(attribute.Int("tools.count", len(tools)))

	s.logger.Debugw("tools resolved",
		"message_id", env.Message.ID,
		"count", len(tools),
	)

	return env, nil
}

// ============================================================================
// Built-in Tools — 示例/常用工具
// ============================================================================

// CurrentTimeTool 返回一个获取当前时间的工具。
func CurrentTimeTool() ToolDef {
	return ToolDef{
		Category: "utility",
		Scopes:   []string{}, // 全场景
		Tool: buildTool(
			"current_time",
			"获取当前的日期和时间。当用户询问时间相关问题时使用此工具。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"timezone": map[string]any{
						"type":        "string",
						"description": "时区（IANA 格式，如 Asia/Shanghai）。可选，默认使用服务器时区。",
					},
				},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				now := time.Now()
				return map[string]any{
					"time":     now.Format(time.RFC3339),
					"unix":     now.Unix(),
					"timezone": now.Location().String(),
				}, nil
			},
		),
		PromptSection: &ToolPromptSection{
			Name:  "current_time",
			Order: 320,
			Content: `## 工具：current_time

使用 ` + "`current_time`" + ` 工具获取当前时间。
- 当用户问"现在几点"、"今天日期"等时间问题时调用
- 可以指定时区参数获取特定时区的时间`,
			Enabled: true,
		},
	}
}

// EchoTool 返回一个回显工具（主要用于测试和调试）。
func EchoTool() ToolDef {
	return ToolDef{
		Category: "utility",
		Tool: buildTool(
			"echo",
			"回显输入内容。主要用于测试工具调用是否正常工作。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message": map[string]any{
						"type":        "string",
						"description": "要回显的消息内容。",
					},
				},
				"required": []string{"message"},
			},
			func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				msg, _ := m["message"].(string)
				return map[string]any{
					"echo":   msg,
					"length": len(msg),
				}, nil
			},
		),
	}
}

// buildTool 是一个辅助函数，快速构建 llm.Tool。
func buildTool(name, description string, params map[string]any, exec func(ctx *llm.ToolExecContext, input any) (any, error)) llm.Tool {
	return llm.Tool{
		Name:        name,
		Description: description,
		Parameters:  params,
		Execute:     llm.ToolExecuteFunc(exec),
	}
}
