package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
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

func titleFromFirstMessage(content string) string {
	s := strings.Join(strings.Fields(content), " ")
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

// touchSessionAfterSave bumps last_msg_at / message_count and, for a user
// message, fills a placeholder title from the first ~30 runes of content.
func (s *ChatHistoryService) touchSessionAfterSave(sessionID, role, content string) {
	if s == nil || s.db == nil || sessionID == "" {
		return
	}
	id, err := strconv.ParseUint(sessionID, 10, 64)
	if err != nil || id == 0 {
		return
	}
	now := time.Now()
	updates := map[string]any{
		"message_count": gorm.Expr("message_count + 1"),
		"last_msg_at":   now,
		"updated_at":    now,
	}
	if role == dao.ChatRoleUser {
		var sess dao.ChatSession
		if err := s.db.Select("id", "title").First(&sess, id).Error; err == nil && isPlaceholderSessionTitle(sess.Title) {
			if title := titleFromFirstMessage(content); title != "" {
				updates["title"] = title
			}
		}
	}
	_ = s.db.Model(&dao.ChatSession{}).Where("id = ?", id).Updates(updates).Error
}
