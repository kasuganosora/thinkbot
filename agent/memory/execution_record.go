package memory

import (
	"regexp"
	"strings"
)

// Execution-record tags wrap the runtime-generated summary of a previous
// assistant turn's tool calls when it is replayed into the LLM context.
//
// The block is deliberately a tagged system message rather than part of the
// assistant content: if it lived inside the assistant message the model would
// learn to imitate the format and could fabricate its own "execution record".
// Outbound cleaning strips the block in all three paths (channel text, stored
// public block, Web stream) so a model that echoes it cannot leak it.
const (
	ExecutionRecordTagOpen  = "<execution_record>"
	ExecutionRecordTagClose = "</execution_record>"
)

var (
	executionRecordRe         = regexp.MustCompile(`(?is)<execution_record>.*?</execution_record>`)
	executionRecordUnclosedRe = regexp.MustCompile(`(?is)<execution_record>.*$`)
)

// StripExecutionRecords removes a replayed execution-record block (including
// its content) from outbound text. Same three-step shape as StripRecallBlocks:
// complete pairs first, then an unclosed opening tag (streaming truncation),
// then surrounding whitespace.
func StripExecutionRecords(text string) string {
	if !strings.Contains(strings.ToLower(text), "<execution_record") {
		return text
	}
	cleaned := executionRecordRe.ReplaceAllString(text, "")
	cleaned = executionRecordUnclosedRe.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}
