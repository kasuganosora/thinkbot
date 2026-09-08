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
| `code.go` | 授权码生成（`TB-XXXX-XXXX` 格式，安全字母表）+ 格式匹配（`extractPlatform` 平台名大小写不敏感，统一归一为小写，避免 `"Telegram"` 大写 Source 导致映射查不到） |
| `service.go` | `BindService`：生成码（同一用户可并存多个有效码，旧码不因新码失效）、消费码（事务 + `used_at IS NULL` 乐观锁，防并发重放）、查映射（`ResolveMapping` / `ResolveBySource`）、列出/删除绑定；消费与拦截均先经 `NormalizeCode` 归一化（大小写、空格/点分隔），同平台账号重复绑定同一用户幂等返回 |
| `admin_checker.go` | `IdentityAdminChecker`：通过映射解析管理员身份；另附 `StaticAdminChecker` / `AllowAllChecker` / `DenyAllChecker` 便捷实现与 `IsAdminUser` 辅助 |
| `bind_stage.go` | `BindStage`：Pipeline Stage（Order=3），拦截授权码消息、回复绑定结果并中止 Pipeline（不流入 LLM）；含 OTel span 与 trace_id 关联日志 |
| `module.go` | fx 模块：提供 `*BindService` / `*BindStage`，经 `pipeline.ProvideStageInfo` 自动注册 Stage（Order=3），OnStart 执行 `Migrate`（AutoMigrate `bind_codes` / `identity_mappings`） |

## 授权码格式

```
TB-XXXX-XXXX
```

- 字母表：`23456789ABCDEFGHJKMNPQRSTVWXYZ`（30 个字符，排除 0/O/1/I/L 等易混淆字符；注意 `V` 在表内）
- 搜索空间：30^8 ≈ 6.6×10^11。`code.go` 文件头注释中的 `29^8` 是笔误（字符数实为 30），`bindCodeRegex` 也按 `[A-Z2-9]` 全集匹配
- 总长度 12 字符（含 2 个连字符），8 个随机字符；随机源 `crypto/rand`，取模上界即字母表长度，无偏
- 有效期：5 分钟
- 一次性：使用后不可重复（事务内 `WHERE used_at IS NULL` 乐观锁防并发重放）

## 领域错误

```go
identity.ErrCodeNotFound     // 授权码不存在
identity.ErrCodeExpired      // 授权码已过期
identity.ErrCodeUsed         // 授权码已被使用
identity.ErrAlreadyBound     // 平台账号已绑定其他用户
identity.ErrMappingNotFound  // 映射不存在（解绑时）
```

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

## identity_mappings 的下游消费方

映射表是跨平台身份的共享事实源，除本包外还有两类只读消费者：

- **`agent/command` 的 `BindingChecker`**：命令声明 `RequireBound`（目前仅 `/chatid`）时，
  据此判断平台用户是否已绑定 thinkbot 内部账号。生产实现在 `api/botservice.go`，
  用 `BindService.ResolveBySource` 适配为 `command.BindingCheckerFunc` 后经
  `CommandStage.SetBinder` 注入，不在 `agent/command` 模块内自动接线
- **`toolperm` 的多身份 OR 匹配**：评估工具权限时按「平台 + 平台数字 ID」反查
  （`identity_mappings` → `users`）已绑定用户的 thinkbot 账号名，并入候选身份集
  （与平台数字 ID、平台账号名是 OR 关系）——权限规则的 `user_id` 里填 thinkbot
  账号名即可识别已绑定用户，未绑定的平台账号不会因此命中。该路径直接读表
  （`resolveThinkbotUsernames`），不经 `BindService`

> 注意：`platform` 字段一律为小写（`extractPlatform` 归一化写入；未知前缀时原样返回，
> 大小写不保证）。若绕过本包直接写映射（如运维脚本），必须保持小写，否则按
> `telegram` 等小写前缀解析时查不到。

## Pipeline 集成

`BindStage` 以 Order=3 注册（`identity.DefaultOrder`），早于 `CommandStage`（`command.DefaultOrder = 5`）：

```go
stages := []core.StageInfo{
    identity.AsStageInfo(bindStage),      // Order=3
    command.AsStageInfo(commandStage),    // Order=5
    {Stage: llmStage, Order: 100, Enabled: true},
}
```

> `identity.Module` 已通过 `pipeline.ProvideStageInfo` 把 `BindStage` 自动注册进
> pipeline 的 stage 分组（Order=3，早于 CommandStage）。若装配 pipeline 时遗漏
> BindStage，授权码消息会直接流入 LLM 被当作闲聊，绑定机制完全失效——
> multi-bot pipeline 务必确认该 Stage 已接线。

## AdminChecker 集成

```go
checker := identity.NewIdentityAdminChecker(bindSvc, authSvc)
// 在 command 模块中使用
stage := command.NewCommandStage("command", registry, checker, tp, logger)
```

`IdentityAdminChecker.IsAdmin(ctx, source, userID)` 逻辑（任一依赖为 nil 直接返回 false，fail-closed）：
- **Web 渠道**：`source` 以 `web` 前缀开头 → `userID` 是内部用户 ID 的字符串形式 → 查库验证角色
- **其他渠道**：通过 `IdentityMapping` 查找绑定的内部用户 → 查库验证角色；未绑定即非管理员
- 两条路径均要求用户 `Status == active` 且 `Role == admin`

`command.AdminChecker` 与本包 `identity.AdminChecker` 为同签名镜像接口，避免 `identity → agent/command` 循环依赖。
