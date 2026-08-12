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
