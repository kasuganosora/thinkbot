package identity

import (
	"context"
	"strconv"
	"strings"

	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/dao"
)

// ============================================================================
// IdentityAdminChecker — 基于 IdentityMapping 的管理员权限检查器
//
// 实现 command.AdminChecker 接口，解析跨平台用户身份：
//   - Web 渠道：UserID 是内部用户 ID（字符串形式），直接查库
//   - 其他渠道：通过 IdentityMapping 查找绑定的内部用户，再查角色
// ============================================================================

// IdentityAdminChecker 通过身份映射判断管理员权限。
type IdentityAdminChecker struct {
	bindSvc *BindService
	authSvc *auth.AuthService
}

// NewIdentityAdminChecker 创建身份映射管理员检查器。
func NewIdentityAdminChecker(bindSvc *BindService, authSvc *auth.AuthService) *IdentityAdminChecker {
	return &IdentityAdminChecker{
		bindSvc: bindSvc,
		authSvc: authSvc,
	}
}

// IsAdmin 检查指定来源的用户是否为管理员。
//
// source 是消息来源标识（如 "telegram-bot1"），自动提取平台类型。
// userID 是平台侧用户 ID。
func (c *IdentityAdminChecker) IsAdmin(ctx context.Context, source, userID string) bool {
	if c.bindSvc == nil || c.authSvc == nil {
		return false
	}

	// Web 渠道：UserID 直接是内部用户 ID
	if strings.HasPrefix(source, "web") {
		return c.checkInternalUser(ctx, userID)
	}

	// 其他渠道：通过身份映射查找内部用户
	mapping, err := c.bindSvc.ResolveBySource(ctx, source, userID)
	if err != nil || mapping == nil {
		return false
	}
	return c.checkInternalUserID(ctx, mapping.UserID)
}

// checkInternalUser 通过字符串形式的内部用户 ID 检查角色。
func (c *IdentityAdminChecker) checkInternalUser(ctx context.Context, userIDStr string) bool {
	id, err := strconv.ParseUint(userIDStr, 10, 64)
	if err != nil {
		return false
	}
	return c.checkInternalUserID(ctx, uint(id))
}

// checkInternalUserID 通过 uint 内部用户 ID 检查角色。
func (c *IdentityAdminChecker) checkInternalUserID(ctx context.Context, userID uint) bool {
	user, err := c.authSvc.GetUser(ctx, userID)
	if err != nil || user == nil {
		return false
	}
	if user.Status != auth.StatusActive {
		return false
	}
	return user.Role == auth.RoleAdmin
}

// Ensure IdentityAdminChecker satisfies the AdminChecker interface.
var _ AdminChecker = (*IdentityAdminChecker)(nil)

// AdminChecker 是 command.AdminChecker 的镜像接口，避免 identity → agent/command 循环依赖。
type AdminChecker interface {
	IsAdmin(ctx context.Context, source, userID string) bool
}

// --- 便捷适配器（供测试和简单场景使用） ---

// StaticAdminChecker 基于 source+userID 组合的静态管理员检查器。
type StaticAdminChecker struct {
	// admins key 为 "platform:userID"，如 "telegram:123456789"。
	admins map[string]bool
}

// NewStaticAdminChecker 创建静态管理员检查器。
// adminKeys 格式为 "platform:userID"。
func NewStaticAdminChecker(adminKeys ...string) *StaticAdminChecker {
	m := make(map[string]bool, len(adminKeys))
	for _, k := range adminKeys {
		m[k] = true
	}
	return &StaticAdminChecker{admins: m}
}

// IsAdmin 实现 AdminChecker 接口。
func (c *StaticAdminChecker) IsAdmin(_ context.Context, source, userID string) bool {
	platform := extractPlatform(source)
	return c.admins[platform+":"+userID]
}

// AllowAllChecker 始终返回 true（仅用于测试）。
type AllowAllChecker struct{}

// IsAdmin 实现 AdminChecker 接口。
func (AllowAllChecker) IsAdmin(context.Context, string, string) bool { return true }

// DenyAllChecker 始终返回 false（安全默认）。
type DenyAllChecker struct{}

// IsAdmin 实现 AdminChecker 接口。
func (DenyAllChecker) IsAdmin(context.Context, string, string) bool { return false }

// --- dao.User 辅助 ---

// IsAdminUser 判断 dao.User 是否为活跃管理员。
func IsAdminUser(user *dao.User) bool {
	return user != nil &&
		user.Status == auth.StatusActive &&
		user.Role == auth.RoleAdmin
}
