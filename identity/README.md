# Identity — 跨平台身份绑定

跨平台身份绑定模块，允许用户通过一次性授权码将 Telegram / Misskey 等非 Web 渠道的身份映射到内部用户系统。

## 流程

```
Web 页面 → 生成 TB-XXXX-XXXX 授权码（5分钟有效）
                ↓
Telegram/Misskey → 发送授权码 → BindStage(Order=3) 拦截
                ↓
验证授权码 → 创建 IdentityMapping → 回复绑定结果
                ↓
后续消息 → AdminChecker 通过映射查角色 → 命令权限判断
```

## 组件

| 文件 | 职责 |
|------|------|
| `code.go` | 授权码生成（`TB-XXXX-XXXX` 格式，安全字母表）+ 格式匹配 |
| `service.go` | `BindService`：生成码、消费码、查/删映射 |
| `admin_checker.go` | `IdentityAdminChecker`：通过映射解析管理员身份 |
| `bind_stage.go` | `BindStage`：Pipeline Stage（Order=3），拦截授权码消息 |
| `module.go` | fx 模块（DI + AutoMigrate） |

## 授权码格式

```
TB-XXXX-XXXX
```

- 安全字母表：`23456789ABCDEFGHJKMNPQRSTVWXYZ`（排除 0/O/1/I/L 等易混淆字符）
- 搜索空间：29^8 ≈ 2×10^11
- 有效期：5 分钟
- 一次性：使用后不可重复

## API 端点

| 方法 | 路径 | 描述 |
|------|------|------|
| `POST` | `/api/bindcode` | 生成一次性授权码 |
| `GET` | `/api/bindcode` | 列出未使用且未过期的码 |
| `GET` | `/api/bindings` | 列出已绑定的平台身份 |
| `DELETE` | `/api/bindings/:id` | 解绑某个平台身份 |

## 数据表

### `bind_codes`
| 字段 | 类型 | 说明 |
|------|------|------|
| id | uint PK | 自增主键 |
| user_id | uint | 内部用户 ID |
| code | varchar(32) UNIQUE | 授权码 |
| used_at | timestamp NULL | 使用时间（NULL=未使用） |
| expires_at | timestamp | 过期时间 |
| created_at | timestamp | 创建时间 |

### `identity_mappings`
| 字段 | 类型 | 说明 |
|------|------|------|
| id | uint PK | 自增主键 |
| user_id | uint | 内部用户 ID |
| platform | varchar(64) | 平台类型 |
| platform_user_id | varchar(128) | 平台侧用户 ID |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

唯一约束：(platform, platform_user_id) — 一个平台账号只能绑定一个内部用户。

## Pipeline 集成

`BindStage` 应以 Order=3 注册，在 `CommandStage`(Order=5) 之前：

```go
stages := []core.StageInfo{
    identity.AsStageInfo(bindStage),      // Order=3
    command.AsStageInfo(commandStage),     // Order=5
    {Stage: llmStage, Order: 100, Enabled: true},
}
```

## AdminChecker 集成

```go
checker := identity.NewIdentityAdminChecker(bindSvc, authSvc)
// 在 command 模块中使用
stage := command.NewCommandStage("command", registry, checker, tp, logger)
```

`IdentityAdminChecker.IsAdmin(ctx, source, userID)` 逻辑：
- **Web 渠道**：`source` 以 `web` 开头 → `userID` 直接是内部用户 ID → 查库验证角色
- **其他渠道**：通过 `IdentityMapping` 查找绑定的内部用户 → 查库验证角色
