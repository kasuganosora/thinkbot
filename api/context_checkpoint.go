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
// ============================================================================

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
// and a synthetic summary entry is prepended. On any error the history is
// returned unchanged (fail-open to the previous behaviour).
func (s *ChatHistoryService) ApplyContextCheckpoint(botID, sessionID string, history []dao.ChatMessage) []dao.ChatMessage {
	if s == nil || sessionID == "" {
		return history
	}
	cp, err := s.LatestContextCheckpoint(botID, sessionID)
	if err != nil {
		s.logger.Warnw("context checkpoint lookup failed, using raw history", "err", err, "session", sessionID)
		return history
	}
	return applyCheckpointToHistory(cp, history)
}

// applyCheckpointToHistory is the pure part of ApplyContextCheckpoint.
func applyCheckpointToHistory(cp *dao.ContextCheckpoint, history []dao.ChatMessage) []dao.ChatMessage {
	if cp == nil || cp.Summary == "" {
		return history
	}
	out := make([]dao.ChatMessage, 0, len(history)+1)
	out = append(out, dao.ChatMessage{
		BotID:     cp.BotID,
		SessionID: cp.SessionID,
		Role:      dao.ChatRoleContextSummary,
		Content:   cp.Summary,
		CreatedAt: cp.CreatedAt,
	})
	for _, m := range history {
		if m.ID != 0 && m.ID <= cp.BoundaryMessageID {
			continue
		}
		out = append(out, m)
	}
	return out
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
