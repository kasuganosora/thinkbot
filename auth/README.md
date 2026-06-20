# auth — 用户认证与权限管理

提供用户 CRUD、密码验证（bcrypt）、角色权限检查能力。

## 功能

- **用户管理**：创建、查询、更新、禁用、删除用户
- **密码认证**：bcrypt 哈希存储，登录自动更新最后登录时间
- **角色权限**：`admin`（全部权限）和 `member`（仅 `bot.use`）
- **引导管理员**：检测环境变量 `AUTH_BOOTSTRAP_ADMIN` / `AUTH_BOOTSTRAP_PASSWORD` 自动创建初始管理员

## 关键类型

| 类型 | 说明 |
|------|------|
| `AuthService` | 用户管理与认证服务 |
| `CreateUserInput` | 创建用户参数 |

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
