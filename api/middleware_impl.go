package api

import (
	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 认证 & 权限中间件
// ============================================================================

// cookieAuth 返回 Cookie 认证中间件。
// 读取 Cookie → 验证 JWT → 注入 *dao.User 到 gin.Context。
func (s *Server) cookieAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr, err := c.Cookie(CookieName)
		if err != nil || tokenStr == "" {
			Fail(c, errs.Unauthorized("not logged in"))
			c.Abort()
			return
		}

		claims, err := s.cookie.DecodeToken(tokenStr)
		if err != nil {
			Fail(c, errs.Unauthorized("invalid or expired session"))
			c.Abort()
			return
		}

		// 验证 JWT 中的用户状态
		if claims.Status != auth.StatusActive {
			Fail(c, errs.Unauthorized("user account is disabled"))
			c.Abort()
			return
		}

		// 从 DB 获取最新用户信息（确保角色/状态实时性）
		user, err := s.authSvc.GetUser(c.Request.Context(), claims.UserID)
		if err != nil {
			Fail(c, errs.Unauthorized("user not found"))
			c.Abort()
			return
		}

		if user.Status != auth.StatusActive {
			Fail(c, errs.Unauthorized("user account is disabled"))
			c.Abort()
			return
		}

		c.Set(ContextKeyUser, user)
		c.Next()
	}
}

// requirePermission 返回权限检查中间件。
// 必须在 cookieAuth 之后使用。
func requirePermission(permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := currentUser(c)
		if user == nil {
			Fail(c, errs.Unauthorized("not logged in"))
			c.Abort()
			return
		}

		if !auth.HasPermission(user.Role, permission) {
			Fail(c, errs.Forbidden("insufficient permissions"))
			c.Abort()
			return
		}

		c.Next()
	}
}

// currentUser 从 gin.Context 中提取当前登录用户。
func currentUser(c *gin.Context) *dao.User {
	v, exists := c.Get(ContextKeyUser)
	if !exists {
		return nil
	}
	user, ok := v.(*dao.User)
	if !ok {
		return nil
	}
	return user
}

// ============================================================================
// 审计日志辅助
// ============================================================================

// auditLog 在 handler 内部调用，记录一条结构化审计日志。
// 典型用法：
//
//	auditLog(c, s.logger, "create_bot", "id="+req.ID)
//	auditLog(c, s.logger, "delete_user", "id="+strconv.FormatUint(uint64(id), 10))
//	auditLog(c, s.logger, "update_config", "key="+key, "value="+req.Value)
//
// 自动从 gin.Context 提取当前用户，无需 handler 手动传参。
func auditLog(c *gin.Context, logger loggableLogger, action string, details ...any) {
	user := currentUser(c)
	args := []any{
		"action", action,
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"ip", c.ClientIP(),
	}
	if user != nil {
		args = append(args, "user_id", user.ID, "user", user.Username, "role", user.Role)
	} else {
		args = append(args, "user", "anonymous")
	}
	args = append(args, details...)
	logger.Infow("[AUDIT]", args...)
}

// loggableLogger 抽象 zap.SugaredLogger，方便测试。
type loggableLogger interface {
	Infow(msg string, keysAndValues ...any)
	Warnw(msg string, keysAndValues ...any)
	Errorw(msg string, keysAndValues ...any)
}
