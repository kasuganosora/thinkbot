package llm

import "context"

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
}

// ToolCall represents a tool invocation requested by the model.
type ToolCall struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Input      any    `json:"input"`
}

// ToolResult holds the output of a tool execution.
type ToolResult struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Output     any    `json:"output"`
}
