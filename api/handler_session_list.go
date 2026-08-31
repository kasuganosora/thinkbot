package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/kasuganosora/thinkbot/dao"
	"gorm.io/gorm"
)

// ============================================================================
// Session 列表管理 Handler
//
// 前端契约（sessionApi）：
//   GET    /api/bots/:id/sessions          — 会话列表（按最后消息时间倒序）
//   POST   /api/bots/:id/sessions          — 新建会话
//   DELETE /api/sessions/:sid              — 删除会话（级联删除消息）
//   PUT    /api/sessions/:sid              — 更新会话（标题、归档）
// ============================================================================

// SessionListResp 会话列表响应。
type SessionListResp struct {
	Sessions []dao.ChatSession `json:"sessions"`
	Total    int               `json:"total"`
}

// CreateSessionReq 新建会话请求。
type CreateSessionReq struct {
	Title string `json:"title"`
}

// UpdateSessionReq 更新会话请求。
type UpdateSessionReq struct {
	Title  string `json:"title"`
	Status string `json:"status"` // "active" / "archived"
}

// handleListSessions 返回指定 Bot 下的所有会话列表。
// GET /api/bots/:id/sessions
func (s *Server) handleListSessions(c *gin.Context) {
	botID := c.Param("id")
	if botID == "" {
		FailMsg(c, http.StatusBadRequest, "bot_id required")
		return
	}

	var sessions []dao.ChatSession
	q := s.db.Where("bot_id = ?", botID).
		Order("last_msg_at DESC, created_at DESC").
		Find(&sessions)

	if q.Error != nil {
		s.logger.Warnw("list sessions failed", "bot_id", botID, "err", q.Error)
		FailMsg(c, http.StatusInternalServerError, "查询失败")
		return
	}

	// 首次访问且尚无会话：自动创建"默认会话"并迁移历史消息，
	// 避免 session_id 为空/ NULL 的历史记录因会话隔离而"消失"。
	if len(sessions) == 0 {
		defaultSess := dao.ChatSession{
			BotID:        botID,
			Title:        "默认会话",
			Status:       dao.SessionStatusActive,
			MessageCount: 0,
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		if err := s.db.Create(&defaultSess).Error; err == nil {
			// 将本 bot 下未归属会话的历史消息归到默认会话
			if res := s.db.Model(&dao.ChatMessage{}).
				Where("bot_id = ? AND (session_id = '' OR session_id IS NULL)", botID).
				Update("session_id", strconv.FormatUint(defaultSess.ID, 10)); res.Error != nil {
				s.logger.Warnw("migrate history to default session failed", "bot_id", botID, "err", res.Error)
			} else if res.RowsAffected > 0 {
				defaultSess.MessageCount = int(res.RowsAffected)
				s.db.Model(&dao.ChatSession{}).Where("id = ?", defaultSess.ID).
					Update("message_count", defaultSess.MessageCount)
			}
			sessions = []dao.ChatSession{defaultSess}
		}
	}

	// 统计总数
	var total int64
	s.db.Model(&dao.ChatSession{}).Where("bot_id = ?", botID).Count(&total)
	if len(sessions) > 0 {
		total = int64(len(sessions))
	}

	OK(c, gin.H{
		"sessions": sessions,
		"total":    total,
	})
}

// handleCreateSession 为指定 Bot 创建新会话。
// POST /api/bots/:id/sessions
func (s *Server) handleCreateSession(c *gin.Context) {
	botID := c.Param("id")
	if botID == "" {
		FailMsg(c, http.StatusBadRequest, "bot_id required")
		return
	}

	var req CreateSessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		req = CreateSessionReq{Title: "新会话"} // 默认标题
	}
	if req.Title == "" {
		req.Title = "新会话"
	}

	session := dao.ChatSession{
		BotID:        botID,
		Title:        req.Title,
		Status:       dao.SessionStatusActive,
		MessageCount: 0,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	if err := s.db.Create(&session).Error; err != nil {
		s.logger.Warnw("create session failed", "bot_id", botID, "err", err)
		FailMsg(c, http.StatusInternalServerError, "创建失败")
		return
	}

	OK(c, session)
}

// handleDeleteSession 删除指定会话及其所有关联消息。
// DELETE /api/sessions/:sid
func (s *Server) handleDeleteSession(c *gin.Context) {
	sidStr := c.Param("sid")
	sid, err := strconv.ParseUint(sidStr, 10, 64)
	if err != nil {
		FailMsg(c, http.StatusBadRequest, "invalid session id")
		return
	}

	// 查找会话
	var session dao.ChatSession
	if err := s.db.First(&session, sid).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			FailMsg(c, http.StatusNotFound, "会话不存在")
			return
		}
		s.logger.Warnw("find session failed", "session_id", sid, "err", err)
		FailMsg(c, http.StatusInternalServerError, "查询失败")
		return
	}

	// 级联删除该会话下的所有聊天消息
	result := s.db.Where("session_id = ?", sid).Delete(&dao.ChatMessage{})
	if result.Error != nil {
		// 消息表可能还没有 session_id 字段，不阻塞删除
		s.logger.Debugw("delete session messages (may not have session_id yet)", "session_id", sid, "err", result.Error)
	}

	// 删除会话本身
	if err := s.db.Delete(&session).Error; err != nil {
		s.logger.Warnw("delete session failed", "session_id", sid, "err", err)
		FailMsg(c, http.StatusInternalServerError, "删除失败")
		return
	}

	auditLog(c, s.logger, "session_delete", "session", sidStr, "title", session.Title)
	OK(c, gin.H{"ok": true})
}

// handleUpdateSession 更新会话信息（标题或状态）。
// PUT /api/sessions/:sid
func (s *Server) handleUpdateSession(c *gin.Context) {
	sidStr := c.Param("sid")
	sid, err := strconv.ParseUint(sidStr, 10, 64)
	if err != nil {
		FailMsg(c, http.StatusBadRequest, "invalid session id")
		return
	}

	var session dao.ChatSession
	if err := s.db.First(&session, sid).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			FailMsg(c, http.StatusNotFound, "会话不存在")
			return
		}
		FailMsg(c, http.StatusInternalServerError, "查询失败")
		return
	}

	var req UpdateSessionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		// 无 body 时仅刷新 updated_at
		session.UpdatedAt = time.Now()
	} else {
		if req.Title != "" {
			session.Title = req.Title
		}
		if req.Status == dao.SessionStatusArchived || req.Status == dao.SessionStatusActive {
			session.Status = req.Status
		}
		session.UpdatedAt = time.Now()
	}

	if err := s.db.Save(&session).Error; err != nil {
		FailMsg(c, http.StatusInternalServerError, "更新失败")
		return
	}

	OK(c, session)
}
