package memory

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// BackfillMessage 是回灌所需的单条入站用户消息（已从事件流归一化）。
type BackfillMessage struct {
	ID        uint64
	Content   string
	CreatedAt time.Time
	// Channel 会话空间标识（记忆关联）。非空时作为 ChannelScope，否则退回 UserScope。
	Channel string
	// UserID 用户标识（Channel 为空时的 scope 兜底）。
	UserID string
	// MessageID 原始消息 ID（追溯用）。
	MessageID string
}

// UserMessageSource 抽象「入站用户消息事件流」，backfill 消费它而非直接扫 chat_messages。
// 生产实现查询 user_message_events 表（见 NewDBUserMessageSource）；测试可用内存实现。
// 这是 deepseek-harness append-only trajectory 思想在 thinkbot 的落地：记忆回灌的权威
// 数据源是系统自身发出的「用户说了什么」事件，而非原始渠道存储表，从而与渠道解耦、
// 根治「清空 tiered/memory 表后从 chat_messages 回潮」的陷阱。
type UserMessageSource interface {
	// LoadSince 返回 botID 下 id > sinceID 的事件流消息（按 id 升序）。
	LoadSince(ctx context.Context, botID string, sinceID uint64) ([]BackfillMessage, error)
}

// BackfillFromChatHistory 将事件流中尚未处理的入站用户消息灌入工作记忆 L0，
// 使 dreaming 能处理此前从未进入记忆系统的历史 backlog。
//
// 背景：L0 工作记忆原本只由实时聊天产生的 ActionNote 写入（见 note_capture.go /
// note_adapter.go）。历史消息一直停留在独立的聊天日志表，从未进入记忆系统，导致
// dreaming 每天 03:00 面对的 L0 永远是空的、ingested 恒为 0。此函数把事件流中
// 的历史消息补灌进 L0，补齐这个 backlog。
//
// 增量幂等（sinceID）：只处理 id > sinceID 的事件流消息。调用方应把已补灌的最大 id
// 作为水位线持久化（见 config.BotMemoryBackfillWatermarkKey），下次传入该水位线即可
// 跳过已处理消息。这同时做到：① 进程启动重启不会重复灌入；② 清空 tiered/memory 表后，
// 只要事件流与水位线仍在，就不会从历史消息回潮（根治回灌陷阱）。
//
// 写入细节：
//   - 每条消息作为一个 L0 条目写入 store（生产为 memStore：
//     MultiStore → TieredStoreAdapter → TieredStore L0 层，write-through 到 tiered_memories）。
//   - scope：优先按 Channel（ChannelScope），无则按 UserID（UserScope）。
//   - CreatedAt 设为当前时间——必须如此，否则以原始时间（可能数天前）写入会被
//     dreaming 的 LookbackDays / ActiveThresholdHours 门槛判为过期而跳过。
//   - Source="chat_history"，并在 Metadata 记录原始事件 id / message_id，便于追溯。
//
// 返回 (写入条目数, 已处理消息的最大 id, error)。maxID 始终 >= sinceID，便于调用方
// 直接持久化为下一轮水位线。
func BackfillFromChatHistory(ctx context.Context, store Store, src UserMessageSource, botID string, sinceID uint64, logger *zap.SugaredLogger) (written int, maxID uint64, err error) {
	if store == nil || src == nil {
		return 0, sinceID, nil
	}
	msgs, err := src.LoadSince(ctx, botID, sinceID)
	if err != nil {
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
		if m.Channel != "" {
			scope = ChannelScope(m.Channel)
		} else {
			scope = UserScope(m.UserID)
		}

		entry := Entry{
			Scope:    scope,
			Content:  content,
			Category: "conversation",
			Source:   "chat_history",
			Metadata: map[string]any{
				"event_id":   m.ID,
				"message_id": m.MessageID,
			},
			CreatedAt:      now,
			LastAccessedAt: now,
		}
		if err := store.Append(ctx, entry); err != nil {
			if logger != nil {
				logger.Warnw("backfill: append failed", "err", err, "event_id", m.ID)
			}
			continue
		}
		written++
	}

	if logger != nil {
		logger.Infow("memory backfill from event stream",
			"bot_id", botID, "since_id", sinceID, "messages", len(msgs), "written", written, "max_id", maxID)
	}
	return written, maxID, nil
}
