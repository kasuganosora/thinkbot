package api

import (
	"errors"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/toolperm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Bot 工具权限管理 Handler
//
// 路由前缀：/api/bots/:id/tool-permissions
// 权限：requirePermission(auth.PermBotManage)
// ============================================================================

// handleListBotToolPerms 列出某 bot 的全部工具权限规则。
// GET /api/bots/:id/tool-permissions
func (s *Server) handleListBotToolPerms(c *gin.Context) {
	botID := c.Param("id")
	rules, err := s.permSvc.ListRules(botID)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, rules)
}

// handleCreateBotToolPerm 创建一条工具权限规则。
// POST /api/bots/:id/tool-permissions
func (s *Server) handleCreateBotToolPerm(c *gin.Context) {
	botID := c.Param("id")
	var req toolperm.RuleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	rule, err := s.permSvc.CreateRule(botID, req)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, rule)
}

// handleUpdateBotToolPerm 更新一条工具权限规则。
// PUT /api/bots/:id/tool-permissions/:rid
func (s *Server) handleUpdateBotToolPerm(c *gin.Context) {
	botID := c.Param("id")
	rid := c.Param("rid")
	var req toolperm.RuleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	rule, err := s.permSvc.UpdateRule(botID, rid, req)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			Fail(c, errs.NotFound("工具权限规则不存在"))
			return
		}
		Fail(c, err)
		return
	}
	OK(c, rule)
}

// handleDeleteBotToolPerm 删除一条工具权限规则。
// DELETE /api/bots/:id/tool-permissions/:rid
func (s *Server) handleDeleteBotToolPerm(c *gin.Context) {
	botID := c.Param("id")
	rid := c.Param("rid")
	if err := s.permSvc.DeleteRule(botID, rid); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			Fail(c, errs.NotFound("工具权限规则不存在"))
			return
		}
		Fail(c, err)
		return
	}
	OK(c, nil)
}

// handleResetBotToolPermDefaults 清空并恢复 web 默认全开规则。
// POST /api/bots/:id/tool-permissions/reset-defaults
func (s *Server) handleResetBotToolPermDefaults(c *gin.Context) {
	botID := c.Param("id")
	if err := s.permSvc.ResetDefaults(botID); err != nil {
		Fail(c, err)
		return
	}
	OK(c, nil)
}
