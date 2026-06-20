package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 梦境巩固配置 Handler — 读取 / 设置（admin）
// ============================================================================

// handleGetDreamingConfig 获取指定 Bot 的梦境巩固配置。
// GET /api/bots/:id/dreaming
//
// @Summary      获取梦境配置
// @Description  获取指定 Bot 的梦境巩固配置
// @Tags         梦境巩固
// @Produce      json
// @Param        id  path      string  true  "Bot ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/dreaming [get]
func (s *Server) handleGetDreamingConfig(c *gin.Context) {
	botID := c.Param("id")

	builder := config.NewBuilder(s.store, s.logger)
	cfg := builder.GetDreamingConfig(botID)

	OK(c, DreamingConfigResp{
		Enabled:  cfg.Enabled,
		Schedule: cfg.Schedule,
	})
}

// handleUpdateDreamingConfig 更新指定 Bot 的梦境巩固配置。
// PUT /api/bots/:id/dreaming
//
// 请求体（字段可选）：
//
//	{"enabled": true, "schedule": "0 3 * * *"}
//
// 注意：修改配置后需要重启 Bot 才能生效。
//
// @Summary      更新梦境配置
// @Description  更新指定 Bot 的梦境巩固配置（字段可选）
// @Tags         梦境巩固
// @Accept       json
// @Produce      json
// @Param        id    path      string                  true  "Bot ID"
// @Param        body  body      UpdateDreamingConfigReq true  "更新梦境配置请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/dreaming [put]
func (s *Server) handleUpdateDreamingConfig(c *gin.Context) {
	botID := c.Param("id")

	var req UpdateDreamingConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	// 读取当前配置，合并更新
	builder := config.NewBuilder(s.store, s.logger)
	cfg := builder.GetDreamingConfig(botID)

	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.Schedule != nil {
		if *req.Schedule == "" {
			Fail(c, errs.BadRequest("schedule must not be empty"))
			return
		}
		cfg.Schedule = *req.Schedule
	}

	if err := builder.SetDreamingConfig(c.Request.Context(), botID, cfg); err != nil {
		Fail(c, errs.Wrap(err, "failed to set dreaming config"))
		return
	}

	auditLog(c, s.logger, "update_dreaming_config", "bot_id", botID, "enabled", cfg.Enabled, "schedule", cfg.Schedule)

	OKMsg(c, "dreaming config updated, restart bot to take effect", DreamingConfigResp{
		Enabled:  cfg.Enabled,
		Schedule: cfg.Schedule,
	})
}
