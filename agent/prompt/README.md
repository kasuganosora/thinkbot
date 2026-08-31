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

其他方法：`Path()`、`Content()`、`Loaded()`、`Variables()`、`ModTime()`。

## 安全扫描

`prompt_scan.go` 对注入 system prompt 的上下文文件做威胁检测：

```go
findings := prompt.ScanForThreats(content)
if prompt.HasThreats(content) {
    log.Warn(prompt.FindingsSummary(findings))
}
```

检测类别：经典提示注入（`ignore previous instructions` 等）、系统提示覆盖、
HTML 注释 / 隐藏 div 注入、C2 / promptware 模式、数据渗出（curl/wget + secrets、读取 `.env`）、
不可见 Unicode 字符（零宽空格、RTL 覆盖等）。

`SoulLoaderConfig.ScanMode` 控制行为：`ScanModeOff` 不扫描、`ScanModeWarn` 告警但仍加载（默认）、
`ScanModeBlock` 阻止加载并返回错误。

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

**旁路事件**：通过 `outbound.EmitterFromContext(ctx)` 发射 `prompt.assembled`
（含长度、段落列表、变量统计、是否截断）。
