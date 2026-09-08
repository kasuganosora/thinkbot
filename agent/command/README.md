# Command 模块

Pipeline 命令拦截 Stage，在 LLM 之前处理以 `/` 开头的斜杠命令。

## 设计

CommandStage 拦截以 `/` 开头的命令消息，在 LLM 之前执行并短路后续 Stage。

注册位置分两种：

- **fx / StageInfo 路径**：默认 `DefaultOrder = 5`（`AsStageInfo` / `ProvideStage`），早于 Session(50) / Memory(100) / Prompt(200) / LLM(500)
- **生产 tg/misskey 管线**（`api.BotService.buildPipeline`）：显式 `pb.Add(4, cmdStage)`，紧随授权码绑定 `BindStage`（Order=3）之后、enrich/LLM 之前

```
消息流：  Ingress → [BindStage O=3] → [CommandStage O=4] → enrich O=40+ → … → LLM O=100
                                      │
                                      ├─ 已注册命令？→ AdminOnly / RequireBound 校验 → 执行 handler → Abort(跳过后续) → Dispatcher 回复
                                      └─ 非命令 / 未注册命令 → 正常放行（交给 LLM）
```

### 核心机制

- **命令解析**：`Parse(text)` 返回 `*ParsedCommand{Name, Args}`；文本不以 `/` 开头或命令名为空时返回 nil，命令名统一转小写
- **命令拦截**：解析成功后在 Registry 中查找 handler
- **权限检查**：`AdminOnly` 命令需通过 `AdminChecker` 验证；`checker` 为 nil 时一律拒绝（安全默认）
- **Pipeline 中止**：命令执行（或被拒绝 / 执行出错）后调用 `env.Abort(nil)`，跳过后续 Stage（不调用 LLM），但保留 `ActionReply` 供 Dispatcher 正常派发
- **回复目标**：优先取 `Message.Metadata["reply_target"]`，缺省回退到 `Message.Channel`
- **未知命令**：未注册的 `/xxx` 命令会被放行，交给 LLM 自然处理

## 内建命令

| 命令 | 管理员 | 需绑定 | 说明 |
|------|--------|--------|------|
| `/help` | 否 | 否 | 显示所有可用命令（🔗/🔒 按需输出脚注） |
| `/chatid` | 否 | **是** | 显示当前会话/群 ID、发送者 ID、平台账号（用于工具权限按群配置） |
| `/clear` | 是 | 否 | 清空当前会话上下文（工作记忆） |
| `/compact [N]` | 是 | 否 | 压缩上下文，保留最近 N 条消息（`DefaultKeepRecent` = 3） |
| `/status` | 否 | 否 | 显示当前会话状态（ID、状态、消息数、话题、创建/最后活动时间） |

`RegisterBuiltins(registry, accessor, keepRecent)` 负责注册以上命令。
`/help` 与 `/chatid` 始终注册（不依赖 session）；`accessor` 为 nil 时跳过依赖 session 的 `/clear`、`/compact`、`/status`。

> `/chatid` 原为 AdminOnly，自 f9cb9ac 起改为 RequireBound：任何已绑定账号的用户都可获取标识信息，未绑定者一律拒绝。

## 生产管线接入（tg/misskey）

`api.BotService` 在每个 bot 的 pipeline 中显式装配（`buildPipeline`，多 bot 管线不走 `pipeline_stages` fx 分组）：

- **条件**：注入了 `identity.BindService` 才接入（`bindSvc != nil`），否则整个命令拦截不注册
- **位置**：Order=4，紧随授权码绑定 `BindStage`（Order=3）
- **命令集**：只注册 `/chatid`——其余内建命令（/clear 等）依赖 web 会话，tg/misskey 入站无对应会话，注册反而暴露无效命令
- **绑定判定**：`bindSvc.ResolveBySource(ctx, source, userID)` 查平台→内部账号映射，无映射或出错均视为未绑定（拒绝）

```go
cmdRegistry := command.NewRegistry()
cmdRegistry.MustRegister(command.NewChatIDHandler())
cmdBinder := command.BindingCheckerFunc(func(ctx context.Context, source, userID string) bool {
    mapping, err := bindSvc.ResolveBySource(ctx, source, userID)
    return err == nil && mapping != nil
})
cmdStage := command.NewCommandStage("command", cmdRegistry, nil, tp, logger).SetBinder(cmdBinder)
pb.Add(4, cmdStage)
```

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
    command.AsStageInfo(cmdStage), // 等价于 {Stage: cmdStage, Order: command.DefaultOrder, Enabled: true}
    // ... 其他 stage
}
// 需要自定义 Order 时用 command.AsStageInfoWithOrder(cmdStage, 8)
// 注意：含 RequireBound 命令（如 /chatid）时必须再链式 SetBinder，否则该类命令一律被拒绝
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

`command.Module` 提供 `*Registry`、一个**默认拒绝所有 AdminOnly 命令**的 `AdminChecker`，
以及通过 `NewCommandStageFromDeps` 构造并已注册内建命令的 `*CommandStage`。
`CommandStageParams.Accessor`（`*SessionManagerAccessor`）为可选依赖，提供后才会注册
`/clear`、`/compact`、`/status`。

```go
app := fx.New(
    command.Module,
    // 提供 SessionAccessor 以启用 session 相关命令
    fx.Provide(func(mgr *session.SessionManager, r session.SessionResolver) *command.SessionManagerAccessor {
        return &command.SessionManagerAccessor{Mgr: mgr, Resolver: r}
    }),
    // 覆盖默认 AdminChecker
    fx.Decorate(func(command.AdminChecker) command.AdminChecker {
        return command.NewStaticAdminChecker("telegram:admin-id-1", "telegram:admin-id-2")
    }),
    // 注册到 "pipeline_stages" 分组
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

### BindingChecker（RequireBound 用）

```go
type BindingChecker interface {
    // source 是消息来源标识（如 "telegram"、"web:1"），userID 是平台侧用户 ID。
    // 未绑定（无映射或解析失败）必须返回 false。
    IsBound(ctx context.Context, source, userID string) bool
}
```

- 通过 `CommandStage.SetBinder(b)` 注入，支持链式调用；不设置时所有 RequireBound 命令被拒绝
- `BindingCheckerFunc` 为函数适配器
- 生产实现基于 `identity.BindService.ResolveBySource`（见上文「生产管线接入」）

### CommandHandler

```go
type CommandHandler interface {
    Name() string        // 命令名（不含 /）
    Description() string // 描述（/help 使用）
    AdminOnly() bool     // 是否需要管理员权限
    RequireBound() bool  // 是否要求发送者已绑定 thinkbot 内部账号（与 AdminOnly 正交）
    Execute(ctx context.Context, env *core.Envelope, args string) (*CommandResult, error)
}
```

`CommandResult{Reply string, OK bool}`：`Reply` 为空表示不回复。
`CommandFunc` 是把函数适配为 `CommandHandler` 的结构体（字段 `CmdName` / `CmdDesc` / `CmdAdminOnly` / `CmdRequireBound` / `Fn`）。

### SessionAccessor

```go
type SessionAccessor interface {
    GetFromEnvelope(env *core.Envelope) *session.Session
}
```

内建实现 `SessionManagerAccessor` 先读 Envelope KV 中的 `session.id`（上游 SessionStage 注入），
未命中则用 `Resolver` 自行解析并 `GetOrCreate`。
