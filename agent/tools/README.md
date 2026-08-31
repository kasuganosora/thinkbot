# tools — Bot 工具管理

管理 LLM 可调用的工具（Function Call）及其提示词。支持静态注册与动态提供、场景过滤、权限策略，
并通过 `prompt.Registry` 的 Section 机制自动把工具说明注入 system prompt。

## 功能

- **工具注册**：`ToolManager.Register(ToolDef)` 静态注册；`AddProvider(ToolProvider)` 按请求动态提供
- **Channel 专属工具**：Channel 实现 `ChannelToolProvider`，`BotService` 在 StartBot 时一次性收集注册
- **场景过滤**：`ToolDef.Scopes`（`private` / `group` / `subagent`），空表示全场景可用
- **权限策略**：`ToolPolicy`（黑名单模式）与 `PatternPolicy`（glob 通配 + allow/deny/ask）
- **提示词生成**：工具描述段落自动注册到 `prompt.Registry`（header=300、rules=301、单工具描述=310）
- **流水线集成**：`ToolsStage`（Order=150）为可选诊断 Stage，仅提前解析工具用于 trace/日志

> 注意：工具注入 LLM 并不依赖 `ToolsStage`，`LLMConfig.ToolResolver` 会在 LLM 调用时自动解析。

## 关键类型

| 类型 | 说明 |
|------|------|
| `ToolManager` | 统一入口，整合 `ToolRegistry` + `ToolPromptManager` + 策略 |
| `ToolRegistry` | 线程安全注册中心，管理静态工具与动态 Provider |
| `ToolDef` | 工具完整定义：内嵌 `llm.Tool` + `Category`/`Scopes`/`PromptSection`/`RequireApproval` |
| `ToolProvider` / `ToolFunc` | 动态工具提供者接口及其函数适配器 |
| `ChannelToolProvider` | Channel 提供专属工具（返回 `[]ToolDef`） |
| `ToolSessionContext` | 每次解析的会话上下文（BotID/Channel/ChatType/UserID/MessageID/IsSubagent/IsSystem/SourceChannelType/Extra） |
| `ToolPolicy` / `ToolRule` | 黑名单策略：按 channel + chatType 限定，支持用户白名单绕过 |
| `ToolPolicyProvider` / `ToolPolicyFunc` | 运行时动态获取策略；`NewStorePolicyProvider` 从配置实时读取 |
| `PatternPolicy` / `PatternRule` | glob 模式策略，决策为 `PermAllow`/`PermDeny`/`PermAsk`，后匹配规则覆盖前者 |
| `ToolPromptManager` | 将 `ToolPromptSection` 注册到 `prompt.Registry` |
| `ToolsStage` | Pipeline Stage（Order=150，诊断用途） |
| `ToolInfo` | 已注册工具的只读快照，供列表展示 / 自省 |
| `ToolAccessEvaluator` | 基于完整会话上下文（platform + userID）的访问控制，设置后取代 legacy `ToolPolicyProvider` |
| `DynamicCategory` | 动态 ToolProvider（MCP/浏览器）工具的统一分类标记（"dynamic"） |

## 权限策略

两套策略并存，均实现 `FilterTools`：

- `ToolPolicy`：默认全放行，`Rules[].Disabled` 列出禁用工具，`AllowedUsers` 可放行特定用户。
  存储键为 `tools.<botID>.policy`（JSON）。
- `PatternPolicy`：支持 `*` 通配与 `a|b` 或语法，`DefaultDecision` 缺省为 `PermAllow`。
  存储键为 `tools.pattern.<botID>.policy`。
  预设工厂：`ReadOnlyPolicy()`、`SafePolicy()`、`SubagentPolicy()`、`GroupChatPolicy()`。

`ToolManager` 在 `ResolveTools` 中优先应用 `ToolAccessEvaluator`（经 `SetAccessEvaluator` 注入，能感知 platform 与 userID），未设置时回退到 legacy `ToolPolicyProvider`；构造时若传入 `PolicyStore`
则自动接入 `NewStorePolicyProvider`，无需手动 `SetPolicyProvider`。`ListTools`/`ListAllTools` 返回静态（或含动态 provider）工具的 `[]ToolInfo` 快照，供权限管理界面自省。

## 使用示例

```go
mgr := tools.NewToolManager(promptReg, cfgStore, logger)

// 注册内置工具
_ = mgr.RegisterMany(tools.CurrentTimeTool(), tools.EchoTool())

// 自定义工具
_ = mgr.Register(tools.ToolDef{
    Category: "utility",
    Scopes:   []string{"private"},
    Tool: tools.BuildTool("calculate", "计算表达式", schema,
        func(ctx *llm.ToolExecContext, input any) (any, error) { ... }),
})

// 全局提示词段落
mgr.SetHeaderSection(tools.DefaultToolHeaderSection([]string{"current_time", "echo"}))
mgr.SetRulesSection(tools.DefaultToolRulesSection())

// 解析当前会话可用工具
list, _ := mgr.ResolveForEnvelope(ctx, env)
```
