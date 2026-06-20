package tools

import (
	"context"
	"time"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// now 工具 — 获取当前时间（per-bot 时区）
// ============================================================================

// nowToolProvider 是一个动态 ToolProvider，为每个会话提供带正确时区的 now 工具。
type nowToolProvider struct {
	resolveTimezone func(botID string) string
}

func (p *nowToolProvider) Tools(ctx context.Context, sctx *agenttools.ToolSessionContext) ([]llm.Tool, error) {
	tz := "UTC"
	if p.resolveTimezone != nil && sctx != nil && sctx.BotID != "" {
		if t := p.resolveTimezone(sctx.BotID); t != "" {
			tz = t
		}
	}
	return []llm.Tool{buildNowTool(tz)}, nil
}

// buildNowTool 创建带固定时区的 now 工具。
func buildNowTool(timezone string) llm.Tool {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
		timezone = "UTC"
	}

	return llm.Tool{
		Name: "now",
		Description: "获取当前日期和时间。返回本地时间、UTC 时间、时区、星期几等信息。" +
			"当用户询问当前时间、日期相关问题时使用此工具。",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			now := time.Now().In(loc)
			return map[string]any{
				"datetime":   now.Format("2006-01-02 15:04:05"),
				"date":       now.Format("2006-01-02"),
				"time":       now.Format("15:04:05"),
				"weekday":    now.Weekday().String(),
				"timezone":   timezone,
				"utc":        now.UTC().Format("2006-01-02T15:04:05Z"),
				"unix":       now.Unix(),
				"iso8601":    now.Format(time.RFC3339),
				"isWeekend":  now.Weekday() == time.Saturday || now.Weekday() == time.Sunday,
			}, nil
		}),
	}
}

// ============================================================================
// 辅助：解析时区（用于测试和其他模块）
// ============================================================================

// ParseTimezone 将 IANA 时区字符串解析为 *time.Location。
// 解析失败时返回 time.UTC。
func ParseTimezone(tz string) *time.Location {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}
