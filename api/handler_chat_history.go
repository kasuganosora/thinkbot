package api

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/dao"
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
//
// @Summary      聊天历史
// @Description  游标分页查询聊天历史记录
// @Tags         聊天
// @Produce      json
// @Param        botId   query     string  true   "Bot ID"
// @Param        cursor  query     string  false  "分页游标"
// @Param        limit   query     int     false  "每页条数"  default(20)
// @Success      200  {object}  Response
// @Failure      400  {object}  Response
// @Failure      401  {object}  Response
// @Security     CookieAuth
// @Router       /api/chat/history [get]
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

	OK(c, gin.H{
		"messages":   toChatMessageDTOs(page.Messages),
		"nextCursor": page.NextCursor,
		"hasMore":    page.HasMore,
	})
}

// chatMessageDTO 前端展示用的消息结构。
// ToolCalls 由持久化的 JSON 字符串反序列化为数组，供前端复原工具卡片。
type chatMessageDTO struct {
	ID        uint64 `json:"id"`
	BotID     string `json:"botId"`
	UserID    string `json:"userId"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	ToolCalls []any  `json:"toolCalls"`
	TraceID   string `json:"traceId"`
	CreatedAt string `json:"createdAt"`
}

// toChatMessageDTOs 将持久化消息转换为前端 DTO，并解析 ToolCalls JSON。
func toChatMessageDTOs(msgs []dao.ChatMessage) []chatMessageDTO {
	out := make([]chatMessageDTO, 0, len(msgs))
	for _, m := range msgs {
		dto := chatMessageDTO{
			ID:        m.ID,
			BotID:     m.BotID,
			UserID:    m.UserID,
			Role:      m.Role,
			Content:   m.Content,
			ToolCalls: []any{},
			TraceID:   m.TraceID,
			CreatedAt: m.CreatedAt.Format(time.RFC3339),
		}
		if m.ToolCalls != "" {
			var tc []any
			if err := json.Unmarshal([]byte(m.ToolCalls), &tc); err == nil && tc != nil {
				dto.ToolCalls = tc
			}
		}
		out = append(out, dto)
	}
	return out
}
