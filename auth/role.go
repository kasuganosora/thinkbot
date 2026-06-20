package auth

// ============================================================================
// 角色 & 权限定义
//
// 现阶段仅两种角色：
//   - admin:  管理员，拥有全部权限（创建/管理 Bot、管理用户、系统设置）
//   - member: 普通成员，只能使用 Bot（Web 页面）
//
// 后续如需更细粒度的权限，可扩展 permission 表并引入 RBAC。
// ============================================================================

// Role 角色常量。
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// AllRoles 返回所有合法角色。
func AllRoles() []string {
	return []string{RoleAdmin, RoleMember}
}

// IsValidRole 检查角色是否合法。
func IsValidRole(role string) bool {
	for _, r := range AllRoles() {
		if r == role {
			return true
		}
	}
	return false
}

// Permission 权限常量。
const (
	// PermBotCreate 创建 Bot。
	PermBotCreate = "bot.create"

	// PermBotManage 配置/编辑/删除 Bot。
	PermBotManage = "bot.manage"

	// PermUserManage 管理用户（创建、删除、改角色、禁用）。
	PermUserManage = "user.manage"

	// PermBotUse 使用 Bot（Web 页面对话）。
	PermBotUse = "bot.use"

	// PermSystemConfig 访问系统设置。
	PermSystemConfig = "system.config"
)

// rolePermissions 映射角色到允许的权限集合。
var rolePermissions = map[string]map[string]bool{
	RoleAdmin: {
		PermBotCreate:     true,
		PermBotManage:     true,
		PermUserManage:    true,
		PermBotUse:        true,
		PermSystemConfig:  true,
	},
	RoleMember: {
		PermBotUse: true,
	},
}

// HasPermission 检查给定角色是否拥有指定权限。
func HasPermission(role, permission string) bool {
	perms, ok := rolePermissions[role]
	if !ok {
		return false
	}
	return perms[permission]
}

// PermissionsForRole 返回该角色的全部权限列表。
func PermissionsForRole(role string) []string {
	perms, ok := rolePermissions[role]
	if !ok {
		return nil
	}
	result := make([]string, 0, len(perms))
	for p := range perms {
		result = append(result, p)
	}
	return result
}
