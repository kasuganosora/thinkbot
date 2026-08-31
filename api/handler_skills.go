package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 技能管理 Handler — 列出 / 启用 / 禁用（admin）
//
// 技能是全局的（从文件系统 skills/ 目录加载），不区分 Bot。
// 但启用状态是 per-bot 的（通过 config.Store 存储）。
// ============================================================================

// handleListSkills 列出所有已注册的技能。
// GET /api/skills
//
// @Summary      技能列表
// @Description  列出所有已注册的技能（需要 bot.manage 权限）
// @Tags         技能
// @Produce      json
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/skills [get]
func (s *Server) handleListSkills(c *gin.Context) {
	infos := s.skillMgr.List()
	OK(c, gin.H{"skills": infos, "total": len(infos)})
}

// handleGetSkill 获取单个技能详情。
// GET /api/skills/:name
//
// @Summary      获取技能
// @Description  获取指定技能的详细信息
// @Tags         技能
// @Produce      json
// @Param        name  path      string  true  "技能名称"
// @Success      200   {object}  Response
// @Failure      404   {object}  Response
// @Security     CookieAuth
// @Router       /api/skills/{name} [get]
func (s *Server) handleGetSkill(c *gin.Context) {
	name := c.Param("name")

	info, ok := s.skillMgr.GetInfo(name)
	if !ok {
		Fail(c, errs.Newf("skill %q not found", name))
		return
	}
	OK(c, info)
}

// handleEnableSkill 启用技能。
// PUT /api/skills/:name/enable
//
// @Summary      启用技能
// @Description  启用指定技能
// @Tags         技能
// @Produce      json
// @Param        name  path      string  true  "技能名称"
// @Success      200   {object}  Response
// @Security     CookieAuth
// @Router       /api/skills/{name}/enable [put]
func (s *Server) handleEnableSkill(c *gin.Context) {
	name := c.Param("name")

	if err := s.skillMgr.Enable(name); err != nil {
		Fail(c, errs.Wrap(err, "failed to enable skill"))
		return
	}
	auditLog(c, s.logger, "enable_skill", "skill", name)
	OKMsg(c, "skill enabled", gin.H{"name": name, "enabled": true})
}

// handleDisableSkill 禁用技能。
// PUT /api/skills/:name/disable
//
// @Summary      禁用技能
// @Description  禁用指定技能
// @Tags         技能
// @Produce      json
// @Param        name  path      string  true  "技能名称"
// @Success      200   {object}  Response
// @Security     CookieAuth
// @Router       /api/skills/{name}/disable [put]
func (s *Server) handleDisableSkill(c *gin.Context) {
	name := c.Param("name")

	if err := s.skillMgr.Disable(name); err != nil {
		Fail(c, errs.Wrap(err, "failed to disable skill"))
		return
	}
	auditLog(c, s.logger, "disable_skill", "skill", name)
	OKMsg(c, "skill disabled", gin.H{"name": name, "enabled": false})
}
