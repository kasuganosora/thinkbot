# prompt — 系统提示词构建

系统提示词的模板化管理、变量替换和动态组装。

## 设计理念

- **Section** 是提示词的最小组装单元（段落），每个 Section 有独立的 `Order` 排序权重
- **Variable** 是 Section 中可替换的变量占位符（`{{.VarName}}`），支持静态值 / Envelope KV / 动态函数三种来源
- **Registry** 是 Section 的线程安全注册中心，支持运行时动态增删
- **Assembler** 负责解析变量、渲染模板、按 Order 拼接最终 prompt

## 关键类型

| 类型 | 说明 |
|------|------|
| `Section` | 提示词段落（Name/Order/Content/Enabled/Conditional/Variables） |
| `Variable` / `VariableSource` | 模板变量与来源（`SourceStatic` / `SourceEnvelopeKV` / `SourceFunc`） |
| `Registry` | Section 注册中心（`Register`/`RegisterMany`/`Unregister`/`Get`/`List`/`Len`/`Metrics`） |
| `Assembler` / `AssemblerConfig` | 组装器与配置 |
| `AssemblyContext` | 组装上下文（Envelope KV 快照 + BotID/Channel/ChatType/UserID/Timestamp） |
| `AssemblyResult` | 组装结果（Prompt / SectionsUsed / SectionsSkipped / 变量统计 / Truncated） |
| `FileLoader` | 从目录加载 `{order}_{name}.md` 模板文件并注册为 Section |
| `SoulLoader` / `SoulLoaderConfig` | SOUL.md 人格加载器（自动创建 + 安全扫描 + 热重载） |
| `SoulStore` / `SoulStat` | SOUL.md 文件 IO 抽象（默认 os 实现，可注入 sandbox 实现） |
| `PromptStage` / `PromptStageConfig` | Pipeline Stage，组装并注入 `system.prompt` |
| `ScanMode` / `ScanFinding` | 上下文文件安全扫描模式与发现项 |

## 文件结构

```
prompt/
├── prompt.go       # Variable / Section / AssemblyContext / Registry / Assembler
├── loader.go       # FileLoader：从 Markdown 目录加载 Section
├── soul.go         # SoulLoader：SOUL.md 加载 + 热重载 + SoulStore 抽象
├── prompt_scan.go  # ScanForThreats：提示注入 / C2 / 数据渗出 / 不可见字符检测
└── stage.go        # PromptStage：Pipeline 集成
```

## Section 与 Order 约定

| Order 范围 | 用途 |
|-----------|------|
| 0-99 | 核心身份 / 角色定义（SOUL.md 默认 Order=0） |
| 100-199 | 行为规则 / 约束 |
| 200-299 | 上下文信息（记忆、会话历史等） |
| 300-399 | 工具 / 能力声明 |
| 400-499 | 输出格式指令 |
| 500+ | 附加指令 |

## 使用示例

```go
registry := prompt.NewRegistry()
registry.Register(prompt.Section{
    Name:    "rules",
    Order:   100,
    Content: "You are talking in {{.ChatType}} chat.",
    Enabled: true,
    Variables: []prompt.Variable{{
        Name:        "ChatType",
        Source:      prompt.SourceEnvelopeKV,
        EnvelopeKey: "chat.type",
        Default:     "private",
    }},
})

asm := prompt.NewAssembler(registry, prompt.DefaultAssemblerConfig())
result, err := asm.Assemble(&prompt.AssemblyContext{Values: kv, BotID: "bot1"})
```

`AssemblerConfig`：`SectionSeparator`（默认 `"\n\n"`）、`TrimEmpty`（默认 true，跳过渲染后为空的段落）、
`StrictMode`（Required 变量解析失败时报错）、`MaxPromptLength`（超限时从高 Order 段落开始移除，
单段落仍超限则硬截断）。

`Assemble` 可传入 `extraSections...` 作为临时段落，不会写入 Registry。

## FileLoader

```go
loader := prompt.NewFileLoader("./prompts", registry)
n, err := loader.LoadAll()   // 目录不存在时静默返回 0
```

- 文件名格式 `{order}_{name}.md`（如 `000_identity.md`、`100_rules.md`）；
  无数字前缀时 Order 默认 500
- 支持 `---` 分隔的简易 front matter（`key: value`），`enabled: false` 可禁用段落
- 内容中的 `{{.VarName}}` 自动发现为变量，默认来源 `SourceEnvelopeKV`、key 即变量名
- front matter 可细化变量：`var_{name}_source`（`static`/`env`）、`var_{name}_key`、
  `var_{name}_default`、`var_{name}_required`、`var_{name}_value`

## SoulLoader

SOUL.md 是 bot 人格的权威来源，注入到 system prompt 最高优先级位置（默认 `identity` / Order=0）。

```go
cfg := prompt.DefaultSoulLoaderConfig() // identity / Order=0 / 5s 轮询 / 20000 字节 / ScanModeWarn
cfg.BotID = "bot1"
soul := prompt.NewSoulLoader(cfg, registry).WithLogger(logger)

if err := soul.Load(); err != nil { /* ... */ }
soul.StartWatcher(ctx)
defer soul.Stop()
```

**路径解析**：`Path` 留空时由 `DefaultSoulPath(botID)` 解析 —— 优先二进制目录下的
`{botID}/SOUL.md`，回退到当前工作目录；`botID` 为空时退化为 `SOUL.md`。

**自动创建**：文件不存在时写入 `DefaultSoulContent` 模板，用户编辑后热重载生效。
文件被删除时 watcher 会重建默认模板。

**热重载**：轮询文件 mtime（不依赖 fsnotify），变更时重新注册 Section；
`SetOnReload(cb)` 可注册重载回调。

**内容截断**：超过 `MaxContentBytes` 时保留头部 70% + 尾部 20%，中间标注省略字节数。

**SoulStore**：默认 `osSoulStore` 直接操作宿主文件系统。docker 持久容器（DooD）场景下，
调用方可注入基于 `sandbox.Workspace` 的实现，使读写和 mtime 轮询落到容器内真实文件。

**原始内容读写**：`ReadRaw(ctx)` / `WriteRaw(ctx, data)` 读写含 front matter 的原始内容
（`ReadRaw` 不截断），与 `Load()` 走同一 SoulStore 后端。供 `agent/bot` 的 soul 工具
（bot 自改人格）使用；写入后调用方手动 `Load()` 或等待 watcher 在 `ReloadInterval` 内自动重载。

其他方法：`Path()`、`Stat()`、`Content()`、`Loaded()`、`Variables()`、`ModTime()`、`ScanMode()`。

## 安全扫描

`prompt_scan.go` 对注入 system prompt 的上下文文件（如 SOUL.md）做威胁检测：

```go
findings := prompt.ScanForThreats(content)
if prompt.HasThreats(content) {
    log.Warn(prompt.FindingsSummary(findings))
}
```

检测类别：经典提示注入（`ignore previous instructions` 等）、系统提示覆盖、
HTML 注释 / 隐藏 div 注入、C2 / promptware 模式（含已知框架名）、数据渗出
（curl/wget + secrets、读取 `.env` / credentials）、
不可见 Unicode 字符（零宽空格、RTL 覆盖、BOM、方向隔离符等）。

`SoulLoaderConfig.ScanMode` 控制行为：`ScanModeOff` 不扫描、`ScanModeWarn` 告警但仍加载（默认）、
`ScanModeBlock` 阻止加载并返回错误。

### 其他扫描辅助

- `StripInvisibleUnicode(content)` — 移除不可见 Unicode 字符，返回清洗后文本与被移除码位列表
  （已去重）。与 `ScanForThreats` 复用同一字符集（检测能发现的就能移除），且幂等，
  适合反复清洗的场景（如 workflow 闭环每轮回注审查意见）
- `ScanFeedback(content)` — 审查意见场景的精简规则集：代码审查文本天然会提到 C2、渗出等词，
  全量规则会大面积误报，故只保留注入类强信号；结果**只用于记录告警，不据此阻断**
  （workflow 的审查意见清洗在使用）

## Pipeline 集成

`PromptStage` 推荐放在 Order=200：在 MemoryStage 之后（记忆已注入）、LLMStage 之前。

```go
stage := prompt.NewPromptStage("prompt", asm, prompt.DefaultPromptStageConfig(), tp, logger)
```

**读取的 Envelope KV**：`memory.context`、`memory.entries_used`、`memory.compressed`、
`bot.config`、`bot.id`，以及 Registry 中各 Section 变量声明的 `EnvelopeKey`。

**写入的 Envelope KV**：`system.prompt`、`system.prompt.sections_used`、`system.prompt.length`。

**PromptStageConfig**：
- `BaseSectionName`（默认 `identity`）—— Registry 中不存在同名段落时，
  从 `bot.config` 的 `SystemPrompt` 自动生成 Order=0 的临时基础段落
- `InjectMemoryContext`（默认 true）—— 自动把 `memory.context` 注入为临时段落
- `MemorySectionOrder`（默认 200）
- `FallbackToConfig`（默认 true）—— 组装失败时回退到 BotConfig 的 SystemPrompt
- `OperatorSectionName` / `OperatorSectionOrder`（默认 `operator_instructions` / 10）——
  Registry 已有身份段落（SOUL.md）时，`bot.config` 的 `SystemPrompt` 以
  `# Operator Instructions` 段落紧跟身份之后注入；为空则不注入

**SOUL.md 与 system_prompt 的组合**（`ComposeIdentity` 同一规则，notify bot 模式也用它）：
SOUL.md 是身份（Order=0），bot 的 `system_prompt` 字段（`AgentConfig.EffectiveSystemPrompt`）
是运营者指令（Order=10）；没有 SOUL.md 时 `system_prompt` 本身即身份（原回退语义）。

**排序确定性**：段落按 `(Order, Name)` 排序。多个工具段落 / 全部技能共用同一 Order，
只按 Order 排序时先后会随 map 迭代变化，逐轮 system prompt 不同、前缀缓存失效。

### 主链路接线（api/botservice.go）

主 bot pipeline 在 Order=97（节奏门控 95 之后、LLM 100 之前）挂 `PromptStage`
（`api/prompt_wiring.go` 的 `newMainPromptStage`，`InjectMemoryContext=false`）。
system prompt 自上而下：

| 段落 | 来源 | 性质 |
|---|---|---|
| `identity` (0) | SOUL.md（SoulLoader，soul 工具改写后立即重载，外部编辑 5s 内按 mtime 重载） | 稳定 |
| `operator_instructions` (10) | bot 的 `system_prompt` | 稳定 |
| `skill_trigger` (150) | 技能使用说明（清单与正文经 `use_skill` 按需获取） | 稳定 |
| `tool_*` (300-325) | `ToolDef.PromptSection` 使用指引（不渲染自动生成的工具描述，描述已在工具 schema 里） | 稳定 |
| pipeline 警告 | LLMStage `core.MergeWarnings` | 逐轮 |
| 记忆召回 | LLMStage 追加 `KVMemoryRecall` | 逐轮 |
| 回复控制协议 | LLMStage（仅开启的 bot，非心跳） | 稳定但在逐轮内容之后 |

技能正文段落 `skill_<name>`（500）虽在 Registry 中，但主链路经 `AssemblerConfig.Exclude`
排除：全部已启用技能正文合计可达数十万字符，按需由 `use_skill` 以 tool_result 返回。

前四项构成稳定前缀（可被模型服务端前缀缓存）；稳定前缀内不得出现时间戳等逐轮变化内容
（SOUL.md 的 `{{.Var}}` 模板变量若取逐轮值会破坏缓存）。潜水模式下 LLMStage 用
`KVSoulContent` + 观察者指令整体替换 system prompt，SOUL 只出现一次。

**旁路事件**：通过 `outbound.EmitterFromContext(ctx)` 发射 `prompt.assembled`
（含长度、段落列表、变量统计、是否截断）。
