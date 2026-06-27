package llm

// ============================================================================
// PatchToolCalls — Dangling tool-call repair
//
// Scans message sequences for assistant messages containing ToolCallParts
// that lack a corresponding ToolResultPart, and inserts placeholder
// ToolMessages for any missing responses.
//
// This prevents LLM providers (especially Anthropic) from rejecting requests
// due to incomplete tool_call / tool_result message sequences — a common
// failure when tool execution is interrupted, times out, or errors mid-step.
//
// Inspired by cloudwego/eino's patchtoolcalls middleware.
// ============================================================================

// DefaultPatchPlaceholder is the content inserted for missing tool responses.
const DefaultPatchPlaceholder = "Tool execution was interrupted or skipped."

// PatchToolCalls scans the message sequence and inserts placeholder
// ToolMessages for any tool calls that lack a corresponding response.
//
// The function is idempotent: if all tool calls already have responses,
// the input slice is returned unchanged (same reference).
func PatchToolCalls(messages []Message) []Message {
	return PatchToolCallsWith(messages, DefaultPatchPlaceholder)
}

// PatchToolCallsWith is like PatchToolCalls but allows customizing the
// placeholder message content for missing tool responses.
//
// Algorithm:
//  1. Single pass to collect all ToolCallIDs that have a ToolResultPart.
//  2. Walk messages: for each assistant message with dangling tool calls,
//     skip past consecutive tool messages, then insert a placeholder
//     tool message covering all missing IDs.
func PatchToolCallsWith(messages []Message, placeholder string) []Message {
	if len(messages) == 0 {
		return messages
	}

	// Phase 1: collect all responded tool-call IDs.
	responded := make(map[string]bool)
	for _, msg := range messages {
		if msg.Role != MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if tr, ok := part.(ToolResultPart); ok {
				responded[tr.ToolCallID] = true
			}
		}
	}

	// Quick check: is any patching needed?
	needPatch := false
	for _, msg := range messages {
		if msg.Role != MessageRoleAssistant {
			continue
		}
		for _, part := range msg.Content {
			if tc, ok := part.(ToolCallPart); ok && !responded[tc.ToolCallID] {
				needPatch = true
				break
			}
		}
		if needPatch {
			break
		}
	}
	if !needPatch {
		return messages
	}

	// Phase 2: build patched output, inserting placeholders for missing calls.
	out := make([]Message, 0, len(messages)+4)

	for i := 0; i < len(messages); i++ {
		out = append(out, messages[i])

		if messages[i].Role != MessageRoleAssistant {
			continue
		}

		// Collect dangling tool calls from this assistant message.
		var missing []ToolCallPart
		for _, part := range messages[i].Content {
			if tc, ok := part.(ToolCallPart); ok && !responded[tc.ToolCallID] {
				missing = append(missing, tc)
			}
		}
		if len(missing) == 0 {
			continue
		}

		// Advance past consecutive tool messages that follow this assistant
		// message — the placeholder goes after all existing responses.
		for i+1 < len(messages) && messages[i+1].Role == MessageRoleTool {
			i++
			out = append(out, messages[i])
		}

		// Insert placeholder tool message for the missing responses.
		placeholders := make([]ToolResultPart, len(missing))
		for j, tc := range missing {
			placeholders[j] = ToolResultPart{
				ToolCallID: tc.ToolCallID,
				ToolName:   tc.ToolName,
				Result:     placeholder,
			}
		}
		out = append(out, ToolMessage(placeholders...))
	}

	return out
}
