package memory

import (
	"context"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

// BackfillFromChatHistory 将历史对话（chat_messages）灌入工作记忆 L0，
// 使 dreaming 能处理此前从未进入记忆系统的历史 backlog。
//
// 背景：L0 工作记忆原本只由实时聊天产生的 ActionNote 写入（见 note_capture.go /
// note_adapter.go）。历史 chat_messages 一直停留在独立的聊天日志表，从未进入记忆
// 系统，导致 dreaming 每天 03:00 面对的 L0 永远是空的、ingested 恒为 0。
// 此函数把历史对话补灌进 L0，补齐这个 backlog。
//
// 写入细节：
//   - 每条聊天消息作为一个 L0 条目写入 store（生产为 memStore：
//     MultiStore → TieredStoreAdapter → TieredStore L0 层，write-through 到 tiered_memories）。
//   - scope：优先按会话 ChannelScope(session_id)，无会话则按用户 UserScope(user_id)。
//   - CreatedAt 设为当前时间——必须如此，否则以原始聊天时间（可能数天前）写入会被
//     dreaming 的 LookbackDays / ActiveThresholdHours 门槛判为过期而跳过。
//   - Source="chat_history"，并在 Metadata 记录原始 chat_message_id / role / 原始时间，便于追溯。
//
// 调用方负责幂等（例如仅在分层记忆为空时调用），避免每次进程启动重复灌入。
//
// 返回实际写入的条目数（已按 botID 过滤，只处理当前 bot 的历史对话）。
func BackfillFromChatHistory(ctx context.Context, store Store, db *gorm.DB, botID string, logger *zap.SugaredLogger) (int, error) {
	if store == nil || db == nil {
		return 0, nil
	}
	var msgs []dao.ChatMessage
	if err := db.Where("bot_id = ?", botID).Order("created_at ASC").Find(&msgs).Error; err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}

	now := time.Now()
	written := 0
	for _, m := range msgs {
		content := m.Content
		if content == "" {
			continue
		}

		var scope Scope
		if m.SessionID != "" {
			scope = ChannelScope(m.SessionID)
		} else {
			scope = UserScope(m.UserID)
		}

		entry := Entry{
			Scope:    scope,
			Content:  content,
			Category: "conversation",
			Source:   "chat_history",
			Metadata: map[string]any{
				"chat_message_id":    m.ID,
				"role":               m.Role,
				"original_created_at": m.CreatedAt.Format(time.RFC3339),
			},
			CreatedAt:     now,
			LastAccessedAt: now,
		}
		if err := store.Append(ctx, entry); err != nil {
			if logger != nil {
				logger.Warnw("backfill: append failed", "err", err, "chat_message_id", m.ID)
			}
			continue
		}
		written++
	}

	if logger != nil {
		logger.Infow("memory backfill from chat history",
			"bot_id", botID, "messages", len(msgs), "written", written)
	}
	return written, nil
}
