# auth — 用户认证与权限管理

提供用户 CRUD、密码验证（bcrypt）、角色权限检查能力。不含会话/令牌管理——登录态由 `api` 层的 Cookie 会话负责，本包只做凭据校验与用户表操作。

## 功能

- **用户管理**：创建、查询、更新（角色/密码/资料）、启用/禁用、删除用户
- **密码认证**：bcrypt（`DefaultCost`）哈希存储，`Authenticate` 成功自动更新 `LastLoginAt`（更新失败不影响登录）；禁用用户返回 `ErrUserDisabled`，用户不存在与密码错误统一返回 `ErrInvalidCredentials`（避免响应差异泄露账号存在性）
- **角色权限**：`admin`（全部权限）和 `member`（仅 `bot.use`），静态映射于 `role.go`
- **引导管理员**（两条显式路径，空库首登不再自动成 admin）：
  - fx `OnStart` 钩子（`module.go`）检测环境变量 `AUTH_BOOTSTRAP_ADMIN` / `AUTH_BOOTSTRAP_PASSWORD`，且库中尚无 admin 时创建初始管理员（创建失败仅记日志，不阻断启动）
  - `AUTH_BOOTSTRAP_TOKEN`（fx 模块读取后传入 `New` 的 `bootstrapToken`）：
    由 `AuthenticateOrBootstrap` 执行——空库首次登录必须以该令牌作为密码，
    才能在事务中创建首位 admin 并登录（事务内二次校验，防 TOCTOU 竞态）；
    用户名不能为空，邮箱/显示名不设置
  - 未配置任何 bootstrap 途径时空库首登返回 `ErrBootstrapDisabled`（fail-closed），
    防止任意可达登录接口的客户端静默接管实例

`AuthenticateOrBootstrap` 返回 `(user, bootstrapped bool, err)`：正常登录 `bootstrapped=false`；只有 `ErrInvalidCredentials` 可能触发自举分支，其他错误原样返回。

## 关键类型

| 类型 | 说明 |
|------|------|
| `AuthService` | 用户管理与认证服务，`New(db, bootstrapToken)` 创建（token 为空即禁用空库首登自举） |
| `CreateUserInput` | 创建用户参数（Username/Password/Email/Role/DisplayName） |
| `UpdateProfileInput` | 资料更新参数（Email/DisplayName/Avatar，指针字段，仅更新非 nil） |

## 主要方法

`CreateUser` / `Authenticate` / `AuthenticateOrBootstrap` / `GetUser` /
`GetUserByUsername` / `ListUsers`（按创建时间降序）/ `UpdateRole` / `UpdatePassword` /
`UpdateProfile` / `EnableUser` / `DisableUser` / `DeleteUser` / `Can` / `DB`

`DB()` 返回底层 `*gorm.DB`，仅供内部模块使用（如 `module.go` 的 bootstrap 检查）。

约束：用户名非空（trim 后）、`CreateUser`/`UpdatePassword` 密码至少 6 位、角色必须合法（空默认 `member`）；用户名重复返回 `ErrUserExists` 语义的冲突错误；`UpdateProfile` 全部字段为 nil 时是空操作；`Can` 对 nil 用户或非 active 用户返回 false。

## 其他导出符号

| 符号 | 说明 |
|------|------|
| `Module` / `NewModule` / `AuthParams` | fx 模块：提供 `*AuthService` 并注册生命周期钩子 |
| `HashPassword(plain)` / `VerifyPassword(hash, plain)` | bcrypt 哈希与比对（`password.go`） |
| `AllRoles()` / `IsValidRole(role)` | 角色枚举与校验（`role.go`） |
| `HasPermission(role, perm)` / `PermissionsForRole(role)` | 角色权限查询 |

## 领域错误

```go
auth.ErrUserNotFound      // 按 ID/用户名查询不存在
auth.ErrUserExists        // 用户名重复（CreateUser 语义）
auth.ErrInvalidCredentials // 密码错误或用户名不存在
auth.ErrUserDisabled      // 用户被禁用
auth.ErrInvalidRole       // UpdateRole 角色非法
auth.ErrBootstrapDisabled // 未配置 bootstrap 途径时空库首登被拒
```

校验类错误（参数为空、密码过短、角色非法的 `CreateUser` 分支）经 `util/errs` 返回 `BadRequest`/`Conflict`，不是上述哨兵。

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
svc := auth.New(db, "") // 第二参数为 bootstrap token，空串 = 禁用空库首登自举
user, err := svc.Authenticate(ctx, "admin", "password123")
if err == nil && svc.Can(user, auth.PermBotManage) {
    // 管理员操作
}
```
