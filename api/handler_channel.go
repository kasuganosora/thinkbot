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
//
// @Summary      Channel 类型
// @Description  返回系统支持的 Channel 类型及其配置字段
// @Tags         Channel 管理
// @Produce      json
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/channels/types [get]
func (s *Server) handleListChannelTypes(c *gin.Context) {
	OK(c, gin.H{
		"types": SupportedChannelTypes(),
	})
}

// handleListChannels 返回指定 Bot 的所有 Channel 配置。
// GET /api/bots/:id/channels
//
// @Summary      Channel 列表
// @Description  返回指定 Bot 的所有 Channel 配置
// @Tags         Channel 管理
// @Produce      json
// @Param        id  path      string  true  "Bot ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/channels [get]
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
//
// @Summary      创建 Channel
// @Description  为指定 Bot 创建新的 Channel 配置
// @Tags         Channel 管理
// @Accept       json
// @Produce      json
// @Param        id    path      string            true  "Bot ID"
// @Param        body  body      CreateChannelReq  true  "创建 Channel 请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/channels [post]
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
	auditLog(c, s.logger, "create_channel", "bot_id", botID, "channel_id", ch.ID, "type", req.Type)
	OK(c, ch)
}

// handleUpdateChannel 更新 Channel 配置。
// PUT /api/bots/:id/channels/:cid
//
// @Summary      更新 Channel
// @Description  更新指定的 Channel 配置
// @Tags         Channel 管理
// @Accept       json
// @Produce      json
// @Param        id    path      string             true  "Bot ID"
// @Param        cid   path      string             true  "Channel ID"
// @Param        body  body      UpdateChannelReq   true  "更新 Channel 请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/channels/{cid} [put]
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
	auditLog(c, s.logger, "update_channel", "bot_id", botID, "channel_id", channelID)
	OK(c, ch)
}

// handleDeleteChannel 删除 Channel 配置。
// DELETE /api/bots/:id/channels/:cid
//
// @Summary      删除 Channel
// @Description  删除指定的 Channel 配置
// @Tags         Channel 管理
// @Produce      json
// @Param        id   path      string  true  "Bot ID"
// @Param        cid  path      string  true  "Channel ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/channels/{cid} [delete]
func (s *Server) handleDeleteChannel(c *gin.Context) {
	botID := c.Param("id")
	channelID := c.Param("cid")

	if err := s.botSvc.DeleteChannelDefinition(botID, channelID); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "delete_channel", "bot_id", botID, "channel_id", channelID)
	OKMsg(c, "channel deleted", nil)
}
