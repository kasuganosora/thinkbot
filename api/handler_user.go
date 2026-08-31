package api

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 用户管理 Handler — CRUD / 角色 / 密码 / 启停（admin）
// ============================================================================

// handleListUsers 列出所有用户。
// GET /api/users
//
// @Summary      用户列表
// @Description  列出所有用户（需要 user.manage 权限）
// @Tags         用户管理
// @Produce      json
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/users [get]
func (s *Server) handleListUsers(c *gin.Context) {
	users, err := s.authSvc.ListUsers(c.Request.Context())
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, users)
}

// handleCreateUser 创建用户。
// POST /api/users
//
// @Summary      创建用户
// @Description  创建新用户（需要 user.manage 权限）
// @Tags         用户管理
// @Accept       json
// @Produce      json
// @Param        body  body      CreateUserReq  true  "创建用户请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/users [post]
func (s *Server) handleCreateUser(c *gin.Context) {
	var req CreateUserReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	user, err := s.authSvc.CreateUser(c.Request.Context(), auth.CreateUserInput{
		Username:    req.Username,
		Password:    req.Password,
		Email:       req.Email,
		Role:        req.Role,
		DisplayName: req.DisplayName,
	})
	if err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "create_user", "target", req.Username, "role", req.Role)
	OK(c, user)
}

// handleGetUser 获取指定用户。
// GET /api/users/:id
//
// @Summary      获取用户
// @Description  获取指定 ID 的用户详情
// @Tags         用户管理
// @Produce      json
// @Param        id   path      int  true  "用户 ID"
// @Success      200  {object}  Response
// @Failure      400  {object}  Response
// @Security     CookieAuth
// @Router       /api/users/{id} [get]
func (s *Server) handleGetUser(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		Fail(c, err)
		return
	}

	user, err := s.authSvc.GetUser(c.Request.Context(), id)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, user)
}

// handleUpdateUser 更新用户资料（邮箱、显示名、头像）。
// PUT /api/users/:id
//
// @Summary      更新用户资料
// @Description  更新指定用户的邮箱、显示名、头像
// @Tags         用户管理
// @Accept       json
// @Produce      json
// @Param        id    path      int              true  "用户 ID"
// @Param        body  body      UpdateUserReq    true  "更新用户请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/users/{id} [put]
func (s *Server) handleUpdateUser(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		Fail(c, err)
		return
	}

	var req UpdateUserReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if err := s.authSvc.UpdateProfile(c.Request.Context(), id, auth.UpdateProfileInput{
		Email:       req.Email,
		DisplayName: req.DisplayName,
		Avatar:      req.Avatar,
	}); err != nil {
		Fail(c, err)
		return
	}
	OKMsg(c, "user updated", nil)
}

// handleDeleteUser 删除用户。
// DELETE /api/users/:id
//
// @Summary      删除用户
// @Description  删除指定 ID 的用户
// @Tags         用户管理
// @Produce      json
// @Param        id  path      int  true  "用户 ID"
// @Success      200  {object}  Response
// @Failure      400  {object}  Response
// @Security     CookieAuth
// @Router       /api/users/{id} [delete]
func (s *Server) handleDeleteUser(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		Fail(c, err)
		return
	}

	if err := s.authSvc.DeleteUser(c.Request.Context(), id); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "delete_user", "target_id", id)
	OKMsg(c, "user deleted", nil)
}

// handleUpdateUserRole 修改用户角色。
// PUT /api/users/:id/role
//
// @Summary      修改角色
// @Description  修改指定用户的角色
// @Tags         用户管理
// @Accept       json
// @Produce      json
// @Param        id    path      int            true  "用户 ID"
// @Param        body  body      UpdateRoleReq  true  "角色请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/users/{id}/role [put]
func (s *Server) handleUpdateUserRole(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		Fail(c, err)
		return
	}

	var req UpdateRoleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if err := s.authSvc.UpdateRole(c.Request.Context(), id, req.Role); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "update_user_role", "target_id", id, "new_role", req.Role)
	OKMsg(c, "role updated", nil)
}

// handleDisableUser 禁用用户。
// PUT /api/users/:id/disable
//
// @Summary      禁用用户
// @Description  禁用指定用户的账户
// @Tags         用户管理
// @Produce      json
// @Param        id  path      int  true  "用户 ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/users/{id}/disable [put]
func (s *Server) handleDisableUser(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		Fail(c, err)
		return
	}

	if err := s.authSvc.DisableUser(c.Request.Context(), id); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "disable_user", "target_id", id)
	OKMsg(c, "user disabled", nil)
}

// handleEnableUser 启用用户。
// PUT /api/users/:id/enable
//
// @Summary      启用用户
// @Description  启用指定用户的账户
// @Tags         用户管理
// @Produce      json
// @Param        id  path      int  true  "用户 ID"
// @Success      200  {object}  Response
// @Security     CookieAuth
// @Router       /api/users/{id}/enable [put]
func (s *Server) handleEnableUser(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		Fail(c, err)
		return
	}

	if err := s.authSvc.EnableUser(c.Request.Context(), id); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "enable_user", "target_id", id)
	OKMsg(c, "user enabled", nil)
}

// handleResetPassword 管理员重置用户密码。
// PUT /api/users/:id/password
//
// @Summary      重置密码
// @Description  管理员重置指定用户的密码
// @Tags         用户管理
// @Accept       json
// @Produce      json
// @Param        id    path      int  true  "用户 ID"
// @Param        body  body      object{password=string}  true  "重置密码请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Security     CookieAuth
// @Router       /api/users/{id}/password [put]
func (s *Server) handleResetPassword(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		Fail(c, err)
		return
	}

	var req struct {
		Password string `json:"password" binding:"required,min=6"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	if err := s.authSvc.UpdatePassword(c.Request.Context(), id, req.Password); err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "reset_password", "target_id", id)
	OKMsg(c, "password reset", nil)
}

// parseID 从 URL 参数 :id 解析 uint ID。
func parseID(c *gin.Context) (uint, error) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		return 0, errs.BadRequest("invalid id: " + idStr)
	}
	return uint(id), nil
}
