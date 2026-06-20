package api

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 聊天历史 Handler — 游标分页查询
// ============================================================================

// handleChatHistory 游标分页查询聊天历史记录。
// GET /api/chat/history?botId=xxx&cursor=&limit=20
//
// 查询参数：
//   - botId: Bot ID（必填）
//   - cursor: 分页游标（首次查询留空，后续从上一页 nextCursor 获取）
//   - limit: 每页消息数（默认 20，最大 100）
//
// 返回消息按时间倒序（最新在前），配合 cursor 实现无限滚动翻页。
func (s *Server) handleChatHistory(c *gin.Context) {
	botID := c.Query("botId")
	if botID == "" {
		Fail(c, errs.BadRequest("botId is required"))
		return
	}

	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}

	cursor := c.Query("cursor")
	limit := defaultPageSize
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	page, err := s.chatHistory.PaginateHistory(botID, strconv.FormatUint(uint64(user.ID), 10), cursor, limit)
	if err != nil {
		Fail(c, err)
		return
	}

	OK(c, page)
}
