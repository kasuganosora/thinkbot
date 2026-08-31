# auth — 用户认证与权限管理

提供用户 CRUD、密码验证（bcrypt）、角色权限检查能力。

## 功能

- **用户管理**：创建、查询、更新（角色/密码/资料）、启用/禁用、删除用户
- **密码认证**：bcrypt 哈希存储，登录自动更新最后登录时间；禁用用户直接拒绝
- **角色权限**：`admin`（全部权限）和 `member`（仅 `bot.use`）
- **引导管理员**：
  - fx `OnStart` 钩子检测环境变量 `AUTH_BOOTSTRAP_ADMIN` / `AUTH_BOOTSTRAP_PASSWORD`，
    且库中尚无 admin 时创建初始管理员
  - `AuthenticateOrBootstrap` 在用户表为空时，用本次提交的凭据在事务中创建
    admin 并登录（事务内二次校验，防 TOCTOU 竞态）

## 关键类型

| 类型 | 说明 |
|------|------|
| `AuthService` | 用户管理与认证服务，`New(db)` 创建 |
| `CreateUserInput` | 创建用户参数（Username/Password/Email/Role/DisplayName） |
| `UpdateProfileInput` | 资料更新参数（Email/DisplayName/Avatar，指针字段，仅更新非 nil） |

## 主要方法

`CreateUser` / `Authenticate` / `AuthenticateOrBootstrap` / `GetUser` /
`GetUserByUsername` / `ListUsers` / `UpdateRole` / `UpdatePassword` /
`UpdateProfile` / `EnableUser` / `DisableUser` / `DeleteUser` / `Can` / `DB`

约束：用户名非空、密码至少 6 位、角色必须是合法角色。

## 领域错误

```go
auth.ErrUserNotFound
auth.ErrUserExists
auth.ErrInvalidCredentials
auth.ErrUserDisabled
auth.ErrInvalidRole
```

## 角色与状态常量

```go
auth.RoleAdmin  // "admin"
auth.RoleMember // "member"

auth.StatusActive   // "active"
auth.StatusDisabled // "disabled"
```

## 权限常量

```go
auth.PermBotCreate    // "bot.create"
auth.PermBotManage    // "bot.manage"
auth.PermUserManage   // "user.manage"
auth.PermBotUse       // "bot.use"
auth.PermSystemConfig // "system.config"
```

## 使用示例

```go
svc := auth.New(db)
user, err := svc.Authenticate(ctx, "admin", "password123")
if svc.Can(user, auth.PermBotManage) {
    // 管理员操作
}
```
