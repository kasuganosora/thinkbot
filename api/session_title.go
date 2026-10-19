package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/strutil"
	"gorm.io/gorm"
)

func isPlaceholderSessionTitle(t string) bool {
	switch strings.TrimSpace(t) {
	case "", "新会话", "默认会话":
		return true
	default:
		return false
	}
}

// titleFromFirstMessage 由首条用户消息生成会话标题（截断到 30 rune）。
//
// 必须先剥离渠道装饰再压缩空白：strings.Fields 会把 "[Reply to 栞娜: <bot 原文>]\n<用户正文>"
// 的换行压成空格，剥离正则依赖该换行作为边界，顺序颠倒将直接失效（B18）。
func titleFromFirstMessage(content string) string {
	s := strings.Join(strings.Fields(strutil.StripChannelContextMarkers(content)), " ")
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "/") {
		return ""
	}
	if s == "[附件]" {
		return "附件"
	}
	runes := []rune(s)
	const max = 30
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return s
}

// touchSessionAfterSave refreshes last_msg_at / message_count and, for a user
// message, fills a placeholder title from the first ~30 runes of content.
//
// message_count is derived from the actual rows (COUNT(*)) instead of a blind
// "+1": a "+1" drifts whenever some path deletes/moves rows or inserts without
// touching. db is the transaction the insert ran in, so the count is
// committed atomically with the row.
func (s *ChatHistoryService) touchSessionAfterSave(db *gorm.DB, sessionID, role, content string) error {
	if s == nil || db == nil || sessionID == "" {
		return nil
	}
	id, err := strconv.ParseUint(sessionID, 10, 64)
	if err != nil || id == 0 {
		return nil
	}
	now := time.Now()
	updates := map[string]any{
		"message_count": gorm.Expr("(SELECT COUNT(*) FROM chat_messages WHERE session_id = ?)", sessionID),
		"last_msg_at":   now,
		"updated_at":    now,
	}
	if role == dao.ChatRoleUser {
		var sess dao.ChatSession
		if err := db.Select("id", "title").First(&sess, id).Error; err == nil && isPlaceholderSessionTitle(sess.Title) {
			if title := titleFromFirstMessage(content); title != "" {
				updates["title"] = title
			}
		}
	}
	return db.Model(&dao.ChatSession{}).Where("id = ?", id).Updates(updates).Error
}
