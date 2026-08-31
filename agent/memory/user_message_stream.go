package memory

import (
	"context"
	"strconv"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

// dbUserMessageSource 是 UserMessageSource 的生产实现，查询 user_message_events 表。
type dbUserMessageSource struct {
	db *gorm.DB
}

// NewDBUserMessageSource 创建基于 user_message_events 表的事件流数据源。
func NewDBUserMessageSource(db *gorm.DB) UserMessageSource {
	return &dbUserMessageSource{db: db}
}

// LoadSince 返回 botID 下 id > sinceID 的事件流消息（按 id 升序）。
func (s *dbUserMessageSource) LoadSince(ctx context.Context, botID string, sinceID uint64) ([]BackfillMessage, error) {
	var rows []dao.UserMessageEvent
	if err := s.db.WithContext(ctx).
		Where("bot_id = ? AND id > ?", botID, sinceID).
		Order("id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]BackfillMessage, 0, len(rows))
	for _, r := range rows {
		out = append(out, BackfillMessage{
			ID:        r.ID,
			Content:   r.Content,
			CreatedAt: r.CreatedAt,
			Channel:   r.Channel,
			UserID:    r.UserID,
			MessageID: r.MessageID,
		})
	}
	return out, nil
}

// SeedUserMessageEvents 从历史 chat_messages 一次性幂等补齐 user_message_events。
//
// 这是「订阅事件流去扫 chat_messages」迁移的唯一一次 chat_messages 读取：
// 仅当该 bot 的事件流为空时才执行；补齐后运行期由 NoteCaptureMiddleware 直接写入事件流，
// 后续启动不再触碰 chat_messages（guard：事件流非空即跳过）。按 message_id 去重，
// 重复调用安全。
//
// 返回补齐条数；事件流已非空时返回 (0, nil)。
func SeedUserMessageEvents(ctx context.Context, db *gorm.DB, botID string, logger *zap.SugaredLogger) (int, error) {
	if db == nil {
		return 0, nil
	}
	// guard：事件流已非空 → 跳过（根治回灌陷阱的关键：不再周期性扫 chat_messages）。
	var existing int64
	if err := db.WithContext(ctx).Model(&dao.UserMessageEvent{}).
		Where("bot_id = ?", botID).Count(&existing).Error; err != nil {
		return 0, err
	}
	if existing > 0 {
		return 0, nil
	}

	// 取该 bot 的全部 user 角色历史消息。
	var history []dao.ChatMessage
	if err := db.WithContext(ctx).
		Where("bot_id = ? AND role = ?", botID, dao.ChatRoleUser).
		Order("id ASC").
		Find(&history).Error; err != nil {
		return 0, err
	}
	if len(history) == 0 {
		return 0, nil
	}

	// 已存在的 message_id 集合（去重，幂等）。
	existingIDs := make(map[string]struct{})
	var prev []dao.UserMessageEvent
	if err := db.WithContext(ctx).
		Where("bot_id = ?", botID).
		Find(&prev).Error; err != nil {
		return 0, err
	}
	for _, p := range prev {
		existingIDs[p.MessageID] = struct{}{}
	}

	batch := make([]dao.UserMessageEvent, 0, len(history))
	seeded := 0
	for _, m := range history {
		if m.Content == "" {
			continue
		}
		mid := uint64ToStr(m.ID)
		if _, dup := existingIDs[mid]; dup {
			continue
		}
		existingIDs[mid] = struct{}{}
		batch = append(batch, dao.UserMessageEvent{
			BotID:     m.BotID,
			Channel:   m.SessionID, // 用会话 ID 作为记忆关联 channel（与历史 backfill 的 ChannelScope 一致）
			UserID:    m.UserID,
			MessageID: mid,
			Content:   m.Content,
			CreatedAt: m.CreatedAt,
		})
	}
	if len(batch) == 0 {
		return 0, nil
	}
	// 分批写入，避免超大事务。
	const batchSize = 500
	for start := 0; start < len(batch); start += batchSize {
		end := start + batchSize
		if end > len(batch) {
			end = len(batch)
		}
		if err := db.WithContext(ctx).Create(batch[start:end]).Error; err != nil {
			return seeded, err
		}
		seeded += end - start
	}
	if logger != nil {
		logger.Infow("memory event stream seeded from chat history",
			"bot_id", botID, "seeded", seeded, "source_messages", len(history))
	}
	return seeded, nil
}

// uint64ToStr 将自增主键转为 message_id 字符串（去重用）。
func uint64ToStr(v uint64) string {
	return strconv.FormatUint(v, 10)
}
