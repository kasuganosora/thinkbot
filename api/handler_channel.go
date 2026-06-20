package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Channel 管理 Handler — CRUD + 类型列表
// ============================================================================

// handleListChannelTypes 返回系统支持的 Channel 类型及其字段 schema。
// GET /api/channels/types
func (s *Server) handleListChannelTypes(c *gin.Context) {
	OK(c, gin.H{
		"types": SupportedChannelTypes(),
	})
}

// handleListChannels 返回指定 Bot 的所有 Channel 配置。
// GET /api/bots/:id/channels
func (s *Server) handleListChannels(c *gin.Context) {
	botID := c.Param("id")
	channels, err := s.botSvc.ListChannelDefinitions(botID)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, channels)
}

// handleCreateChannel 创建 Channel 配置。
// POST /api/bots/:id/channels
func (s *Server) handleCreateChannel(c *gin.Context) {
	botID := c.Param("id")
	var req CreateChannelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request: "+err.Error()))
		return
	}

	if !IsValidChannelType(req.Type) {
		Fail(c, errs.BadRequest("unsupported channel type: "+req.Type))
		return
	}

	ch, err := s.botSvc.CreateChannelDefinition(botID, req.Name, req.Type, req.Config)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, ch)
}

// handleUpdateChannel 更新 Channel 配置。
// PUT /api/bots/:id/channels/:cid
func (s *Server) handleUpdateChannel(c *gin.Context) {
	botID := c.Param("id")
	channelID := c.Param("cid")
	var req UpdateChannelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request: "+err.Error()))
		return
	}

	ch, err := s.botSvc.UpdateChannelDefinition(botID, channelID, req)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, ch)
}

// handleDeleteChannel 删除 Channel 配置。
// DELETE /api/bots/:id/channels/:cid
func (s *Server) handleDeleteChannel(c *gin.Context) {
	botID := c.Param("id")
	channelID := c.Param("cid")

	if err := s.botSvc.DeleteChannelDefinition(botID, channelID); err != nil {
		Fail(c, err)
		return
	}
	OKMsg(c, "channel deleted", nil)
}
