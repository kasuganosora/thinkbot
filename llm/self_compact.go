package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
// task or before switching topic), with a dedicated faithfulness-first
// summary prompt (see SelfCompactSystemPrompt).
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
// Summarization
//
// Self compaction has its own prompt and template instead of the automatic
// compactor's coding-session template (Goal/Progress/Relevant Files...). The
// first production call (2026-09-26, Telegram) showed why: fed with a casual
// chat whose "facts" were mostly the assistant's own narration, the coding
// template pushed the summarizer to fill every section, so it
//   - recorded an assistant claim that never happened as "Done",
//   - "completed" bare file names (orchestrate.go) into invented absolute
//     paths (it glued them to a working directory mentioned in the focus),
//   - upgraded the owner's suggestion to "agreed" (copied from the focus).
// The self-compact prompt therefore makes faithfulness the primary rule:
// attribute claims, keep identifiers verbatim or omit them, keep the status
// of proposals, and treat the model-provided focus as hints, not evidence.
// ---------------------------------------------------------------------------

const (
	selfCompactTextLimit       = 4000   // runes per text part fed to the summarizer
	selfCompactToolArgLimit    = 600    // runes per tool-call argument value
	selfCompactToolResultLimit = 1500   // runes per tool result
	selfCompactPromptLimit     = 240000 // runes of rendered history (keeps the newest)
)

// SelfCompactSystemPrompt is the system prompt of the compact_context summarizer.
const SelfCompactSystemPrompt = `You write a faithful memory note about an earlier part of a conversation between people and an AI assistant. The assistant will continue the conversation from your note instead of the original messages, so every line you write will be taken as true. The conversation may be casual chat, questions and answers, or a task (e.g. coding) — describe what actually happened, whatever it was.

Faithfulness rules (they override everything else):
1. Only write what the transcript supports: the people's messages, the assistant's messages, tool calls and tool results. Never add facts, outcomes, numbers, names, dates, file paths or identifiers that are not in the transcript.
2. Copy identifiers exactly as they appear (file paths, directories, commit hashes, branch names, URLs, IDs, commands, error text). Never complete, prefix, normalise or "fix" a partial identifier: if a message says "orchestrate.go", write "orchestrate.go", not a guessed directory. If you are not sure of the exact form, leave it out.
3. Attribute claims. What the assistant said it did, found or verified is a claim, not a fact, unless a tool result in the transcript confirms it. Write such items as "assistant said …". Tool results are evidence; assistant narration is not.
4. Keep the status of every item exact. Suggested / proposed / asked / considered / planned is NOT agreed / decided / approved / done. A person's suggestion stays a suggestion; only write "decided" or "agreed" when someone explicitly decided or agreed, and "done" only when the transcript shows it completed.
5. Never infer completion, success, permission or consent. When something is ambiguous or contradicted, put it under "Unverified / uncertain".
6. The <assistant-hints> block (if any) was written by the assistant to say what it wants preserved. Hints tell you what matters; they are NOT evidence. Include a hint only as far as the transcript supports it, with the transcript's wording and status. If a hint contradicts the transcript, follow the transcript.
7. A <previous-note> block (if any) is an earlier note about even older messages. It may contain mistakes: keep items that still matter, drop what is obsolete, never upgrade an item's certainty or status, and do not re-add items the transcript shows were resolved.

Style: terse bullets, same language as the conversation, no preamble. Do not answer or continue the conversation, and do not mention that this is a summary or that context was compacted.`

// SelfCompactTemplate is the output structure requested from the summarizer.
// It fits casual chat as well as task sessions: sections that do not apply
// are written as "(none)" instead of being padded.
const SelfCompactTemplate = `Output exactly the Markdown structure inside <template>, in this order, without the <template> tags. Write "(none)" for a section that does not apply — do not invent content to fill it.
<template>
## Topics
- [what was discussed, in order; who raised it (names/handles as written)]

## Facts established
- [facts supported by the transcript: what people stated about themselves/their wishes, and results shown by tools; attribute where it matters]

## Decisions & commitments
- [explicit decisions (who decided), requests, suggestions and promises, each with its exact status: suggested / requested / agreed / promised / done]

## Open items
- [pending tasks, unanswered questions, things waiting on someone]

## Unverified / uncertain
- [claims the transcript does not confirm (e.g. "assistant said X was done", no tool result shows it), ambiguities, contradictions]

## Identifiers
- [exact paths, hashes, branches, URLs, IDs copied verbatim from the transcript: what each refers to]
</template>`

// SelfCompactOptions tunes one self-compaction summary.
type SelfCompactOptions struct {
	// Focus: optional notes from the model on what to preserve (hints only).
	Focus string
	// MaxOutputTokens caps the summarizer response (0 → compactor SummaryMaxTokens).
	// With reasoning models the cap also covers reasoning tokens.
	MaxOutputTokens int
	// TargetWords is the soft length target stated in the prompt (0 → none).
	TargetWords int
	// ReasoningEffort is passed to the provider when non-empty (e.g. "low").
	ReasoningEffort string
}

// PreviousSummary returns the anchored summary kept for incremental updates
// by the automatic compactor.
func (c *Compactor) PreviousSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.previousSummary
}

// ErrSelfCompactSummaryTruncated is returned when the summarizer hit its
// output limit: a cut-off note would silently lose context, so it is rejected.
var ErrSelfCompactSummaryTruncated = errors.New("summary was cut off by the output token limit")

// SummarizeForSelfCompact summarizes head for a model-initiated compaction.
//
// It renders tool calls/results (tool-heavy turns carry most of their
// information there) without truncating argument keys, treats opts.Focus as
// hints, and uses an older anchored summary only when it is part of head (a
// leading [Conversation Summary] message, i.e. an applied checkpoint). It does
// not read or write the compactor's in-memory previousSummary: the anchor for
// self compaction is the persisted checkpoint, which is visible, auditable and
// subject to expiry, so an expired note cannot resurface from memory.
func (c *Compactor) SummarizeForSelfCompact(ctx context.Context, provider Provider, model *Model, head []Message, opts SelfCompactOptions) (string, error) {
	if len(head) == 0 {
		return "", errors.New("nothing to summarize")
	}
	if provider == nil {
		return "", errors.New("no provider for summarization")
	}
	prompt := buildSelfCompactPrompt(head, opts)
	maxTokens := opts.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = c.liveConfig().SummaryMaxTokens
	}
	temp := 0.2
	params := GenerateParams{
		Model:       model,
		System:      SelfCompactSystemPrompt,
		Messages:    []Message{UserMessage(prompt)},
		MaxTokens:   &maxTokens,
		Temperature: &temp,
	}
	if e := strings.TrimSpace(opts.ReasoningEffort); e != "" {
		params.ReasoningEffort = &e
	}
	res, err := provider.DoGenerate(WithStatsFeature(ctx, "context_compact"), params)
	if err != nil {
		return "", err
	}
	if res.FinishReason == FinishReasonLength {
		return "", ErrSelfCompactSummaryTruncated
	}
	summary := strings.TrimSpace(res.Text)
	if summary == "" {
		return "", errors.New("summarizer returned an empty summary")
	}
	return summary, nil
}

// splitLeadingSummary separates a leading anchored summary message (an
// applied checkpoint or an earlier compaction) from the rest of head.
func splitLeadingSummary(head []Message) (string, []Message) {
	if len(head) == 0 || head[0].Role != MessageRoleSystem {
		return "", head
	}
	text := strings.TrimSpace(TextFromParts(head[0].Content))
	if !strings.HasPrefix(text, ConversationSummaryHeader) {
		return "", head
	}
	text = strings.TrimPrefix(text, ConversationSummaryHeader)
	text = strings.TrimSuffix(strings.TrimSpace(text), ConversationSummaryFooter)
	return strings.TrimSpace(text), head[1:]
}

func buildSelfCompactPrompt(head []Message, opts SelfCompactOptions) string {
	prev, rest := splitLeadingSummary(head)
	var sb strings.Builder
	if prev != "" {
		sb.WriteString("<previous-note>\n")
		sb.WriteString(prev)
		sb.WriteString("\n</previous-note>\n\n")
	}
	sb.WriteString("<transcript>\n")
	sb.WriteString("Messages in chronological order. [user] = a person talking to the assistant, [assistant] = the AI assistant (its statements are claims unless a tool result confirms them), [system] = system notices.\n\n")
	sb.WriteString(renderMessagesForSummary(rest, summaryRenderLimits{
		text: selfCompactTextLimit, toolArg: selfCompactToolArgLimit, toolResult: selfCompactToolResultLimit,
	}, selfCompactPromptLimit))
	sb.WriteString("\n</transcript>\n\n")
	if f := strings.TrimSpace(opts.Focus); f != "" {
		sb.WriteString("<assistant-hints>\n")
		sb.WriteString(f)
		sb.WriteString("\n</assistant-hints>\n\n")
	}
	if prev != "" {
		sb.WriteString("Write one updated note that merges the previous note with the transcript.\n")
	} else {
		sb.WriteString("Write the note for the transcript.\n")
	}
	sb.WriteString("Keep who said what (names/handles as written), commitments made to people with their exact status, and unresolved questions.\n")
	if opts.TargetWords > 0 {
		fmt.Fprintf(&sb, "Keep the whole note short: at most about %d words (or %d CJK characters). Drop details that will not matter later; never drop an open commitment.\n", opts.TargetWords, opts.TargetWords*2)
	}
	sb.WriteString("\n")
	sb.WriteString(SelfCompactTemplate)
	return sb.String()
}

type summaryRenderLimits struct {
	text       int // runes per text part
	toolArg    int // runes per tool-call argument value
	toolResult int // runes per tool result
}

// RenderMessagesForSummary renders messages as plain text for a summarizer,
// including tool calls and (truncated) tool results. When the rendering
// exceeds totalLimit runes, the oldest messages are dropped first.
func RenderMessagesForSummary(messages []Message, partLimit, totalLimit int) string {
	argLimit := partLimit / 3
	if partLimit > 0 && argLimit < 1 {
		argLimit = 1
	}
	return renderMessagesForSummary(messages, summaryRenderLimits{text: partLimit, toolArg: argLimit, toolResult: partLimit}, totalLimit)
}

func renderMessagesForSummary(messages []Message, lim summaryRenderLimits, totalLimit int) string {
	var blocks []string
	for _, m := range messages {
		var parts []string
		for _, p := range m.Content {
			switch v := p.(type) {
			case TextPart:
				if t := strings.TrimSpace(v.Text); t != "" {
					parts = append(parts, clipRunes(t, lim.text))
				}
			case ToolCallPart:
				parts = append(parts, fmt.Sprintf("<tool_call name=%q>%s</tool_call>", v.ToolName, renderToolInput(v.Input, lim.toolArg)))
			case ToolResultPart:
				tag := "tool_result"
				if v.IsError {
					tag = "tool_error"
				}
				parts = append(parts, fmt.Sprintf("<%s name=%q>%s</%s>", tag, v.ToolName, clipRunes(renderAny(v.Result), lim.toolResult), tag))
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

// renderToolInput renders tool-call arguments for the summarizer, clipping
// each argument value separately. Clipping the marshalled JSON as a whole
// (the previous behaviour) cut off whatever came last — json.Marshal sorts
// map keys, so for write_file-style calls {"content": <long>, "path": ...}
// the path was the part that got lost.
func renderToolInput(input any, valueLimit int) string {
	var v any = input
	switch t := input.(type) {
	case string:
		if json.Unmarshal([]byte(t), &v) != nil {
			return clipRunes(t, valueLimit)
		}
	case []byte:
		if json.Unmarshal(t, &v) != nil {
			return clipRunes(string(t), valueLimit)
		}
	case json.RawMessage:
		if json.Unmarshal(t, &v) != nil {
			return clipRunes(string(t), valueLimit)
		}
	}
	m, ok := v.(map[string]any)
	if !ok {
		if b, err := json.Marshal(v); err == nil {
			var generic map[string]any
			if json.Unmarshal(b, &generic) == nil && generic != nil {
				m, ok = generic, true
			}
		}
	}
	if !ok {
		return clipRunes(renderAny(v), valueLimit)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		var val string
		if s, isStr := m[k].(string); isStr {
			b, _ := json.Marshal(clipRunes(s, valueLimit))
			val = string(b)
		} else {
			val = clipRunes(renderAny(m[k]), valueLimit)
		}
		fmt.Fprintf(&sb, "%q: %s", k, val)
	}
	sb.WriteString("}")
	return sb.String()
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
