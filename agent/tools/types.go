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

// DynamicCategory 是动态 ToolProvider（MCP / 浏览器等）提供的工具的分类标记。
//
// 动态工具只有 llm.Tool（名称/描述/参数），没有静态 ToolDef 的 Category 元数据，
// 因此在 ListAllTools 中统一打上此分类，供 API 层映射为中文分组名。
const DynamicCategory = "dynamic"

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
// ChannelToolProvider — Channel 专属工具提供者
// ============================================================================

// ChannelToolProvider 由 Channel 实现，返回该 Channel 专属的工具定义。
// 与 ToolProvider（per-request 动态解析）不同，ChannelToolProvider 是静态的：
// BotService 在 StartBot 时遍历所有 Channel 一次性收集工具并注册到 ToolManager。
//
// 工具通过闭包捕获 Channel 的 API 客户端，因此跨 Channel 调用天然可行——
// Misskey 工具始终用 Misskey 的 API，无论从哪个 Channel 触发。
type ChannelToolProvider interface {
	// ChannelTools 返回此 Channel 提供的全部工具定义。
	// 返回 nil 或空切片表示此 Channel 无专属工具。
	ChannelTools(ctx context.Context) ([]ToolDef, error)
}

// ============================================================================
// ToolSessionContext — 工具会话上下文
// ============================================================================

// ExtraKeyChatSessionID 是ToolSessionContext.Extra 中前端会话 ID 的 key。
//
// 由 web侧在注入消息时写进 message metadata，再由 envelopeToSessionContext
// 搬进 Extra。工具需要知道「当前是哪个前端会话」时用它——例如工作流提交时
// 记录来源会话，好让前端刷新页面后能把工作流卡片恢复出来。
const ExtraKeyChatSessionID = "chat_session_id"

// CallOrigin 标识「本次工具调用来自哪个 bot 的哪个前端会话」。
//
// 为什么需要它：工具是**静态注册**的（进程/bot 启动时注册一次），而 bot 与会话是
// **每次调用才确定**的。想在工具执行时知道来源，只有两条路：
//   - 会话感知 Provider：不可行——`ToolRegistry.Resolve` 里**同名时静态工具优先**，
//     Provider 提供的同名工具会被直接丢弃；
//   - 由调用链在执行前注入 context：即本类型的做法。
//
// 两个字段都可能为空（非 web渠道没有会话概念），消费方必须容忍空值。
type CallOrigin struct {
	BotID     string
	SessionID string
}

// callOriginCtxKey 是 CallOrigin 在 context 中的 key。
type callOriginCtxKey struct{}

// ContextWithCallOrigin 把工具调用来源注入 context，供工具执行时读取。
func ContextWithCallOrigin(ctx context.Context, origin CallOrigin) context.Context {
	return context.WithValue(ctx, callOriginCtxKey{}, origin)
}

// CallOriginFromContext 从 context 取工具调用来源（没有则返回零值）。
func CallOriginFromContext(ctx context.Context) CallOrigin {
	if v, ok := ctx.Value(callOriginCtxKey{}).(CallOrigin); ok {
		return v
	}
	return CallOrigin{}
}

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

	// IsSystem 标记内部/系统会话（cron、心跳、梦境巩固等）。
	// 置为 true 时，工具权限评估（toolperm）直接放行全部工具，
	// 不受 bot_tool_permissions 约束。常规用户渠道（web/telegram/misskey）应为 false。
	IsSystem bool

	// TaskIntent 本轮任务意图，由 envelopeToSessionContext 根据本轮用户消息文本分类。
	//
	// 用于按「任务意图」而非仅「频道」收口工具——例如 Misskey 会话里的代码任务，
	// 本不应暴露 follow/unfollow/create_note 等社交写工具。此前（2026-08 实测）
	// 出现过模型在长代码任务中途丢失焦点、拿 dummy 参数乱测 misskey 写工具的事件，
	// 根因正是社交写工具 scopes:[]（全场景可见）。本字段配合 "social" scope 从架构上根绝此类跑偏。
	// 取值见 TaskIntent* 常量；空串表示未知/模糊意图（社交写工具默认隐藏）。
	TaskIntent string

	// SourceChannelType 消息来源 Channel 的类型（"telegram"/"misskey"/"web"）。
	// 由 Channel 在构建消息时注入到 Metadata["channel_type"]，
	// envelopeToSessionContext 读取后设置。供工具提供者做场景感知决策。
	SourceChannelType string

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

// ============================================================================
// ToolInfo — 工具详情快照（只读）
// ============================================================================

// ToolInfo 是某个已注册工具的只读详情快照，
// 供调试、列表展示、自省（introspection）等场景使用。
type ToolInfo struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Category         string   `json:"category"`
	Scopes           []string `json:"scopes,omitempty"`
	RequireApproval  bool     `json:"requireApproval"`
	HasPromptSection bool     `json:"hasPromptSection"`
	Parameters       any      `json:"parameters,omitempty"`

	// Risk 工具风险级别（"basic" / "sensitive"），由 API 层按 toolperm 的
	// 分级规则填充，用于权限配置界面区分「无害基础工具」与「需要管控的敏感工具」。
	// 空值表示调用方未做分级。
	Risk string `json:"risk,omitempty"`
}

// ============================================================================
// 任务意图（TaskIntent）— 按任务类型收口工具
// ============================================================================

// 任务意图常量，供 ToolDef.Scopes 中的 "social" scope 与 ToolSessionContext.TaskIntent 联动。
//
// 设计目标：社交写工具（follow/unfollow/create_note/delete_note/create_renote/react/unreact）
// 仅在「本轮任务明确是社交操作」时才暴露给模型。代码/文件/模糊意图的会话一律不暴露，
// 从架构上根绝「长任务退化导致模型拿社交工具乱测」的跑偏（2026-08 实测案例）。
const (
	// TaskIntentSocial 本轮任务明确为社交操作（关注/发帖/点赞/提及等）。
	// 设了 "social" scope 的工具仅在此意图下暴露。
	TaskIntentSocial = "social"

	// TaskIntentNonsocial 本轮任务明确为非社交（代码/文件/查询等）。
	// 社交写工具隐藏。
	TaskIntentNonsocial = "nonsocial"
	// 空串 "" 表示未知/模糊意图：社交写工具默认隐藏，
	// 仅当用户消息命中社交关键词（见 ClassifyTaskIntent）才暴露，避免误杀正常社交。
)

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
		case "social":
			// 社交写工具仅在「本轮任务意图为社交操作」时暴露。
			//
			// 根因修复：2026-08 实测，Misskey 会话里的代码任务（如「给 cfblog 加无障碍」）
			// 因社交写工具 scopes:[]（全场景可见），模型在长任务中途丢失焦点后拿
			// dummy 参数乱测 follow/unfollow。将这类工具收口到 "social" scope，
			// 由 TaskIntent 决定是否暴露，从架构上根绝跑偏。
			//
			// 放行规则：
			//   - 系统会话（心跳/cron 主动发帖）需要 → 兜底放行（心跳走 ChannelPoster 不经工具，此处仅兜底）；
			//   - 子代理（spawn / workflow 内部执行）禁止社交写操作 —— 仅执行被分配的子任务，
			//     与 wire.go 把 workflow/memory 工具在 subagent 场景排除防套娃的设计一致，
			//     同时堵住「子代理绕过 channel 拿到 misskey_create_note 照发」的已知漏洞；
			//   - 用户渠道中仅当 TaskIntent==social（用户消息命中社交关键词）才放行；
			//   - 代码任务 / 模糊意图（TaskIntent 空或 nonsocial）→ 隐藏。
			if sctx.IsSystem {
				return true
			}
			if sctx.IsSubagent {
				return false
			}
			if sctx.TaskIntent == TaskIntentSocial {
				return true
			}
			return false
		}
	}
	return false
}
