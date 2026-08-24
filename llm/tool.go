package llm

import "context"

// ctxKeyDirectReply 标记「当前消息是否处于直接回复语境」（对方 @ 了 Bot 或回复了 Bot）。
// 由 llmroute 在编排前注入，供 Channel 工具（如 Misskey 的发布工具）判断：
// 在直接回复语境下应改用「文本回复」（框架会自动串接回复），而非手动发孤立帖，避免重复发文。
type ctxKeyDirectReply struct{}

// WithDirectReply 把「是否直接回复语境」注入 ctx。
func WithDirectReply(ctx context.Context, v bool) context.Context {
	return context.WithValue(ctx, ctxKeyDirectReply{}, v)
}

// IsDirectReply 读取「是否直接回复语境」。未设置时返回 false。
func IsDirectReply(ctx context.Context) bool {
	v, ok := ctx.Value(ctxKeyDirectReply{}).(bool)
	return ok && v
}

// ctxKeyInboundReply 标记「本轮由某个入站帖/消息驱动、框架会自动串接回复」。
//
// 与 ctxKeyDirectReply 的区别：DirectReply 只在「被 @ / 被回复」时为真，
// 但框架对**任何**有回复目标的入站帖都会自动串接回复（社交时间线上未 @ Bot 的
// 普通帖同样如此）。此时若 Channel 工具再手动发一条新帖，同一条入站帖就会收到
// 两条发文（典型表现：正文手动加 "@某人" 前缀假装回复，实际是条孤立帖）。
// 因此需要一个比 DirectReply 更宽的判据。
type ctxKeyInboundReply struct{}

// InboundReply 描述「本轮由哪个入站渠道的消息驱动」。
// Source 为 core.Message.Source（渠道实例名），Channel 工具用它确认
// 该入站消息是否来自**自己所属的渠道**——跨渠道场景（如在 Web 会话里
// 指示 Bot 去 Misskey 发帖）不应被拦截。
type InboundReply struct {
	// Source 入站渠道实例名。
	Source string
	// HasReplyTarget 框架是否持有该消息的回复目标（能串接回复）。
	HasReplyTarget bool
}

// WithInboundReply 把「入站回复语境」注入 ctx。
func WithInboundReply(ctx context.Context, v InboundReply) context.Context {
	return context.WithValue(ctx, ctxKeyInboundReply{}, v)
}

// InboundReplyFrom 读取「入站回复语境」。未设置时返回零值 + false。
func InboundReplyFrom(ctx context.Context) (InboundReply, bool) {
	v, ok := ctx.Value(ctxKeyInboundReply{}).(InboundReply)
	return v, ok
}

// IsFrameworkReplyContext 判断本轮是否「由 source 渠道的入站消息驱动、且框架会串接回复」。
// source 传入调用方渠道自身的实例名；不匹配（或未注入）时返回 false，
// 保证跨渠道主动发帖能力不被误伤。
func IsFrameworkReplyContext(ctx context.Context, source string) bool {
	if source == "" {
		return false
	}
	v, ok := InboundReplyFrom(ctx)
	return ok && v.HasReplyTarget && v.Source == source
}

// ToolExecuteFunc is the signature for a tool's execution handler.
// input is the parsed arguments from the LLM. The return value becomes the
// tool result output sent back to the model.
type ToolExecuteFunc func(ctx *ToolExecContext, input any) (any, error)

// ToolExecContext is passed to ToolExecuteFunc and carries the parent context,
// call metadata, and a mechanism for streaming progress updates.
type ToolExecContext struct {
	context.Context
	ToolCallID   string
	ToolName     string
	InvocationID string            // 服务端生成的本次执行唯一标识，用于日志追踪/前端区分
	SendProgress func(content any) // nil when not in streaming mode

	// UserRequest 触发本轮编排的用户请求文本（或子代理任务描述）。
	// 供标记了 RequiresUserIntent 的写工具做「是否根植于用户显式意图」的护栏判定。
	UserRequest string
}

// ToolApprovalDecision controls how a tool call requiring approval is handled.
type ToolApprovalDecision string

const (
	ToolApprovalApproved ToolApprovalDecision = "approved"
	ToolApprovalRejected ToolApprovalDecision = "rejected"
	ToolApprovalDeferred ToolApprovalDecision = "deferred"
)

// ToolApprovalResult holds the outcome of a tool approval check.
type ToolApprovalResult struct {
	Decision   ToolApprovalDecision `json:"decision"`
	ApprovalID string               `json:"approvalId,omitempty"`
	Reason     string               `json:"reason,omitempty"`
	Metadata   map[string]any       `json:"metadata,omitempty"`

	// 以下字段由编排层在 defer 时自动补全，用于 HITL 续跑锚点（按工具名预批准）：
	ToolName   string `json:"toolName,omitempty"`   // 触发审批的工具名
	ToolCallID string `json:"toolCallId,omitempty"` // 本次工具调用 ID
	Input      any    `json:"-"`                    // 工具入参（不序列化，仅用于续跑重建）
}

// Tool describes a function tool that the model can call.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters is a JSON Schema (map[string]any or *jsonschema.Schema) describing
	// the tool's input.  Providers will serialize it appropriately.
	Parameters   any           `json:"parameters,omitempty"`
	CacheControl *CacheControl `json:"-"`

	// Execute, when non-nil, allows the orchestration layer to automatically
	// run this tool and feed the result back to the model in a multi-step loop.
	Execute ToolExecuteFunc `json:"-"`

	// RequireApproval, when true, causes the orchestration layer to call the
	// configured ApprovalHandler before executing this tool.
	RequireApproval bool `json:"-"`

	// RequiresUserIntent, when true, marks this tool as a *user-intent-gated
	// write action* (e.g. Misskey follow/unfollow/post/react). It is only
	// executed when the triggering user request (cfg.UserRequest) explicitly
	// references that social action; otherwise the orchestration layer refuses
	// to run it *before* execution. This blocks the model from spontaneously
	// issuing external side-effects mid-unrelated-task (a 2026-08 incident had
	// the model derail during a code task and spam Misskey write tools), while
	// still allowing legitimate, user-requested social calls from any channel.
	RequiresUserIntent bool `json:"-"`

	// DeferredLoad marks this tool as lazily loaded (Claude-style defer_loading).
	// When true, the orchestration layer initially shows the model only the
	// tool's name and description — its Parameters (input schema) are hidden —
	// until the tool is "loaded" on demand. Loading happens either via the
	// injected tool_search tool, or automatically when the model references the
	// tool by name. Once loaded, the full schema is included so the model can
	// call it with arguments.
	DeferredLoad bool `json:"-"`

	// Keywords are extra terms used by tool_search to match this tool. Optional.
	Keywords []string `json:"-"`
}

// ToolCall represents a tool invocation requested by the model.
type ToolCall struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Input      any    `json:"input"`
}

// ToolResult holds the output of a tool execution.
type ToolResult struct {
	ToolCallID   string `json:"toolCallId"`
	ToolName     string `json:"toolName"`
	InvocationID string `json:"invocationId,omitempty"`
	Output       any    `json:"output"`
}
