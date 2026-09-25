package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// ============================================================================
// Self compaction — model-initiated context compaction (compact_context tool)
//
// The automatic Compactor (compaction.go) only fires when the estimated prompt
// crosses a token threshold. Self compaction lets the model itself decide to
// fold older context into an anchored summary (e.g. after finishing a large
// task or before switching topic), reusing the same Compactor/summary prompt.
//
// Mechanics:
//   - Each orchestration run (OrchestrateGenerate / OrchestrateStream) attaches
//     a fresh *LiveContext to the ctx passed to tools. Right before tools run,
//     the loop records a snapshot of the messages that were sent this step.
//   - A tool (compact_context) reads the snapshot, picks a pairing-safe
//     boundary with SelectCompactionBoundary, summarizes the head, and calls
//     ScheduleReplaceHead(boundary, summaryMsg).
//   - After the step's assistant/tool messages are appended, the loop applies
//     the pending replacement: messages = [summary] + messages[boundary:].
//     The loop only ever appends between snapshot and apply, so the boundary
//     index stays valid, and the compact_context call/result pair itself lands
//     in the kept tail.
// ============================================================================

// ConversationSummaryHeader / Footer frame an anchored summary injected into
// the message list. Shared by the automatic compactor and self compaction so
// the model always sees the same shape.
const (
	ConversationSummaryHeader = "[Conversation Summary]"
	ConversationSummaryFooter = "[End of Summary]"
)

// FormatConversationSummary wraps summary text in the standard frame.
func FormatConversationSummary(summary string) string {
	return fmt.Sprintf("%s\n%s\n%s", ConversationSummaryHeader, strings.TrimSpace(summary), ConversationSummaryFooter)
}

// ConversationSummaryMessage builds the system message that replaces
// compacted history (same role/shape the automatic compactor uses).
func ConversationSummaryMessage(summary string) Message {
	return SystemMessage(FormatConversationSummary(summary))
}

// Errors returned by LiveContext.ScheduleReplaceHead.
var (
	ErrSelfCompactAlreadyDone = errors.New("context was already compacted during this turn")
	ErrSelfCompactBadBoundary = errors.New("invalid compaction boundary")
)

// LiveContext is the per-orchestration handle that lets a tool observe the
// messages of the running loop and schedule a head replacement. It is safe
// for concurrent use (tools of one step may run in parallel).
type LiveContext struct {
	mu       sync.Mutex
	snapshot []Message
	pending  *liveReplacement
	applied  bool
}

type liveReplacement struct {
	boundary int
	summary  Message
	// snapLen is len(snapshot) when scheduled; used to validate that the loop
	// only appended afterwards.
	snapLen int
}

// NewLiveContext creates an empty handle.
func NewLiveContext() *LiveContext { return &LiveContext{} }

type liveContextKey struct{}

// WithLiveContext attaches lc to ctx.
func WithLiveContext(ctx context.Context, lc *LiveContext) context.Context {
	return context.WithValue(ctx, liveContextKey{}, lc)
}

// LiveContextFromContext returns the handle of the innermost orchestration
// run, or nil when called outside of one.
func LiveContextFromContext(ctx context.Context) *LiveContext {
	if ctx == nil {
		return nil
	}
	lc, _ := ctx.Value(liveContextKey{}).(*LiveContext)
	return lc
}

// SetSnapshot records the messages sent in the current step. The
// orchestration loop calls it right before tools execute; it is exported for
// custom loops and tests.
func (lc *LiveContext) SetSnapshot(messages []Message) {
	if lc == nil {
		return
	}
	cp := make([]Message, len(messages))
	copy(cp, messages)
	lc.mu.Lock()
	lc.snapshot = cp
	lc.mu.Unlock()
}

// Snapshot returns a copy of the messages sent in the current step.
func (lc *LiveContext) Snapshot() []Message {
	if lc == nil {
		return nil
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	cp := make([]Message, len(lc.snapshot))
	copy(cp, lc.snapshot)
	return cp
}

// Compacted reports whether a replacement was scheduled or applied in this run.
func (lc *LiveContext) Compacted() bool {
	if lc == nil {
		return false
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.pending != nil || lc.applied
}

// ScheduleReplaceHead asks the loop to replace snapshot[:boundary] with
// summary once the current step's messages are appended. At most one
// replacement per orchestration run is accepted. The boundary must be
// pairing-safe (see IsSafeCompactionBoundary).
func (lc *LiveContext) ScheduleReplaceHead(boundary int, summary Message) error {
	if lc == nil {
		return errors.New("no live conversation context")
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.pending != nil || lc.applied {
		return ErrSelfCompactAlreadyDone
	}
	if boundary <= 0 || boundary >= len(lc.snapshot) {
		return ErrSelfCompactBadBoundary
	}
	if !IsSafeCompactionBoundary(lc.snapshot, boundary) {
		return fmt.Errorf("%w: boundary %d splits a tool call from its result", ErrSelfCompactBadBoundary, boundary)
	}
	lc.pending = &liveReplacement{boundary: boundary, summary: summary, snapLen: len(lc.snapshot)}
	return nil
}

// applyPending applies a scheduled replacement to the loop's message list.
// It returns the (possibly) new slice and whether a replacement happened.
func (lc *LiveContext) applyPending(messages []Message) ([]Message, bool) {
	if lc == nil {
		return messages, false
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	p := lc.pending
	if p == nil {
		return messages, false
	}
	lc.pending = nil
	// The loop only appends between snapshot and apply; if that invariant is
	// broken (e.g. PrepareStep rewrote history), drop the replacement rather
	// than cutting at a wrong index.
	if len(messages) < p.snapLen || p.boundary >= len(messages) || !IsSafeCompactionBoundary(messages, p.boundary) {
		return messages, false
	}
	out := make([]Message, 0, 1+len(messages)-p.boundary)
	out = append(out, p.summary)
	out = append(out, messages[p.boundary:]...)
	lc.applied = true
	lc.snapshot = nil
	return out, true
}

// IsSafeCompactionBoundary reports whether cutting messages at index b (head =
// messages[:b], tail = messages[b:]) keeps every tool call together with its
// results: the tail must not start with a tool-result message, and no tool
// result in the tail may answer a tool call that lives in the head.
func IsSafeCompactionBoundary(messages []Message, b int) bool {
	if b <= 0 || b >= len(messages) {
		return false
	}
	if messages[b].Role == MessageRoleTool {
		return false
	}
	headCalls := make(map[string]bool)
	for _, m := range messages[:b] {
		for _, p := range m.Content {
			if tc, ok := p.(ToolCallPart); ok && tc.ToolCallID != "" {
				headCalls[tc.ToolCallID] = true
			}
		}
	}
	if len(headCalls) == 0 {
		return true
	}
	for _, m := range messages[b:] {
		for _, p := range m.Content {
			if tr, ok := p.(ToolResultPart); ok && headCalls[tr.ToolCallID] {
				return false
			}
		}
	}
	return true
}

// SelectCompactionBoundary picks the head/tail split for compaction, keeping
// at least keepRecent messages verbatim. It walks towards the start (keeping
// more) until a pairing-safe boundary is found, preferring one that starts at
// a user turn when such a boundary exists within a small window. Returns 0
// when nothing can be compacted safely.
func SelectCompactionBoundary(messages []Message, keepRecent int) int {
	if keepRecent < 1 {
		keepRecent = 1
	}
	target := len(messages) - keepRecent
	if target <= 0 {
		return 0
	}
	firstSafe := 0
	for b := target; b > 0; b-- {
		if !IsSafeCompactionBoundary(messages, b) {
			continue
		}
		if firstSafe == 0 {
			firstSafe = b
		}
		if messages[b].Role == MessageRoleUser {
			return b
		}
		// Do not trade away more than a few extra messages just to land on
		// a user turn; an assistant-start boundary is equally valid.
		if firstSafe-b >= 4 {
			break
		}
	}
	return firstSafe
}

// ---------------------------------------------------------------------------
// Summarization (reuses Compactor prompt + incremental previousSummary state)
// ---------------------------------------------------------------------------

const (
	selfCompactPartLimit   = 1500   // runes per text/tool part fed to the summarizer
	selfCompactPromptLimit = 240000 // runes of rendered history (keeps the newest)
)

// PreviousSummary returns the anchored summary kept for incremental updates.
func (c *Compactor) PreviousSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.previousSummary
}

// SummarizeForSelfCompact summarizes head for a model-initiated compaction.
// Unlike SummarizeHead it renders tool calls/results (tool-heavy turns carry
// most of their information there), honours a caller-provided focus, and
// updates the compactor's incremental previousSummary so later automatic
// compactions build on the same anchor.
func (c *Compactor) SummarizeForSelfCompact(ctx context.Context, provider Provider, model *Model, head []Message, focus string) (string, error) {
	if len(head) == 0 {
		return "", errors.New("nothing to summarize")
	}
	if provider == nil {
		return "", errors.New("no provider for summarization")
	}
	prompt := c.buildSelfCompactPrompt(head, focus)
	maxTokens := c.liveConfig().SummaryMaxTokens
	temp := 0.3
	res, err := provider.DoGenerate(WithStatsFeature(ctx, "context_compact"), GenerateParams{
		Model:       model,
		System:      CompactionSystemPrompt,
		Messages:    []Message{UserMessage(prompt)},
		MaxTokens:   &maxTokens,
		Temperature: &temp,
	})
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(res.Text)
	if summary == "" {
		return "", errors.New("summarizer returned an empty summary")
	}
	c.mu.Lock()
	c.previousSummary = summary
	c.mu.Unlock()
	return summary, nil
}

func (c *Compactor) buildSelfCompactPrompt(head []Message, focus string) string {
	var sb strings.Builder
	sb.WriteString("Conversation to summarize:\n\n")
	sb.WriteString(RenderMessagesForSummary(head, selfCompactPartLimit, selfCompactPromptLimit))
	sb.WriteString("\n---\n\n")
	if prev := c.PreviousSummary(); prev != "" {
		sb.WriteString("Update the anchored summary below using the conversation history above.\n")
		sb.WriteString("Preserve still-true details, remove stale details, and merge in the new facts.\n")
		sb.WriteString("<previous-summary>\n")
		sb.WriteString(prev)
		sb.WriteString("\n</previous-summary>\n\n")
	} else {
		sb.WriteString("Create a new anchored summary from the conversation history.\n\n")
	}
	if f := strings.TrimSpace(focus); f != "" {
		sb.WriteString("The assistant explicitly asked that the summary preserve the following (keep these details verbatim where possible):\n<must-preserve>\n")
		sb.WriteString(f)
		sb.WriteString("\n</must-preserve>\n\n")
	}
	sb.WriteString("Also keep: who said what (names/handles), commitments or promises made to people, and any unresolved questions.\n\n")
	sb.WriteString(SummaryTemplate)
	return sb.String()
}

// RenderMessagesForSummary renders messages as plain text for a summarizer,
// including tool calls and (truncated) tool results. When the rendering
// exceeds totalLimit runes, the oldest lines are dropped first.
func RenderMessagesForSummary(messages []Message, partLimit, totalLimit int) string {
	var blocks []string
	for _, m := range messages {
		var parts []string
		for _, p := range m.Content {
			switch v := p.(type) {
			case TextPart:
				if t := strings.TrimSpace(v.Text); t != "" {
					parts = append(parts, clipRunes(t, partLimit))
				}
			case ToolCallPart:
				parts = append(parts, fmt.Sprintf("<tool_call name=%q>%s</tool_call>", v.ToolName, clipRunes(renderAny(v.Input), partLimit/3)))
			case ToolResultPart:
				tag := "tool_result"
				if v.IsError {
					tag = "tool_error"
				}
				parts = append(parts, fmt.Sprintf("<%s name=%q>%s</%s>", tag, v.ToolName, clipRunes(renderAny(v.Result), partLimit), tag))
			case ImagePart:
				parts = append(parts, "[image]")
			case FilePart:
				parts = append(parts, "[file]")
			}
		}
		if len(parts) == 0 {
			continue
		}
		blocks = append(blocks, fmt.Sprintf("[%s]: %s", m.Role, strings.Join(parts, "\n")))
	}
	// Keep the newest blocks within totalLimit.
	total := 0
	start := len(blocks)
	for i := len(blocks) - 1; i >= 0; i-- {
		n := utf8.RuneCountInString(blocks[i]) + 2
		if totalLimit > 0 && total+n > totalLimit && start < len(blocks) {
			break
		}
		total += n
		start = i
	}
	out := strings.Join(blocks[start:], "\n\n")
	if start > 0 {
		out = fmt.Sprintf("[... %d older messages omitted ...]\n\n", start) + out
	}
	return out
}

func renderAny(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case json.RawMessage:
		return string(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func clipRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…(truncated)"
}
