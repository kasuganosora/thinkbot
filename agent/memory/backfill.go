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
// 增量幂等（sinceID）：只处理 id > sinceID 的 chat_message。调用方应把已补灌的
// 最大 id 作为水位线持久化（见 config.BotMemoryBackfillWatermarkKey），下次传入该
// 水位线即可跳过已处理消息。这同时做到：① 进程启动重启不会重复灌入；② 清空 tiered/
// memory 表后，只要水位线仍在，就不会从历史 chat_messages 回潮（根治回灌陷阱）。
//
// 写入细节：
//   - 每条聊天消息作为一个 L0 条目写入 store（生产为 memStore：
//     MultiStore → TieredStoreAdapter → TieredStore L0 层，write-through 到 tiered_memories）。
//   - scope：优先按会话 ChannelScope(session_id)，无会话则按用户 UserScope(user_id)。
//   - CreatedAt 设为当前时间——必须如此，否则以原始聊天时间（可能数天前）写入会被
//     dreaming 的 LookbackDays / ActiveThresholdHours 门槛判为过期而跳过。
//   - Source="chat_history"，并在 Metadata 记录原始 chat_message_id / role / 原始时间，便于追溯。
//
// 返回 (写入条目数, 已处理消息的最大 id, error)。maxID 始终 >= sinceID，便于调用方
// 直接持久化为下一轮水位线。
func BackfillFromChatHistory(ctx context.Context, store Store, db *gorm.DB, botID string, sinceID uint64, logger *zap.SugaredLogger) (written int, maxID uint64, err error) {
	if store == nil || db == nil {
		return 0, sinceID, nil
	}
	var msgs []dao.ChatMessage
	if err := db.Where("bot_id = ? AND id > ?", botID, sinceID).Order("created_at ASC").Find(&msgs).Error; err != nil {
		return 0, sinceID, err
	}
	if len(msgs) == 0 {
		return 0, sinceID, nil
	}

	now := time.Now()
	written = 0
	maxID = sinceID
	for _, m := range msgs {
		if m.ID > maxID {
			maxID = m.ID
		}
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
				"chat_message_id":     m.ID,
				"role":                m.Role,
				"original_created_at": m.CreatedAt.Format(time.RFC3339),
			},
			CreatedAt:      now,
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
			"bot_id", botID, "since_id", sinceID, "messages", len(msgs), "written", written, "max_id", maxID)
	}
	return written, maxID, nil
}
