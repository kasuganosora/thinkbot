package api

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/identity"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 授权码 & 身份绑定 API
//
// 用户在 Web 页面：
//   - POST /api/bindcode          → 生成一次性授权码
//   - GET  /api/bindcode          → 查看未使用且未过期的授权码
//   - GET  /api/bindings          → 查看已绑定的平台身份
//   - DELETE /api/bindings/:id    → 解绑某个平台身份
// ============================================================================

// handleGenerateBindCode 生成一次性授权码。
// 每个码有效期为 5 分钟，只能使用一次。
//
// @Summary      生成授权码
// @Description  生成一个一次性授权码（5 分钟有效），用于跨平台身份绑定
// @Tags         授权码与绑定
// @Produce      json
// @Success      200  {object}  Response
// @Failure      401  {object}  Response
// @Security     CookieAuth
// @Router       /api/bindcode [post]
func (s *Server) handleGenerateBindCode(c *gin.Context) {
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}

	code, err := s.bindSvc.GenerateCode(c.Request.Context(), user.ID)
	if err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "generate_bind_code", "user_id", user.ID)

	OK(c, gin.H{
		"code":      code.Code,
		"expiresAt": code.ExpiresAt,
		"ttl":       int(time.Until(code.ExpiresAt).Minutes()),
		"hint":      fmt.Sprintf("请在 Telegram/Misskey 等平台向 Bot 发送 %s 完成绑定。有效期 5 分钟，仅限使用一次。", code.Code),
	})
}

// handleListBindCodes 列出当前用户未使用且未过期的授权码。
//
// @Summary      列出授权码
// @Description  返回当前用户所有未使用且未过期的授权码
// @Tags         授权码与绑定
// @Produce      json
// @Success      200  {object}  Response
// @Failure      401  {object}  Response
// @Security     CookieAuth
// @Router       /api/bindcode [get]
func (s *Server) handleListBindCodes(c *gin.Context) {
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}

	codes, err := s.bindSvc.ListCodes(c.Request.Context(), user.ID)
	if err != nil {
		Fail(c, err)
		return
	}

	type codeItem struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expiresAt"`
		TTLSec    int       `json:"ttlSec"`
	}

	now := time.Now()
	items := make([]codeItem, 0, len(codes))
	for _, c := range codes {
		items = append(items, codeItem{
			Code:      c.Code,
			ExpiresAt: c.ExpiresAt,
			TTLSec:    int(c.ExpiresAt.Sub(now).Seconds()),
		})
	}

	OK(c, gin.H{"codes": items})
}

// handleListBindings 列出当前用户的所有身份映射。
//
// @Summary      列出绑定
// @Description  返回当前用户所有已绑定的平台身份
// @Tags         授权码与绑定
// @Produce      json
// @Success      200  {object}  Response
// @Failure      401  {object}  Response
// @Security     CookieAuth
// @Router       /api/bindings [get]
func (s *Server) handleListBindings(c *gin.Context) {
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}

	mappings, err := s.bindSvc.ListBindings(c.Request.Context(), user.ID)
	if err != nil {
		Fail(c, err)
		return
	}

	type bindingItem struct {
		ID             uint      `json:"id"`
		Platform       string    `json:"platform"`
		PlatformUserID string    `json:"platformUserId"`
		CreatedAt      time.Time `json:"createdAt"`
	}

	items := make([]bindingItem, 0, len(mappings))
	for _, m := range mappings {
		items = append(items, bindingItem{
			ID:             m.ID,
			Platform:       m.Platform,
			PlatformUserID: m.PlatformUserID,
			CreatedAt:      m.CreatedAt,
		})
	}

	OK(c, gin.H{"bindings": items})
}

// handleDeleteBinding 解绑某个平台身份。
//
// @Summary      删除绑定
// @Description  解除指定的平台身份映射
// @Tags         授权码与绑定
// @Produce      json
// @Param        id  path      int  true  "绑定记录 ID"
// @Success      200  {object}  Response
// @Failure      400  {object}  Response
// @Failure      401  {object}  Response
// @Failure      404  {object}  Response
// @Security     CookieAuth
// @Router       /api/bindings/{id} [delete]
func (s *Server) handleDeleteBinding(c *gin.Context) {
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("not logged in"))
		return
	}

	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		Fail(c, errs.BadRequest("invalid binding id"))
		return
	}

	// 查找映射，确认归属
	mappings, err := s.bindSvc.ListBindings(c.Request.Context(), user.ID)
	if err != nil {
		Fail(c, err)
		return
	}

	var target *dao.IdentityMapping
	for i := range mappings {
		if mappings[i].ID == uint(id) {
			target = &mappings[i]
			break
		}
	}
	if target == nil {
		Fail(c, errs.NotFound("binding not found"))
		return
	}

	if err := s.bindSvc.DeleteBinding(c.Request.Context(), user.ID, target.Platform, target.PlatformUserID); err != nil {
		Fail(c, err)
		return
	}

	auditLog(c, s.logger, "delete_binding",
		"binding_id", id,
		"platform", target.Platform,
		"platform_user_id", target.PlatformUserID)

	OKMsg(c, "解绑成功", nil)
}

// Ensure identity.BindService is referenced (suppress unused import).
var _ = identity.IsBindCode
