package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/strutil"
)

// ============================================================================
// Bot 管理 Handler — CRUD / 启停（admin）
// ============================================================================

// handleListBots 列出所有 Bot 定义。
// GET /api/bots
//
// @Summary      Bot 列表
// @Description  列出所有 Bot 定义及其运行状态
// @Tags         Bot 管理
// @Produce      json
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots [get]
func (s *Server) handleListBots(c *gin.Context) {
	defs, err := s.botSvc.ListDefinitions()
	if err != nil {
		Fail(c, err)
		return
	}

	// 附加运行状态
	type botListItem struct {
		dao.BotDefinition
		Running bool `json:"running"`
	}

	result := make([]botListItem, len(defs))
	for i, def := range defs {
		result[i].BotDefinition = def
		result[i].Running = s.botSvc.IsRunning(def.ID)
	}

	OK(c, result)
}

// handleGetBot 获取单个 Bot 定义。
// GET /api/bots/:id
//
// @Summary      获取 Bot
// @Description  获取指定 Bot 的定义和运行时信息
// @Tags         Bot 管理
// @Produce      json
// @Param        id   path      string  true  "Bot ID"
// @Success      200  {object}  Response
// @Failure      404  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id} [get]
func (s *Server) handleGetBot(c *gin.Context) {
	id := c.Param("id")

	def, err := s.botSvc.GetDefinition(id)
	if err != nil {
		Fail(c, err)
		return
	}

	// 尝试获取运行时信息
	type botDetail struct {
		dao.BotDefinition
		Running bool `json:"running"`
		Info    *any `json:"info,omitempty"`
	}

	detail := botDetail{
		BotDefinition: *def,
		Running:       s.botSvc.IsRunning(id),
	}

	if info, err := s.botSvc.GetBotInfo(id); err == nil && info != nil {
		i := any(info)
		detail.Info = &i
	}

	OK(c, detail)
}

// handleCreateBot 创建 Bot 定义。
// POST /api/bots
//
// @Summary      创建 Bot
// @Description  创建新的 Bot 定义（需要 bot.manage 权限）
// @Tags         Bot 管理
// @Accept       json
// @Produce      json
// @Param        body  body      CreateBotReq  true  "创建 Bot 请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/bots [post]
func (s *Server) handleCreateBot(c *gin.Context) {
	var req CreateBotReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	def := &dao.BotDefinition{
		ID:           req.ID,
		Name:         req.Name,
		SystemPrompt: req.SystemPrompt,
		LLMMain:      req.LLMMain,
		LLMLight:     req.LLMLight,
		Model:        req.Model,
		Temperature:  req.Temperature,
		MaxTokens:    req.MaxTokens,
		Workers:      req.Workers,
		Status:       dao.BotStatusStopped,
	}

	if err := s.botSvc.CreateDefinition(def); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "create_bot", "bot_id", req.ID, "name", req.Name)
	OK(c, def)
}

// handleUpdateBot 更新 Bot 定义。
// PUT /api/bots/:id
//
// @Summary      更新 Bot
// @Description  更新指定 Bot 的配置（字段可选，需要 bot.manage 权限）
// @Tags         Bot 管理
// @Accept       json
// @Produce      json
// @Param        id    path      string        true  "Bot ID"
// @Param        body  body      UpdateBotReq  true  "更新 Bot 请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id} [put]
func (s *Server) handleUpdateBot(c *gin.Context) {
	id := c.Param("id")

	var req UpdateBotReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	updates := map[string]any{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.SystemPrompt != nil {
		updates["system_prompt"] = *req.SystemPrompt
	}
	if req.LLMMain != nil {
		updates["llm_main"] = *req.LLMMain
	}
	if req.LLMLight != nil {
		updates["llm_light"] = *req.LLMLight
	}
	if req.Model != nil {
		updates["model"] = *req.Model
	}
	if req.Temperature != nil {
		updates["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		updates["max_tokens"] = *req.MaxTokens
	}
	if req.Workers != nil {
		updates["workers"] = *req.Workers
	}

	if len(updates) == 0 {
		OKMsg(c, "no changes", nil)
		return
	}

	if err := s.botSvc.UpdateDefinition(id, updates); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "update_bot", "bot_id", id, "fields", strutil.MapKeys(updates))
	OKMsg(c, "bot updated", nil)
}

// handleDeleteBot 删除 Bot 定义。
// DELETE /api/bots/:id
//
// @Summary      删除 Bot
// @Description  删除指定 Bot 定义
// @Tags         Bot 管理
// @Produce      json
// @Param        id  path      string  true  "Bot ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id} [delete]
func (s *Server) handleDeleteBot(c *gin.Context) {
	id := c.Param("id")

	if err := s.botSvc.DeleteDefinition(id); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "delete_bot", "bot_id", id)
	OKMsg(c, "bot deleted", nil)
}

// handleStartBot 启动 Bot。
// POST /api/bots/:id/start
//
// @Summary      启动 Bot
// @Description  启动指定 Bot
// @Tags         Bot 管理
// @Produce      json
// @Param        id  path      string  true  "Bot ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/start [post]
func (s *Server) handleStartBot(c *gin.Context) {
	id := c.Param("id")

	if err := s.botSvc.StartBot(c.Request.Context(), id); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "start_bot", "bot_id", id)
	OKMsg(c, "bot started", nil)
}

// handleStopBot 停止 Bot。
// POST /api/bots/:id/stop
//
// @Summary      停止 Bot
// @Description  停止指定 Bot
// @Tags         Bot 管理
// @Produce      json
// @Param        id  path      string  true  "Bot ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/bots/{id}/stop [post]
func (s *Server) handleStopBot(c *gin.Context) {
	id := c.Param("id")
	s.botSvc.StopBot(id)
	auditLog(c, s.logger, "stop_bot", "bot_id", id)
	OKMsg(c, "bot stopped", nil)
}
