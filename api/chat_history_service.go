package api

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// ChatHistoryService — 聊天记录持久化 + 游标分页查询
//
// 使用游标分页（Cursor-based Pagination）替代传统的 OFFSET 分页：
//   - OFFSET 在 SQLite/MySQL 中扫描并丢弃前 N 行，O(N) 复杂度
//   - 游标分页用 WHERE 条件直接定位，O(log N + page_size)，与页码无关
//   - 数据变动时游标锚点不变，无偏移问题
//
// 游标格式：base64.RawURLEncoding("{unix_nano}_{id}")
// ============================================================================

// defaultPageSize 默认每页消息数。
const defaultPageSize = 20

// maxPageSize 单页最大消息数。
const maxPageSize = 100

// HistoryPage 分页查询结果。
type HistoryPage struct {
	Messages   []dao.ChatMessage `json:"messages"`
	NextCursor string            `json:"nextCursor,omitempty"`
	HasMore    bool              `json:"hasMore"`
}

// ChatHistoryService 聊天历史服务。
type ChatHistoryService struct {
	db     *gorm.DB
	logger *zap.SugaredLogger
}

// NewChatHistoryService 创建聊天历史服务。
func NewChatHistoryService(db *gorm.DB, logger *zap.SugaredLogger) *ChatHistoryService {
	return &ChatHistoryService{
		db:     db,
		logger: logger.With("component", "chat_history"),
	}
}

// SaveMessage 保存一条聊天消息。
func (s *ChatHistoryService) SaveMessage(botID, userID, role, content, traceID string) error {
	msg := dao.ChatMessage{
		BotID:     botID,
		UserID:    userID,
		Role:      role,
		Content:   content,
		TraceID:   traceID,
		CreatedAt: time.Now(),
	}
	if err := s.db.Create(&msg).Error; err != nil {
		return fmt.Errorf("chat_history: save message: %w", err)
	}
	return nil
}

// PaginateHistory 游标分页查询聊天历史（向更旧的方向翻页）。
//
// cursor 为空时返回最新的消息。返回的消息按时间倒序（最新在前）。
// 使用 WHERE 条件替代 OFFSET，时间复杂度 O(log N + limit)。
func (s *ChatHistoryService) PaginateHistory(botID, userID, cursor string, limit int) (*HistoryPage, error) {
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	q := s.db.Model(&dao.ChatMessage{}).
		Where("bot_id = ? AND user_id = ?", botID, userID)

	if cursor != "" {
		ts, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, fmt.Errorf("chat_history: invalid cursor: %w", err)
		}
		// 元组比较语义：(created_at, id) < (ts, id)
		// SQLite 不支持元组比较，用 OR 表达式实现
		q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", ts, ts, id)
	}

	var messages []dao.ChatMessage
	if err := q.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&messages).Error; err != nil {
		return nil, fmt.Errorf("chat_history: query messages: %w", err)
	}

	page := &HistoryPage{
		Messages: messages,
		HasMore:  false,
	}

	// 多取一条判断 hasMore
	if len(messages) > limit {
		page.HasMore = true
		page.Messages = messages[:limit]
		last := page.Messages[limit-1]
		page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	}

	return page, nil
}

// LoadContext 加载最近 N 条消息作为 LLM 上下文。
// 返回的消息按时间正序（旧→新），直接拼入 LLM messages。
func (s *ChatHistoryService) LoadContext(botID, userID string, limit int) ([]dao.ChatMessage, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	var messages []dao.ChatMessage
	// 先按 DESC 取最近 limit 条，再反转为正序
	if err := s.db.Model(&dao.ChatMessage{}).
		Where("bot_id = ? AND user_id = ?", botID, userID).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&messages).Error; err != nil {
		return nil, fmt.Errorf("chat_history: load context: %w", err)
	}

	// 反转为正序（旧→新）
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}

	return messages, nil
}

// --- 游标编解码 ---

// encodeCursor 将时间戳和 ID 编码为游标字符串。
func encodeCursor(t time.Time, id uint64) string {
	raw := fmt.Sprintf("%d_%d", t.UnixNano(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor 将游标字符串解码为时间戳和 ID。
func decodeCursor(cursor string) (time.Time, uint64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, 0, err
	}
	parts := strings.SplitN(string(raw), "_", 2)
	if len(parts) != 2 {
		return time.Time{}, 0, fmt.Errorf("malformed cursor")
	}
	ns, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("invalid timestamp in cursor: %w", err)
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("invalid id in cursor: %w", err)
	}
	return time.Unix(0, ns), id, nil
}
