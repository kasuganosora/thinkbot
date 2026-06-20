# Command 模块

Pipeline 命令拦截 Stage，在 LLM 之前处理以 `/` 开头的斜杠命令。

## 设计

CommandStage 以 **Order=5** 注册在 Pipeline 最前端，在所有其他 Stage 之前执行。

```
消息流：  Ingress → [CommandStage O=5] → SessionStage O=50 → MemoryStage O=100 → PromptStage O=200 → LLMStage O=500
                         │
                         ├─ 是命令？→ 执行 handler → Abort(跳过后续) → Dispatcher 回复
                         └─ 不是命令 → 正常放行
```

### 核心机制

- **命令拦截**：消息以 `/` 开头时，解析命令名和参数，在 Registry 中查找 handler
- **权限检查**：`AdminOnly` 命令需通过 `AdminChecker` 验证
- **Pipeline 中止**：命令执行后调用 `env.Abort(nil)`，跳过后续 Stage（不调用 LLM），但保留 `ActionReply` 供 Dispatcher 正常派发
- **未知命令**：未注册的 `/xxx` 命令会被放行，交给 LLM 自然处理

## 内建命令

| 命令 | 管理员 | 说明 |
|------|--------|------|
| `/help` | 否 | 显示所有可用命令 |
| `/clear` | 是 | 清空当前会话上下文（工作记忆） |
| `/compact [N]` | 是 | 压缩上下文，保留最近 N 条消息（默认 3） |
| `/status` | 否 | 显示当前会话状态 |

## 使用方式

### 方式一：便捷构造（推荐）

```go
import "github.com/kasuganosora/thinkbot/agent/command"
import "github.com/kasuganosora/thinkbot/agent/session"

// 创建 command stage（包含所有内建命令）
cmdStage := command.NewCommandStageWithBuiltins(
    command.NewStaticAdminChecker("telegram:admin-id"), // 管理员检查（格式：platform:userID）
    sessionMgr,        // Session 管理器
    resolver,          // Session 解析器
    3,                 // /compact 默认保留消息数
    tp,                // TracerProvider
    logger,
)

// 加入 Pipeline
stages := []core.StageInfo{
    {Stage: cmdStage, Order: command.DefaultOrder, Enabled: true},
    // ... 其他 stage
}
```

### 方式二：自定义命令

```go
registry := command.NewRegistry()

// 注册自定义命令
registry.MustRegister(&command.CommandFunc{
    CmdName: "ping",
    CmdDesc: "测试 Bot 是否在线",
    Fn: func(ctx context.Context, env *core.Envelope, args string) (*command.CommandResult, error) {
        return &command.CommandResult{Reply: "pong! 🏓", OK: true}, nil
    },
})

// 创建 stage
stage := command.NewCommandStage("command", registry, checker, tp, logger)
```

### 方式三：fx 模块

```go
app := fx.New(
    command.Module,
    // 覆盖默认 AdminChecker
    fx.Provide(func() command.AdminChecker {
        return command.NewStaticAdminChecker("telegram:admin-id-1", "telegram:admin-id-2")
    }),
    // 通过 pipeline 注册
    command.ProvideStage(command.DefaultOrder),
)
```

## 接口

### AdminChecker

```go
type AdminChecker interface {
    IsAdmin(ctx context.Context, source, userID string) bool
}
```

内建实现：
- `StaticAdminChecker` — 基于 `source:userID` 组合（如 `"telegram:123456"`）
- `AllowAllChecker` — 始终返回 true（测试用）
- `AdminCheckerFunc` — 函数适配器
- `identity.IdentityAdminChecker`（外部）— 通过身份映射查内部用户再验证角色

接入 `auth.AuthService` 的示例：

```go
checker := command.AdminCheckerFunc(func(ctx context.Context, source, userID string) bool {
    // source: "telegram"、"misskey"、"web" 等
    // userID: 平台侧用户 ID（Web 渠道为内部用户 ID 的字符串形式）
    user, err := authSvc.GetUserByUsername(ctx, userID)
    if err != nil {
        return false
    }
    return user.Role == auth.RoleAdmin
})
```

### CommandHandler

```go
type CommandHandler interface {
    Name() string        // 命令名（不含 /）
    Description() string // 描述（/help 使用）
    AdminOnly() bool     // 是否需要管理员权限
    Execute(ctx context.Context, env *core.Envelope, args string) (*CommandResult, error)
}
```
