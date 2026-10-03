package api

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/stages"
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
	// checkpointTTL is the live context-checkpoint TTL source (see
	// context_checkpoint.go); nil → default.
	checkpointTTL atomic.Pointer[checkpointTTLSource]
}

// NewChatHistoryService 创建聊天历史服务。
//
// 注意：不要在此处访问数据库。依赖注入阶段 dao.Migrate 尚未执行，表/列可能还不存在
// （曾因此让启动清理拿到 "no such column: streaming" 而静默失效）。需要读写 DB 的
// 初始化逻辑请放到 OnStart 钩子里，见 newChatHistoryService。
func NewChatHistoryService(db *gorm.DB, logger *zap.SugaredLogger) *ChatHistoryService {
	return &ChatHistoryService{
		db:     db,
		logger: logger.With("component", "chat_history"),
	}
}

// SaveMessage 保存一条聊天消息（无工具调用信息）。
func (s *ChatHistoryService) SaveMessage(botID, userID, role, content, traceID, sessionID string) error {
	return s.SaveMessageWithTools(botID, userID, role, content, traceID, "", sessionID)
}

// SaveMessageWithTools 保存一条聊天消息，并附带工具调用信息（JSON 字符串）。
// toolCallsJSON 为空时等价于 SaveMessage。仅 assistant 消息通常带工具调用。
func (s *ChatHistoryService) SaveMessageWithTools(botID, userID, role, content, traceID, toolCallsJSON, sessionID string) error {
	return s.SaveMessageWithParts(botID, userID, role, content, traceID, toolCallsJSON, "", sessionID)
}

// resolveWritableSession 保证落库用的 session_id 非空且指向真实会话行（B32）。
//
// 此前 sessionID 完全依赖调用方（Web 前端）传参，为空就直接落一条 session_id 为空的
// 消息 —— 这类行在 Web 上既列不出也查不到（孤儿），只能靠一次性迁移收编，而
// 迁移只跑一次，**新产生的空 session 会持续堆积**。这里在写入侧兜底。
//
// 兜底键取 `web:default:<userID>` 而不是「找该 bot 最近一个会话」：前者可去重，
// 同一用户反复触发只会收敛到同一行；后者在并发下会造出多个「默认会话」。
//
// 解析失败时返回空串（保持原行为）而不是报错：落库失败会连带丢掉 Bot 的上下文
// 历史，比多一条孤儿严重。宁可孤儿，不可丢消息。
func (s *ChatHistoryService) resolveWritableSession(botID, userID, sessionID string) string {
	if sessionID != "" {
		return sessionID
	}
	if s == nil || s.db == nil || botID == "" {
		return ""
	}
	sess, err := dao.ResolveSession(s.db, botID, dao.WebDefaultKey(userID), dao.KindWeb, "默认会话")
	if err != nil {
		if s.logger != nil {
			s.logger.Warnw("fallback session resolve failed, message will be orphan",
				"bot", botID, "user", userID, "err", err)
		}
		return ""
	}
	return strconv.FormatUint(sess.ID, 10)
}

// SaveMessageWithParts 保存一条聊天消息，含工具调用 + 有序 parts。
// partsJSON 为空时等价于 SaveMessageWithTools。parts 保留 LLM 输出的文本/工具交错顺序。
func (s *ChatHistoryService) SaveMessageWithParts(botID, userID, role, content, traceID, toolCallsJSON, partsJSON, sessionID string) error {
	sessionID = s.resolveWritableSession(botID, userID, sessionID)
	msg := dao.ChatMessage{
		BotID:     botID,
		UserID:    userID,
		SessionID: sessionID,
		Role:      role,
		Content:   content,
		ToolCalls: toolCallsJSON,
		PartsJSON: partsJSON,
		TraceID:   traceID,
		CreatedAt: time.Now(),
	}
	if err := s.db.Create(&msg).Error; err != nil {
		return fmt.Errorf("chat_history: save message: %w", err)
	}
	s.touchSessionAfterSave(sessionID, role, content)
	return nil
}

// UpsertAssistantByTrace 按 traceID 写入或更新一条 assistant 消息。
//
// 用途：流式回复的**增量落库**。一轮对话的 traceID 全程唯一且稳定，因此可作幂等键——
// 回复过程中可以反复调用本方法刷新同一行，用户中途刷新页面也能读到已产出的内容，
// 而不必等整轮结束。旧行为（只在收尾 Insert 一次）会让中途刷新丢失全部流式内容。
//
// streaming 表示本轮是否仍在产出：中间态传 true，收尾时传 false。前端据此区分
// 「工具真的还在跑」与「进程中途死了但状态没来得及更新」。
//
// 语义：
//   - traceID 对应的行不存在 → 插入（CreatedAt 取当前时间，保证历史排序正确）。
//   - 已存在 → 只更新 content / tool_calls / parts_json / streaming，**不改 created_at**，
//     否则同一条消息会在按时间排序的历史里不断跳到末尾。
//
// traceID 为空时退化为普通 Insert（无幂等键可用）。
func (s *ChatHistoryService) UpsertAssistantByTrace(botID, userID, content, traceID, toolCallsJSON, partsJSON, sessionID string, streaming bool) error {
	if traceID == "" {
		return s.SaveMessageWithParts(botID, userID, dao.ChatRoleAssistant, content, traceID, toolCallsJSON, partsJSON, sessionID)
	}

	// 先尝试更新已有行：命中则结束，避免 Insert 冲突。
	res := s.db.Model(&dao.ChatMessage{}).
		Where("trace_id = ? AND role = ?", traceID, dao.ChatRoleAssistant).
		Updates(map[string]any{
			"content":    content,
			"tool_calls": toolCallsJSON,
			"parts_json": partsJSON,
			"streaming":  streaming,
		})
	if res.Error != nil {
		return fmt.Errorf("chat_history: upsert assistant (update): %w", res.Error)
	}
	if res.RowsAffected > 0 {
		return nil
	}

	// 无既有行 → 插入。
	//
	// 注意并发：同一 traceID 只由单个请求处理循环写入，不存在多写者竞争。
	// 即便极端情况下重复插入，也仅表现为多一条历史，不会破坏数据一致性。
	sessionID = s.resolveWritableSession(botID, userID, sessionID)
	msg := dao.ChatMessage{
		BotID:     botID,
		UserID:    userID,
		SessionID: sessionID,
		Role:      dao.ChatRoleAssistant,
		Content:   content,
		ToolCalls: toolCallsJSON,
		PartsJSON: partsJSON,
		TraceID:   traceID,
		Streaming: streaming,
		CreatedAt: time.Now(),
	}
	if err := s.db.Create(&msg).Error; err != nil {
		return fmt.Errorf("chat_history: upsert assistant (insert): %w", err)
	}
	s.touchSessionAfterSave(sessionID, dao.ChatRoleAssistant, content)
	return nil
}

// MarkStreamingStale 把所有仍标记为 streaming 的 assistant 消息置为已结束。
//
// 服务重启时调用：进程被中断的那些流式回复不可能再继续产出，若继续标记 streaming=true，
// 前端会把它们的 running 工具卡片渲染成永久转圈。启动时统一收敛，是这类「进程死掉留下
// 中间态」问题唯一可靠的清理时机。
func (s *ChatHistoryService) MarkStreamingStale() (int64, error) {
	res := s.db.Model(&dao.ChatMessage{}).
		Where("streaming = ?", true).
		Update("streaming", false)
	if res.Error != nil {
		return 0, fmt.Errorf("chat_history: mark streaming stale: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// PaginateHistory 游标分页查询聊天历史（向更旧的方向翻页）。
//
// cursor 为空时返回最新的消息。返回的消息按时间倒序（最新在前）。
// 使用 WHERE 条件替代 OFFSET，时间复杂度 O(log N + limit)。
func (s *ChatHistoryService) PaginateHistory(botID, userID, cursor string, limit int, sessionID string) (*HistoryPage, error) {
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	// 系统/续跑等无真实归属用户的消息（user_id='system'）按会话归属，对会话所有者可见：
	// workflow 续跑的指令(user)与 bot 总结(assistant)都以 'system' 落库，否则会被 user_id
	// 过滤掉，导致「刷新后看不到 bot 续跑结果」。单用户部署下 'system' 为保留哨兵，不与
	// 真实数字 user_id 冲突。
	const systemUserID = "system"

	// ⚠️ B24-B：会话内查询**不再按 user_id 过滤**。
	//
	// 渠道消息的 user_id 写的就是渠道会话标识本身（botservice.go 的 `userID := sid`），
	// 实测 tg:76017910 会话里 359 条 user_id 全是 'tg:76017910'，群聊更是全群共享一个值——
	// 它恒不等于登录用户 id，于是整个会话一条都查不出来（「列表有、点进去空」）。
	//
	// 这不是开后门：chat_sessions 目前**没有 owner 字段**，handleListSessions 也只按
	// bot_id 就列出全部会话，系统根本不存在会话级多用户隔离。去掉该条件不新增任何
	// 暴露面，只是拆掉一个「看起来有隔离、实际没有」的假闸门。
	// 附带收益：user_id='system' 哨兵不再跨会话可见，收敛到本会话内。
	q := s.db.Model(&dao.ChatMessage{}).Where("bot_id = ? AND session_id = ?", botID, sessionID)
	if sessionID == "" {
		// 无会话维度的历史查询（session_id 为空的老数据）才回退到按归属用户过滤。
		q = q.Where("user_id = ? OR user_id = ?", userID, systemUserID)
	}

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

// CountMessages 统计指定 Bot 的消息总数（跨所有用户）。
func (s *ChatHistoryService) CountMessages(botID string) (int64, error) {
	var n int64
	if err := s.db.Model(&dao.ChatMessage{}).
		Where("bot_id = ?", botID).
		Count(&n).Error; err != nil {
		return 0, fmt.Errorf("chat_history: count messages: %w", err)
	}
	return n, nil
}

// LoadContext 加载最近 N 条消息作为 LLM 上下文。
// 返回的消息按时间正序（旧→新），直接拼入 LLM messages。
func (s *ChatHistoryService) LoadContext(botID, userID string, limit int, sessionID string) ([]dao.ChatMessage, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	var messages []dao.ChatMessage
	// 先按 DESC 取最近 limit 条，再反转为正序
	if err := s.db.Model(&dao.ChatMessage{}).
		Where("bot_id = ? AND user_id = ? AND session_id = ?", botID, userID, sessionID).
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

// LoadContextBySession 与 LoadContext 类似，但只按 sessionID 过滤（不限定 userID）。
// 用于后端续跑注入场景：续跑是系统行为，没有真实 userID，需按会话取回完整历史作为
// agent 续跑的上下文，否则 agent 会丢失对话背景。
func (s *ChatHistoryService) LoadContextBySession(botID, sessionID string, limit int) ([]dao.ChatMessage, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	var messages []dao.ChatMessage
	if err := s.db.Model(&dao.ChatMessage{}).
		Where("bot_id = ? AND session_id = ?", botID, sessionID).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&messages).Error; err != nil {
		return nil, fmt.Errorf("chat_history: load context by session: %w", err)
	}

	// 反转为正序（旧→新）
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}

// SearchContextHistory 在原始 chat_messages 里按子串搜索，供压缩后找回被摘要折叠的细节。
// beforeID > 0 时只搜检查点边界及之前的行（已不在活窗口里的部分）。
func (s *ChatHistoryService) SearchContextHistory(botID, sessionID, query string, beforeID uint64, limit int) ([]stages.ContextHistoryHit, error) {
	query = strings.TrimSpace(query)
	if query == "" || botID == "" || sessionID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 8 {
		limit = 5
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	q := s.db.Model(&dao.ChatMessage{}).
		Where("bot_id = ? AND session_id = ? AND content LIKE ? ESCAPE '\\'", botID, sessionID, "%"+escaped+"%")
	if beforeID > 0 {
		q = q.Where("id <= ?", beforeID)
	}
	var messages []dao.ChatMessage
	if err := q.Order("id DESC").Limit(limit).Find(&messages).Error; err != nil {
		return nil, fmt.Errorf("chat_history: search context: %w", err)
	}
	hits := make([]stages.ContextHistoryHit, 0, len(messages))
	for _, m := range messages {
		excerpt := m.Content
		if r := []rune(excerpt); len(r) > 400 {
			excerpt = string(r[:400]) + "…"
		}
		hits = append(hits, stages.ContextHistoryHit{
			ID:        m.ID,
			Role:      m.Role,
			Excerpt:   excerpt,
			CreatedAt: m.CreatedAt,
		})
	}
	return hits, nil
}

// ClearSessionMessages 清空指定会话的所有聊天消息（保留会话记录本身）。
// 返回被删除的消息数量。
func (s *ChatHistoryService) ClearSessionMessages(sessionID string) (int64, error) {
	res := s.db.Where("session_id = ?", sessionID).Delete(&dao.ChatMessage{})
	if res.Error != nil {
		return 0, fmt.Errorf("chat_history: clear session messages: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// DeleteMessages 按 ID 删除指定会话的部分消息（用于 /compact 只压缩最旧的一段，
// 而非清空整个会话）。bot_id / session_id 作为安全护栏，避免误删其他会话。
// 返回被删除的消息数量。
func (s *ChatHistoryService) DeleteMessages(botID, sessionID string, ids []uint64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := s.db.Where("bot_id = ? AND session_id = ? AND id IN ?", botID, sessionID, ids).
		Delete(&dao.ChatMessage{})
	if res.Error != nil {
		return 0, fmt.Errorf("chat_history: delete messages: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// SaveMessageAt 与 SaveMessage 类似，但允许指定 CreatedAt。用于 /compact 把历史摘要
// 插入到「最近保留段」之前，保持时间顺序（摘要早于最近 N 条，而非追加到末尾）。
func (s *ChatHistoryService) SaveMessageAt(botID, userID, role, content, traceID, sessionID string, createdAt time.Time) error {
	sessionID = s.resolveWritableSession(botID, userID, sessionID)
	msg := dao.ChatMessage{
		BotID:     botID,
		UserID:    userID,
		SessionID: sessionID,
		Role:      role,
		Content:   content,
		TraceID:   traceID,
		CreatedAt: createdAt,
	}
	if err := s.db.Create(&msg).Error; err != nil {
		return fmt.Errorf("chat_history: save message at: %w", err)
	}
	return nil
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
