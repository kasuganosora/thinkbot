package llm

import (
	"fmt"
)

// ============================================================================
// Reduction — Lightweight in-loop context compression
//
// Two-phase safety net that operates WITHIN the orchestration loop to prevent
// context window overflow during multi-step tool execution:
//
//  Phase 1 (TruncateToolResults): After tools execute, individual results
//  exceeding the token threshold are replaced with a short summary. This is
//  complementary to the existing TruncateOutput (which works at byte/line level
//  in runTool) — Reduction works at token level with configurable thresholds.
//
//  Phase 2 (ReduceHistory): Before each LLM call, if total message tokens
//  exceed the clear threshold, older tool results are replaced with compact
//  placeholders. Unlike the heavyweight Compactor (which uses LLM-based
//  summarization for cross-conversation compression), this is a zero-cost
//  prune-only operation suitable for in-loop use.
//
// Inspired by cloudwego/eino's reduction middleware.
// ============================================================================

// ReductionConfig controls in-loop context compression.
type ReductionConfig struct {
	// MaxOutputTokens is the Phase 1 threshold: individual tool results
	// whose estimated token count exceeds this value are replaced with
	// a short summary. Set to 0 to disable Phase 1.
	// Default: 7500 tokens (~30K chars).
	MaxOutputTokens int

	// ClearThresholdTokens is the Phase 2 threshold: when total message
	// tokens exceed this value, older tool results are compacted.
	// Set to 0 to disable Phase 2.
	// Default: 100000 tokens.
	ClearThresholdTokens int

	// RetainRecentSteps is the number of recent orchestration steps
	// (assistant+tool message pairs) preserved during Phase 2 compaction.
	// Default: 4 steps.
	RetainRecentSteps int

	// ExcludeTools lists tool names whose results are never truncated
	// or compacted (e.g., tools returning structured data that would
	// lose meaning when summarized).
	ExcludeTools []string
}

// DefaultReductionConfig returns sensible defaults for in-loop compression.
// These thresholds are intentionally higher than the existing Compactor's
// defaults — Reduction is a safety net, not the primary compression mechanism.
func DefaultReductionConfig() ReductionConfig {
	return ReductionConfig{
		MaxOutputTokens:      7500,
		ClearThresholdTokens: 100000,
		RetainRecentSteps:    4,
	}
}

// DefaultReductionConfigPtr returns a pointer to DefaultReductionConfig.
// Convenience for struct literal initialization (e.g., LLMConfig).
func DefaultReductionConfigPtr() *ReductionConfig {
	rc := DefaultReductionConfig()
	return &rc
}

// excludeSet builds a lookup map from ExcludeTools.
func (c ReductionConfig) excludeSet() map[string]bool {
	if len(c.ExcludeTools) == 0 {
		return nil
	}
	m := make(map[string]bool, len(c.ExcludeTools))
	for _, name := range c.ExcludeTools {
		m[name] = true
	}
	return m
}

// --- Helpers ---

// resultToString converts a tool result value to its string representation.
func resultToString(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	case error:
		return val.Error()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// previewString returns at most maxChars runes of text, with "..." suffix
// if the text was truncated.
func previewString(text string, maxChars int) string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars]) + "..."
}

// --- Phase 1: Per-result truncation ---

// TruncateToolResults applies Phase 1 truncation to tool results.
// Results whose estimated token count exceeds MaxOutputTokens are replaced
// with a short preview + summary indicating the original size.
//
// If MaxOutputTokens is 0 or negative, results are returned unchanged.
func TruncateToolResults(results []ToolResultPart, cfg ReductionConfig) []ToolResultPart {
	if cfg.MaxOutputTokens <= 0 {
		return results
	}
	excluded := cfg.excludeSet()
	// Preview ~1/4 of the threshold in terms of token worth.
	// Since 1 token ≈ 4 chars for English, MaxOutputTokens chars ≈ MaxOutputTokens/4 tokens.
	// E.g. threshold=7500 tokens → preview≈7500 chars≈1875 tokens.
	previewChars := cfg.MaxOutputTokens
	if previewChars < 200 {
		previewChars = 200
	}

	out := make([]ToolResultPart, len(results))
	for i, r := range results {
		if excluded[r.ToolName] {
			out[i] = r
			continue
		}
		tokens := EstimatePartResultTokens(r.Result)
		if tokens <= cfg.MaxOutputTokens {
			out[i] = r
			continue
		}
		// Truncate: keep a preview + summary.
		text := resultToString(r.Result)
		out[i] = ToolResultPart{
			ToolCallID: r.ToolCallID,
			ToolName:   r.ToolName,
			Result: fmt.Sprintf(
				"%s\n\n... [output truncated: ~%d tokens exceeded limit of %d. Use more specific parameters for full output.]",
				previewString(text, previewChars), tokens, cfg.MaxOutputTokens,
			),
			IsError: r.IsError,
		}
	}
	return out
}

// --- Phase 2: History compaction ---

// minCompactTokens is the minimum token size for a tool result to be eligible
// for compaction. Results smaller than this are left untouched.
const minCompactTokens = 200

// ReduceHistory applies Phase 2 compaction to the message history.
//
// If total message tokens are below ClearThresholdTokens, messages are
// returned unchanged. Otherwise, tool results in older messages (before the
// RetainRecentSteps boundary) are replaced with compact placeholders.
//
// This is a zero-cost operation (no LLM calls) — it only prunes large tool
// outputs from the conversation history within the orchestration loop.
func ReduceHistory(messages []Message, cfg ReductionConfig) []Message {
	if cfg.ClearThresholdTokens <= 0 || len(messages) == 0 {
		return messages
	}

	totalTokens := EstimateMessagesTokens(messages)
	if totalTokens <= cfg.ClearThresholdTokens {
		return messages
	}

	// Determine the boundary: keep the last RetainRecentSteps steps untouched.
	retainSteps := cfg.RetainRecentSteps
	if retainSteps <= 0 {
		retainSteps = 4
	}
	boundaryIdx := findRetainBoundary(messages, retainSteps)
	if boundaryIdx <= 0 {
		return messages // nothing to compact
	}

	excluded := cfg.excludeSet()

	// Compact tool results before the boundary.
	out := make([]Message, len(messages))
	copy(out, messages)

	for i := 0; i < boundaryIdx; i++ {
		if out[i].Role != MessageRoleTool {
			continue
		}
		changed := false
		compacted := make([]MessagePart, len(out[i].Content))
		for j, part := range out[i].Content {
			tr, ok := part.(ToolResultPart)
			if !ok || excluded[tr.ToolName] {
				compacted[j] = part
				continue
			}
			tokens := EstimatePartResultTokens(tr.Result)
			if tokens <= minCompactTokens {
				compacted[j] = part
				continue
			}
			preview := previewString(resultToString(tr.Result), 80)
			compacted[j] = ToolResultPart{
				ToolCallID: tr.ToolCallID,
				ToolName:   tr.ToolName,
				Result: fmt.Sprintf(
					"[compacted: ~%d tokens. Preview: %s]",
					tokens, preview,
				),
				IsError: tr.IsError,
			}
			changed = true
		}
		if changed {
			out[i].Content = compacted
		}
	}

	return out
}

// findRetainBoundary returns the message index before which messages are
// eligible for compaction. It keeps the last `retainSteps` orchestration
// steps (each step = assistant message + following tool messages).
func findRetainBoundary(messages []Message, retainSteps int) int {
	stepsFound := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == MessageRoleAssistant {
			stepsFound++
			if stepsFound >= retainSteps {
				return i
			}
		}
	}
	return 0
}

// --- Callback factories ---

// NewOnToolResultsCallback returns an OnToolResults callback that applies
// Phase 1 truncation to tool results after execution.
func NewOnToolResultsCallback(cfg ReductionConfig) func(int, []ToolResultPart) []ToolResultPart {
	return func(step int, results []ToolResultPart) []ToolResultPart {
		return TruncateToolResults(results, cfg)
	}
}

// NewReducePrepareStepCallback returns a PrepareStep callback that applies
// Phase 2 history compaction before each LLM call (from step 1 onward).
//
// This callback is designed to be chained with other PrepareStep callbacks.
// It modifies the params in-place and returns them.
func NewReducePrepareStepCallback(cfg ReductionConfig) func(*GenerateParams) *GenerateParams {
	return func(params *GenerateParams) *GenerateParams {
		params.Messages = ReduceHistory(params.Messages, cfg)
		return params
	}
}
