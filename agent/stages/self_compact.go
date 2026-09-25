package stages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/session"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// compact_context — model-initiated context compaction
//
// The main agent's automatic compactor (LLMConfig.Compaction) only fires on a
// token threshold. This tool lets the bot decide by itself to fold older
// context into an anchored summary, e.g. after finishing a large task or when
// switching topics in a long conversation.
//
// Two layers are compacted with ONE summary:
//  1. The live turn: the orchestration loop replaces messages[:boundary] with
//     the summary (llm.LiveContext), so the remaining steps of this turn run on
//     summary + recent messages. The boundary never splits a tool call from its
//     results; the system prompt/persona (GenerateParams.System, incl. memory
//     recall and pinned prompt sections) is never part of the message list and
//     therefore always preserved.
//  2. Subsequent turns (channels with persisted chat history: web, telegram,
//     workflow continuation): a checkpoint (summary + boundary chat_messages.id)
//     is stored; history loading then yields summary + rows after the boundary.
//     Raw rows are never deleted, so this is reversible and auditable.
//
// The summary reuses the per-session llm.Compactor (same prompt/template and
// incremental previous-summary anchor as automatic compaction).
// ============================================================================

// CompactContextToolName is the tool name exposed to the model.
const CompactContextToolName = "compact_context"

// Self-compaction defaults (overridable via SelfCompactConfig).
const (
	defaultSelfCompactKeepRecent = 6
	minSelfCompactKeepRecent     = 2
	maxSelfCompactKeepRecent     = 40
	defaultSelfCompactCooldown   = 10 * time.Minute
	defaultSelfCompactMinMsgs    = 8
	defaultSelfCompactMinTokens  = 2000
	minSelfCompactHeadMsgs       = 3
	defaultSelfCompactTimeout    = 3 * time.Minute
	maxSelfCompactFocusRunes     = 1000
	selfCompactPreviewRunes      = 600
)

// ContextCheckpoint is a persisted compaction boundary for one chat session:
// history rows with id <= BoundaryMessageID are represented by Summary.
type ContextCheckpoint struct {
	BotID             string
	SessionID         string
	BoundaryMessageID uint64
	Summary           string
	Focus             string
	CompactedMessages int
	TokensBefore      int
	TokensAfter       int
	Source            string
}

// ContextCheckpointStore persists compaction checkpoints (implemented by the
// api layer on top of chat_messages / context_checkpoints).
type ContextCheckpointStore interface {
	// LatestContextCheckpointBoundary returns the boundary of the active
	// checkpoint for the session (0 when none).
	LatestContextCheckpointBoundary(botID, sessionID string) (uint64, error)
	// SaveContextCheckpoint stores a new active checkpoint.
	SaveContextCheckpoint(cp ContextCheckpoint) error
}

// SelfCompactConfig enables the compact_context tool (nil = disabled).
type SelfCompactConfig struct {
	// Store persists checkpoints for sessions with chat history. nil → only
	// the live turn is compacted.
	Store ContextCheckpointStore
	// HistoryMessageIDs returns the chat_messages IDs aligned 1:1 with the
	// leading history messages produced by LLMConfig.MessageBuilder for msg
	// (0 for synthetic entries such as an injected checkpoint summary).
	HistoryMessageIDs func(msg core.Message) []uint64
	// Cooldown between two compactions of the same session (0 → 10min).
	Cooldown time.Duration
	// DefaultKeepRecent messages kept verbatim when the model omits keep_recent (0 → 6).
	DefaultKeepRecent int
	// MinMessages / MinTokens: below either threshold the call is a no-op.
	MinMessages int
	MinTokens   int
	// SummaryTimeout bounds the summarizer LLM call (0 → 3min).
	SummaryTimeout time.Duration
}

func (c *SelfCompactConfig) cooldown() time.Duration {
	if c.Cooldown > 0 {
		return c.Cooldown
	}
	return defaultSelfCompactCooldown
}

func (c *SelfCompactConfig) keepDefault() int {
	if c.DefaultKeepRecent > 0 {
		return c.DefaultKeepRecent
	}
	return defaultSelfCompactKeepRecent
}

func (c *SelfCompactConfig) minMessages() int {
	if c.MinMessages > 0 {
		return c.MinMessages
	}
	return defaultSelfCompactMinMsgs
}

func (c *SelfCompactConfig) minTokens() int {
	if c.MinTokens > 0 {
		return c.MinTokens
	}
	return defaultSelfCompactMinTokens
}

func (c *SelfCompactConfig) timeout() time.Duration {
	if c.SummaryTimeout > 0 {
		return c.SummaryTimeout
	}
	return defaultSelfCompactTimeout
}

// selfCompactCooldowns tracks the last successful compaction per session key.
type selfCompactCooldowns struct {
	m sync.Map // key → time.Time
}

func (c *selfCompactCooldowns) remaining(key string, cd time.Duration, now time.Time) time.Duration {
	v, ok := c.m.Load(key)
	if !ok {
		return 0
	}
	last := v.(time.Time)
	if r := cd - now.Sub(last); r > 0 {
		return r
	}
	return 0
}

func (c *selfCompactCooldowns) mark(key string, now time.Time) { c.m.Store(key, now) }

const compactContextDescription = `Compress (compact) YOUR OWN conversation context: older messages of the current conversation are replaced by a concise structured summary, and the most recent messages are kept verbatim. Takes effect from your next step in this turn and (where the channel persists chat history) for later turns too. The system prompt/persona and memory are never touched; raw history is kept in storage (reversible).

USE it when:
- the conversation or this turn has grown long (many tool calls / large tool outputs) and you are getting close to the context limit;
- you just finished a big multi-step task and the details are no longer needed verbatim;
- the topic clearly switches and the old back-and-forth is no longer relevant.

DO NOT use it when:
- the chat is short (it will simply no-op);
- you are in the middle of a multi-step task whose exact intermediate outputs you still need (finish or reach a checkpoint first);
- you already compacted recently (limited to once per turn with a cooldown) — never call it repeatedly.

Use "focus" to list what the summary MUST preserve (open tasks, promises, IDs, file paths, decisions). Returns before/after message and token estimates plus a summary preview.`

func compactContextSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"keep_recent": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("How many most-recent messages to keep verbatim (default %d, range %d-%d). The boundary is moved earlier if needed so a tool call is never separated from its result.", defaultSelfCompactKeepRecent, minSelfCompactKeepRecent, maxSelfCompactKeepRecent),
				"minimum":     minSelfCompactKeepRecent,
				"maximum":     maxSelfCompactKeepRecent,
			},
			"focus": map[string]any{
				"type":        "string",
				"description": "Optional notes on what the summary must preserve (open tasks, commitments, names, IDs, paths, decisions). Max 1000 characters.",
			},
		},
	}
}

type compactContextArgs struct {
	KeepRecent int
	Focus      string
}

func parseCompactContextArgs(input any, defKeep int) (compactContextArgs, error) {
	args := compactContextArgs{KeepRecent: defKeep}
	var m map[string]any
	switch v := input.(type) {
	case nil:
	case map[string]any:
		m = v
	case string:
		if strings.TrimSpace(v) != "" {
			if err := json.Unmarshal([]byte(v), &m); err != nil {
				return args, fmt.Errorf("invalid arguments: %v", err)
			}
		}
	case json.RawMessage:
		if len(v) > 0 {
			if err := json.Unmarshal(v, &m); err != nil {
				return args, fmt.Errorf("invalid arguments: %v", err)
			}
		}
	case []byte:
		if len(v) > 0 {
			if err := json.Unmarshal(v, &m); err != nil {
				return args, fmt.Errorf("invalid arguments: %v", err)
			}
		}
	default:
		b, err := json.Marshal(v)
		if err == nil {
			err = json.Unmarshal(b, &m)
		}
		if err != nil {
			return args, fmt.Errorf("invalid arguments: %v", err)
		}
	}
	if m != nil {
		switch n := m["keep_recent"].(type) {
		case float64:
			args.KeepRecent = int(n)
		case int:
			args.KeepRecent = n
		case int64:
			args.KeepRecent = int(n)
		case json.Number:
			if i, err := n.Int64(); err == nil {
				args.KeepRecent = int(i)
			}
		}
		if f, ok := m["focus"].(string); ok {
			args.Focus = strings.TrimSpace(f)
		}
	}
	if args.KeepRecent < minSelfCompactKeepRecent {
		args.KeepRecent = minSelfCompactKeepRecent
	}
	if args.KeepRecent > maxSelfCompactKeepRecent {
		args.KeepRecent = maxSelfCompactKeepRecent
	}
	if utf8.RuneCountInString(args.Focus) > maxSelfCompactFocusRunes {
		args.Focus = string([]rune(args.Focus)[:maxSelfCompactFocusRunes])
	}
	return args, nil
}

// selfCompactTurn carries the per-turn inputs captured when the tool is built.
type selfCompactTurn struct {
	botID         string
	chatSessionID string // chat_messages session (empty → no persistence)
	cooldownKey   string
	source        string
	baseMessages  []llm.Message // MessageBuilder output (history + current)
	historyIDs    []uint64      // aligned with baseMessages prefix
}

func selfCompactCooldownKey(env *core.Envelope) string {
	if sid := session.SessionIDFromEnvelope(env); sid != "" {
		return "sess:" + sid
	}
	if cs := chatSessionIDFromEnvelope(env); cs != "" {
		return "chat:" + cs
	}
	return "chan:" + env.Message.BotID + ":" + env.Message.Source + ":" + env.Message.Channel
}

// shouldOfferSelfCompact decides whether compact_context is exposed this turn.
func (s *LLMStage) shouldOfferSelfCompact(env *core.Envelope, tools []llm.Tool) bool {
	if s.config.SelfCompact == nil || len(tools) == 0 || s.config.MaxSteps == 0 {
		return false
	}
	// Heartbeat wake-ups are single-shot JSON decisions without history.
	if isHeartbeatMode(env) || env.Message.Source == core.SourceHeartbeat {
		return false
	}
	for _, t := range tools {
		if t.Name == CompactContextToolName {
			return false
		}
	}
	return true
}

// newCompactContextTool builds the per-turn compact_context tool.
// It is never DeferredLoad: it is most needed exactly when the context is
// large, and its schema is tiny, so it stays visible without tool_search.
func (s *LLMStage) newCompactContextTool(env *core.Envelope, baseMessages []llm.Message) llm.Tool {
	cfg := s.config.SelfCompact
	turn := selfCompactTurn{
		botID:         env.Message.BotID,
		chatSessionID: chatSessionIDFromEnvelope(env),
		cooldownKey:   selfCompactCooldownKey(env),
		source:        env.Message.Source,
		baseMessages:  baseMessages,
	}
	if cfg.HistoryMessageIDs != nil {
		turn.historyIDs = cfg.HistoryMessageIDs(env.Message)
	}
	compactorKey := session.SessionIDFromEnvelope(env)
	return llm.Tool{
		Name:        CompactContextToolName,
		Description: compactContextDescription,
		Parameters:  compactContextSchema(),
		Keywords:    []string{"compact", "compress", "summarize", "context", "压缩", "上下文", "摘要"},
		Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
			return s.runCompactContext(ctx, cfg, turn, compactorKey, input)
		},
	}
}

func (s *LLMStage) summaryCompactor(key string) *llm.Compactor {
	if c, ok := s.getCompactor(key); ok && c != nil {
		return c
	}
	// Automatic compaction disabled: use a default-config compactor that is
	// still kept per session so the incremental anchor survives across turns.
	if key == "" {
		key = "__default__"
	}
	v, _ := s.selfCompactors.LoadOrStore(key, llm.NewCompactor(llm.DefaultCompactionConfig()).SetLogger(s.logger))
	return v.(*llm.Compactor)
}

func (s *LLMStage) runCompactContext(ctx *llm.ToolExecContext, cfg *SelfCompactConfig, turn selfCompactTurn, compactorKey string, input any) (any, error) {
	logger := traceid.WithLoggerFrom(ctx, s.logger)
	args, err := parseCompactContextArgs(input, cfg.keepDefault())
	if err != nil {
		return nil, err
	}

	live := llm.LiveContextFromContext(ctx)
	if live == nil {
		return nil, errors.New("compact_context: no active conversation loop to compact")
	}
	if live.Compacted() {
		return nil, errors.New("compact_context: context was already compacted in this turn; do not call it again this turn")
	}
	now := time.Now()
	if r := s.selfCompactCD.remaining(turn.cooldownKey, cfg.cooldown(), now); r > 0 {
		return nil, fmt.Errorf("compact_context: cooling down — this conversation was compacted recently; try again in %s (only if really needed)", r.Round(time.Second))
	}

	snapshot := live.Snapshot()
	tokensBefore := llm.EstimateMessagesTokens(snapshot)
	noop := func(reason string) (any, error) {
		logger.Infow("context_compact",
			"tool", CompactContextToolName, "status", "noop", "reason", reason,
			"bot_id", turn.botID, "chat_session", turn.chatSessionID, "source", turn.source,
			"messages", len(snapshot), "est_tokens", tokensBefore)
		return map[string]any{
			"status":     "noop",
			"reason":     reason,
			"messages":   len(snapshot),
			"est_tokens": tokensBefore,
			"note":       "Nothing was changed. Continue normally; do not retry compact_context for this conversation soon.",
		}, nil
	}
	if len(snapshot) < cfg.minMessages() || tokensBefore < cfg.minTokens() {
		return noop(fmt.Sprintf("history is already short (%d messages, ~%d tokens; thresholds %d messages / %d tokens)",
			len(snapshot), tokensBefore, cfg.minMessages(), cfg.minTokens()))
	}
	boundary := llm.SelectCompactionBoundary(snapshot, args.KeepRecent)
	if boundary < minSelfCompactHeadMsgs {
		return noop(fmt.Sprintf("not enough older messages to compact safely while keeping the most recent %d verbatim", args.KeepRecent))
	}
	head := snapshot[:boundary]

	sumCtx, cancel := context.WithTimeout(ctx, cfg.timeout())
	defer cancel()
	compactor := s.summaryCompactor(compactorKey)
	summary, err := compactor.SummarizeForSelfCompact(sumCtx, s.provider, s.config.Model, head, args.Focus)
	if err != nil {
		logger.Warnw("context_compact",
			"tool", CompactContextToolName, "status", "error", "stage", "summarize",
			"bot_id", turn.botID, "chat_session", turn.chatSessionID, "err", err)
		return nil, fmt.Errorf("compact_context: summarization failed, context left unchanged: %v", err)
	}
	summaryMsg := llm.ConversationSummaryMessage(summary)
	if err := live.ScheduleReplaceHead(boundary, summaryMsg); err != nil {
		return nil, fmt.Errorf("compact_context: %v", err)
	}
	s.selfCompactCD.mark(turn.cooldownKey, now)

	kept := snapshot[boundary:]
	messagesAfter := 1 + len(kept)
	tokensAfter := llm.EstimateMessageTokens(summaryMsg) + llm.EstimateMessagesTokens(kept)

	// Persist a checkpoint for later turns (sessions with chat history only).
	persisted := false
	persistNote := ""
	var boundaryID uint64
	switch {
	case cfg.Store == nil:
		persistNote = "no checkpoint store configured; compaction applies to the rest of this turn only"
	case turn.chatSessionID == "":
		persistNote = "this channel has no persisted chat history (e.g. Misskey notes); compaction applies to the rest of this turn only"
	default:
		prev, perr := cfg.Store.LatestContextCheckpointBoundary(turn.botID, turn.chatSessionID)
		if perr != nil {
			logger.Warnw("context_compact: load previous checkpoint failed", "err", perr, "chat_session", turn.chatSessionID)
		}
		boundaryID = compactedHistoryBoundary(snapshot, boundary, turn.baseMessages, turn.historyIDs)
		if prev > boundaryID {
			boundaryID = prev
		}
		serr := cfg.Store.SaveContextCheckpoint(ContextCheckpoint{
			BotID:             turn.botID,
			SessionID:         turn.chatSessionID,
			BoundaryMessageID: boundaryID,
			Summary:           summary,
			Focus:             args.Focus,
			CompactedMessages: boundary,
			TokensBefore:      tokensBefore,
			TokensAfter:       tokensAfter,
			Source:            CompactContextToolName,
		})
		if serr != nil {
			logger.Warnw("context_compact: save checkpoint failed", "err", serr, "chat_session", turn.chatSessionID)
			persistNote = "checkpoint could not be saved; compaction applies to the rest of this turn only"
		} else {
			persisted = true
			persistNote = "later turns will load this summary plus the messages after it"
		}
	}

	logger.Infow("context_compact",
		"tool", CompactContextToolName, "status", "compacted",
		"bot_id", turn.botID, "chat_session", turn.chatSessionID, "source", turn.source,
		"messages_before", len(snapshot), "messages_after", messagesAfter,
		"compacted_messages", boundary, "kept_recent", len(kept),
		"est_tokens_before", tokensBefore, "est_tokens_after", tokensAfter,
		"persisted", persisted, "boundary_message_id", boundaryID,
		"focus_len", utf8.RuneCountInString(args.Focus), "summary_len", utf8.RuneCountInString(summary))

	return map[string]any{
		"status":             "compacted",
		"messages_before":    len(snapshot),
		"messages_after":     messagesAfter,
		"compacted_messages": boundary,
		"kept_recent":        len(kept),
		"est_tokens_before":  tokensBefore,
		"est_tokens_after":   tokensAfter,
		"persisted":          persisted,
		"persist_note":       persistNote,
		"summary_preview":    previewRunes(summary, selfCompactPreviewRunes),
		"note":               "Older context is replaced by the summary from your next step on (token counts are estimates for the message list only, excluding system prompt and tool schemas). Continue the conversation normally; do not call compact_context again this turn.",
	}, nil
}

// compactedHistoryBoundary maps a live-message boundary back to the highest
// chat_messages.id covered by the compacted head. Only the prefix of the live
// snapshot that still matches the MessageBuilder output is trusted (automatic
// compaction earlier in the turn could have rewritten it); anything else
// yields a conservative (lower) boundary, which can duplicate but never lose
// context.
func compactedHistoryBoundary(snapshot []llm.Message, boundary int, base []llm.Message, ids []uint64) uint64 {
	var maxID uint64
	n := boundary
	if n > len(ids) {
		n = len(ids)
	}
	if n > len(base) {
		n = len(base)
	}
	if n > len(snapshot) {
		n = len(snapshot)
	}
	for i := 0; i < n; i++ {
		if snapshot[i].Role != base[i].Role || llm.TextFromParts(snapshot[i].Content) != llm.TextFromParts(base[i].Content) {
			break
		}
		if ids[i] > maxID {
			maxID = ids[i]
		}
	}
	return maxID
}

func previewRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
