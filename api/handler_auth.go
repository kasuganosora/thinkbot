package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 认证 Handler — login / logout / me / password
// ============================================================================

// handleLogin 用户登录。
// POST /api/auth/login
//
// @Summary      用户登录
// @Description  通过用户名密码登录，返回 JWT Cookie
// @Tags         认证
// @Accept       json
// @Produce      json
// @Param        body  body      LoginReq  true  "登录请求"
// @Success      200   {object}  Response{data=LoginResp}
// @Failure      400   {object}  Response
// @Failure      401   {object}  Response
// @Router       /api/auth/login [post]
func (s *Server) handleLogin(c *gin.Context) {
	var req LoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	user, err := s.authSvc.Authenticate(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		// 登录失败审计（无法获取 user，记录 username + IP）
		s.logger.Warnw("[AUDIT] login_failed",
			"action", "login",
			"username", req.Username,
			"ip", c.ClientIP(),
			"err", err.Error(),
		)
		Fail(c, err)
		return
	}

	// 签发 JWT Cookie
	token, err := s.cookie.EncodeToken(user)
	if err != nil {
		Fail(c, errs.Wrap(err, "failed to create session token"))
		return
	}
	s.cookie.SetCookie(c, token)

	// 登录成功审计
	s.logger.Infow("[AUDIT] login_success",
		"action", "login",
		"user_id", user.ID,
		"user", user.Username,
		"role", user.Role,
		"ip", c.ClientIP(),
	)

	OK(c, toLoginResp(user))
}

// handleLogout 用户登出。
// POST /api/auth/logout
//
// @Summary      用户登出
// @Description  清除 JWT Cookie
// @Tags         认证
// @Produce      json
// @Success      200  {object}  Response
// @Router       /api/auth/logout [post]
func (s *Server) handleLogout(c *gin.Context) {
	s.cookie.ClearCookie(c)
	OKMsg(c, "logged out", nil)
}

// handleMe 获取当前登录用户信息。
// GET /api/auth/me
//
// @Summary      获取当前用户
// @Description  返回当前登录用户的详细信息
// @Tags         认证
// @Produce      json
// @Success      200  {object}  Response{data=LoginResp}
// @Failure      401  {object}  Response
// @Security     CookieAuth
// @Router       /api/auth/me [get]
func (s *Server) handleMe(c *gin.Context) {
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}
	OK(c, toLoginResp(user))
}

// handleChangePassword 当前用户修改自己的密码。
// PUT /api/auth/password
//
// @Summary      修改密码
// @Description  当前用户修改自己的登录密码
// @Tags         认证
// @Accept       json
// @Produce      json
// @Param        body  body      ChangePasswordReq  true  "修改密码请求"
// @Success      200   {object}  Response
// @Failure      400   {object}  Response
// @Failure      401   {object}  Response
// @Security     CookieAuth
// @Router       /api/auth/password [put]
func (s *Server) handleChangePassword(c *gin.Context) {
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}

	var req ChangePasswordReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	// 验证旧密码
	_, err := s.authSvc.Authenticate(c.Request.Context(), user.Username, req.OldPassword)
	if err != nil {
		Fail(c, errs.BadRequest("old password is incorrect"))
		return
	}

	// 更新密码
	if err := s.authSvc.UpdatePassword(c.Request.Context(), user.ID, req.NewPassword); err != nil {
		Fail(c, err)
		return
	}

	OKMsg(c, "password updated", nil)
}

// toLoginResp 将 dao.User 转换为 LoginResp（不暴露密码哈希等敏感字段）。
func toLoginResp(u *dao.User) LoginResp {
	resp := LoginResp{
		ID:          u.ID,
		Username:    u.Username,
		Role:        u.Role,
		DisplayName: u.DisplayName,
		Avatar:      u.Avatar,
	}
	if u.LastLoginAt != nil && !u.LastLoginAt.IsZero() {
		t := u.LastLoginAt.Format("2006-01-02T15:04:05Z")
		resp.LastLoginAt = &t
	}
	return resp
}
