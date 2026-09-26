package api

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/stages"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Context checkpoints — persistence for the compact_context tool
//
// A checkpoint says: for (bot, session), chat_messages rows with
// id <= boundary are represented by a summary when building LLM context.
// Rows are never deleted (UI history / audit unaffected); deactivating the
// checkpoint restores the full history:
//
//	UPDATE context_checkpoints SET active = 0 WHERE bot_id = ? AND session_id = ?;
//
// Expiry (decided at load time; rows are never mutated): an active checkpoint
// is ignored when
//   - it is older than the TTL (agent.self_compact.checkpoint_ttl, seconds;
//     default 24h, negative disables the TTL), or
//   - none of the rows it covers is in the loaded history window any more —
//     without the checkpoint those rows would have scrolled out of the
//     normal chat_context_limit window as well, so the summary would only
//     keep a stale topic alive. With the default window of 20 rows this
//     ends injection after ~10 newer exchanges.
// ============================================================================

// defaultContextCheckpointTTL is used when no TTL source is configured.
const defaultContextCheckpointTTL = 24 * time.Hour

// checkpointTTLSource returns the checkpoint TTL (<= 0 → no TTL).
type checkpointTTLSource func() time.Duration

var checkpointNow = time.Now // test hook

// SetContextCheckpointTTLSource installs a live TTL source (read on every
// history load, so config changes apply without restart). fn returning <= 0
// disables the TTL; nil restores the 24h default.
func (s *ChatHistoryService) SetContextCheckpointTTLSource(fn func() time.Duration) {
	if s == nil {
		return
	}
	if fn == nil {
		s.checkpointTTL.Store(nil)
		return
	}
	src := checkpointTTLSource(fn)
	s.checkpointTTL.Store(&src)
}

func (s *ChatHistoryService) contextCheckpointTTL() time.Duration {
	if p := s.checkpointTTL.Load(); p != nil && *p != nil {
		return (*p)()
	}
	return defaultContextCheckpointTTL
}

// ChatHistoryService implements stages.ContextCheckpointStore.
var _ stages.ContextCheckpointStore = (*ChatHistoryService)(nil)

// LatestContextCheckpoint returns the active checkpoint for a session (nil when none).
func (s *ChatHistoryService) LatestContextCheckpoint(botID, sessionID string) (*dao.ContextCheckpoint, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return nil, nil
	}
	var cp dao.ContextCheckpoint
	err := s.db.Where("bot_id = ? AND session_id = ? AND active = ?", botID, sessionID, true).
		Order("id DESC").Limit(1).Take(&cp).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("chat_history: load context checkpoint: %w", err)
	}
	return &cp, nil
}

// LatestContextCheckpointBoundary implements stages.ContextCheckpointStore.
func (s *ChatHistoryService) LatestContextCheckpointBoundary(botID, sessionID string) (uint64, error) {
	cp, err := s.LatestContextCheckpoint(botID, sessionID)
	if err != nil || cp == nil {
		return 0, err
	}
	return cp.BoundaryMessageID, nil
}

// LastContextCheckpointAt implements stages.ContextCheckpointStore: creation
// time of the newest checkpoint of the session, active or not (zero when
// none). compact_context derives its cooldown from it so a restart does not
// reset the cooldown.
func (s *ChatHistoryService) LastContextCheckpointAt(botID, sessionID string) (time.Time, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return time.Time{}, nil
	}
	var cp dao.ContextCheckpoint
	err := s.db.Select("id", "created_at").Where("bot_id = ? AND session_id = ?", botID, sessionID).
		Order("id DESC").Limit(1).Take(&cp).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("chat_history: load last context checkpoint: %w", err)
	}
	return cp.CreatedAt, nil
}

// SaveContextCheckpoint implements stages.ContextCheckpointStore: it stores a
// new active checkpoint and deactivates older ones of the same session (kept
// for audit).
func (s *ChatHistoryService) SaveContextCheckpoint(in stages.ContextCheckpoint) error {
	if s == nil || s.db == nil {
		return errors.New("chat_history: no database")
	}
	if in.BotID == "" || in.SessionID == "" {
		return errors.New("chat_history: checkpoint requires bot and session")
	}
	cp := dao.ContextCheckpoint{
		BotID:             in.BotID,
		SessionID:         in.SessionID,
		BoundaryMessageID: in.BoundaryMessageID,
		Summary:           in.Summary,
		Focus:             in.Focus,
		CompactedMessages: in.CompactedMessages,
		TokensBefore:      in.TokensBefore,
		TokensAfter:       in.TokensAfter,
		Source:            in.Source,
		Active:            true,
		CreatedAt:         time.Now(),
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&dao.ContextCheckpoint{}).
			Where("bot_id = ? AND session_id = ? AND active = ?", in.BotID, in.SessionID, true).
			Update("active", false).Error; err != nil {
			return fmt.Errorf("chat_history: deactivate old checkpoints: %w", err)
		}
		if err := tx.Create(&cp).Error; err != nil {
			return fmt.Errorf("chat_history: save context checkpoint: %w", err)
		}
		return nil
	})
}

// ApplyContextCheckpoint rewrites a loaded LLM-context history window using
// the session's active checkpoint: rows covered by the checkpoint are dropped
// and a synthetic summary entry is prepended, unless the checkpoint has
// expired (see the package comment). Every decision is logged with the trace
// ID, so it can be verified from logs which turn used which checkpoint. On
// any error the history is returned unchanged (fail-open to the previous
// behaviour).
func (s *ChatHistoryService) ApplyContextCheckpoint(traceID, botID, sessionID string, history []dao.ChatMessage) []dao.ChatMessage {
	if s == nil || sessionID == "" {
		return history
	}
	cp, err := s.LatestContextCheckpoint(botID, sessionID)
	if err != nil {
		s.logger.Warnw("context checkpoint lookup failed, using raw history", "err", err, "session", sessionID, "trace_id", traceID)
		return history
	}
	if cp == nil || cp.Summary == "" {
		return history
	}
	now := checkpointNow()
	out, d := applyCheckpointToHistory(cp, history, now, s.contextCheckpointTTL())
	fields := []any{
		"trace_id", traceID, "bot_id", botID, "session", sessionID,
		"checkpoint_id", cp.ID, "boundary_message_id", cp.BoundaryMessageID,
		"checkpoint_age", now.Sub(cp.CreatedAt).Round(time.Second).String(),
		"history_rows", len(history),
	}
	if !d.applied {
		s.logger.Infow("context checkpoint skipped", append(fields, "reason", d.reason)...)
		return history
	}
	s.logger.Infow("context checkpoint applied", append(fields,
		"rows_dropped", d.dropped, "rows_kept", len(out)-1,
		"summary_tokens", llm.EstimateTokens(cp.Summary), "summary_len", len([]rune(cp.Summary)))...)
	return out
}

// checkpointDecision explains what applyCheckpointToHistory did.
type checkpointDecision struct {
	applied bool
	reason  string // why it was skipped
	dropped int    // history rows replaced by the summary
}

// applyCheckpointToHistory is the pure part of ApplyContextCheckpoint.
// ttl <= 0 disables the age check.
func applyCheckpointToHistory(cp *dao.ContextCheckpoint, history []dao.ChatMessage, now time.Time, ttl time.Duration) ([]dao.ChatMessage, checkpointDecision) {
	if cp == nil || cp.Summary == "" {
		return history, checkpointDecision{reason: "no checkpoint"}
	}
	if ttl > 0 && !cp.CreatedAt.IsZero() && now.Sub(cp.CreatedAt) > ttl {
		return history, checkpointDecision{reason: fmt.Sprintf("expired: older than ttl %s", ttl)}
	}
	dropped := 0
	var lastCovered time.Time
	for _, m := range history {
		if m.ID != 0 && m.ID <= cp.BoundaryMessageID {
			dropped++
			if m.CreatedAt.After(lastCovered) {
				lastCovered = m.CreatedAt
			}
		}
	}
	if dropped == 0 {
		return history, checkpointDecision{reason: "covered messages are already outside the history window"}
	}
	out := make([]dao.ChatMessage, 0, len(history)-dropped+1)
	out = append(out, dao.ChatMessage{
		BotID:     cp.BotID,
		SessionID: cp.SessionID,
		Role:      dao.ChatRoleContextSummary,
		Content:   checkpointSummaryContent(cp, lastCovered),
		CreatedAt: cp.CreatedAt,
	})
	for _, m := range history {
		if m.ID != 0 && m.ID <= cp.BoundaryMessageID {
			continue
		}
		out = append(out, m)
	}
	return out, checkpointDecision{applied: true, dropped: dropped}
}

// checkpointSummaryContent prefixes the stored summary with when it was
// written and which period it covers, and reminds the model that it is a
// lossy note: newer messages win, unverified items are not facts.
func checkpointSummaryContent(cp *dao.ContextCheckpoint, lastCovered time.Time) string {
	const layout = "2006-01-02 15:04 MST"
	note := "Note about earlier messages of this conversation"
	if !lastCovered.IsZero() {
		note += " (up to " + lastCovered.Format(layout) + ")"
	}
	if !cp.CreatedAt.IsZero() {
		note += ", written " + cp.CreatedAt.Format(layout)
	}
	note += ". It is a lossy summary and may be outdated: newer messages take precedence, and anything marked unverified is not confirmed."
	return note + "\n\n" + cp.Summary
}

// chatHistoryToLLM converts a loaded history window into LLM messages and the
// aligned chat_messages IDs (0 for synthetic entries). It is the single source
// of truth for both the MessageBuilder and compact_context's boundary mapping.
func chatHistoryToLLM(msgs []dao.ChatMessage) ([]llm.Message, []uint64) {
	out := make([]llm.Message, 0, len(msgs))
	ids := make([]uint64, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case dao.ChatRoleUser:
			out = append(out, llm.UserMessage(m.Content))
		case dao.ChatRoleAssistant:
			out = append(out, llm.AssistantMessage(m.Content))
		case dao.ChatRoleContextSummary:
			out = append(out, llm.ConversationSummaryMessage(m.Content))
		case dao.ChatRoleNotify:
			// notify 接口写入的系统备注（外部通知要点）：以 system 消息进入上下文，
			// 与普通消息一样参与 ids 对齐（compact_context 边界映射依赖逐条对应）。
			out = append(out, llm.SystemMessage(m.Content))
		default:
			continue
		}
		ids = append(ids, m.ID)
	}
	return out, ids
}

// historyFromMetadata extracts the history window injected into msg metadata.
func historyFromMetadata(msg core.Message) []dao.ChatMessage {
	if msg.Metadata == nil {
		return nil
	}
	if msgs, ok := msg.Metadata["chat_history"].([]dao.ChatMessage); ok {
		return msgs
	}
	return nil
}

// chatHistoryMessageIDs implements stages.SelfCompactConfig.HistoryMessageIDs.
func chatHistoryMessageIDs(msg core.Message) []uint64 {
	_, ids := chatHistoryToLLM(historyFromMetadata(msg))
	return ids
}
