// Package tools 管理 LLM 可调用的工具及其提示词。
//
// 设计理念：
//   - ToolProvider 接口支持静态注册和动态提供（per-request 场景感知）
//   - 工具提示词通过 prompt.Registry 的 Section 机制统一管理
//   - ToolManager 作为统一入口，组装工具列表 + 注入提示词到 Pipeline
//
// 与 prompt 模块的关系：
//   - 每个工具可以注册一个或多个 prompt.Section（工具使用说明、约束规则等）
//   - ToolManager 在启动时将工具提示词 Section 注册到 prompt.Registry
//   - PromptStage 在组装 system prompt 时自动包含工具段落
//
// 参考 Memoh 的 ToolProvider 模式，适配 thinkbot 的架构。
package tools

import (
	"context"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// ToolProvider — 动态工具提供者
// ============================================================================

// ToolProvider 为 LLM 动态提供工具列表。
// 实现者可以根据请求上下文返回不同的工具集（场景感知）。
//
// 例如：
//   - 群聊时只返回只读工具
//   - 有特定权限时返回管理工具
//   - SubAgent 场景不返回外部联邦工具
type ToolProvider interface {
	// Tools 返回当前上下文下可用的工具列表。
	// 如果没有工具可用，返回 nil, nil。
	Tools(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error)
}

// ToolFunc 是 ToolProvider 的函数适配器，方便快速注册。
type ToolFunc func(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error)

func (f ToolFunc) Tools(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
	return f(ctx, sctx)
}

// ============================================================================
// ToolSessionContext — 工具会话上下文
// ============================================================================

// ToolSessionContext 是每次工具列表请求的上下文。
// 携带当前消息的元信息，供 ToolProvider 做场景感知决策。
type ToolSessionContext struct {
	// BotID 当前 Bot 标识。
	BotID string

	// Channel 当前消息所属会话空间。
	Channel string

	// ChatType 会话类型（private/group/...）。
	ChatType string

	// UserID 发送者 ID。
	UserID string

	// MessageID 消息 ID。
	MessageID string

	// IsSubagent 是否在 SubAgent 场景下调用。
	// SubAgent 场景通常不应返回联邦工具或记忆相关工具。
	IsSubagent bool

	// Extra 额外上下文数据（插件/Stage 注入的自定义参数）。
	Extra map[string]any
}

// GetString 从 Extra 中获取字符串值。
func (c *ToolSessionContext) GetString(key string) string {
	if c.Extra == nil {
		return ""
	}
	if v, ok := c.Extra[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// GetBool 从 Extra 中获取布尔值。
func (c *ToolSessionContext) GetBool(key string) bool {
	if c.Extra == nil {
		return false
	}
	if v, ok := c.Extra[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// ============================================================================
// ToolDef — 工具定义（带元数据）
// ============================================================================

// ToolDef 是一个工具的完整定义，包含 llm.Tool 和元数据。
type ToolDef struct {
	// Tool 是 LLM 层面的工具定义（名称、描述、参数、执行函数）。
	llm.Tool

	// Category 工具分类（如 "utility"、"search"、"memory"）。
	// 用于按类别启用/禁用工具。
	Category string

	// PromptSection 工具关联的提示词段落。
	// 如果非 nil，ToolManager 会将其注册到 prompt.Registry。
	// 通常描述该工具的使用规则、注意事项等。
	PromptSection *ToolPromptSection

	// Scopes 工具适用场景。空表示全场景可用。
	// 常见值: "private", "group", "subagent"
	Scopes []string

	// RequireApproval 是否需要审批才能执行。
	// 继承自 llm.Tool.RequireApproval，但此处更显式。
	RequireApproval bool
}

// ToolPromptSection 是工具的提示词段落定义。
type ToolPromptSection struct {
	// Name prompt.Section 的名称（唯一标识）。
	Name string

	// Order 在 prompt 组装中的排序权重。
	// 工具类段落推荐 300-399。
	Order int

	// Content 提示词内容（支持 {{.VarName}} 变量）。
	Content string

	// Enabled 是否启用。
	Enabled bool
}

// appliesTo 检查工具是否适用于给定场景。
func (d *ToolDef) appliesTo(sctx *ToolSessionContext) bool {
	if len(d.Scopes) == 0 {
		return true // 无限制
	}
	for _, scope := range d.Scopes {
		switch scope {
		case "subagent":
			if sctx.IsSubagent {
				return true
			}
		case "private":
			if sctx.ChatType == "private" && !sctx.IsSubagent {
				return true
			}
		case "group":
			if sctx.ChatType == "group" && !sctx.IsSubagent {
				return true
			}
		}
	}
	return false
}
