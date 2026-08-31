# skill — 技能系统

实现 Anthropic Skills 规范：Skill 是"增强系统提示词的知识与指令包"，与 Tool（执行能力）平级但职责不同。

- Tool = 给 LLM 执行能力（function calling）
- Skill = 给 LLM 知识、指令、工作流模板（注入 system prompt）

## 功能

- **文件系统加载**：每个 Skill 是一个目录，核心文件为 `SKILL.md`（YAML front matter + Markdown 正文）
- **附加资源**：自动扫描 `scripts/`、`references/`、`assets/`
- **自动发现 / 热重载**：`Discover` 支持多根目录与递归深度，`SkillHotReloader` 轮询 `SKILL.md` 修改时间触发重载
- **prompt 集成**：启用的 Skill 正文注册为 prompt Section（名称 `skill_<name>`，Order 500）；技能清单作为 `skill_trigger` Section 注入
- **工具集成**：通过 `use_skill` 工具（function calling）按需加载技能指令
- **状态持久化**：启用状态写入 `skill.<name>.enabled` 配置键

依赖方向：`skill → llm`、`skill → agent/tools`；不依赖 `agent/bot`、`agent/prompt`（通过 `RegistryAdapter`/`StoreAdapter` 解耦）。

## 目录结构与 SKILL.md

```
skills/
  pdf/
    SKILL.md         # 必需
    scripts/         # 可选
    references/      # 可选
    assets/          # 可选
```

```markdown
---
name: pdf
description: 处理 PDF 文件（提取文本、合并、拆分等）。当用户提到 PDF 时使用。
compatibility: [pdf_read_tool]
enabled: true
---

# PDF 处理技能
...
```

`name` 与 `description` 为必填，缺失则该目录被跳过；`enabled` 未指定时默认启用。

## 关键类型

| 类型 | 说明 |
|------|------|
| `Skill` / `SkillMeta` / `SkillInfo` | 技能实体、front matter、只读快照 |
| `SkillResources` | 附加资源路径（Scripts/References/Assets） |
| `SkillManager` | 技能注册、启用/禁用、prompt 注入、`use_skill` |
| `Loader` | 从单个根目录加载一级子目录中的 Skill |
| `DiscoveryConfig` / `DiscoveryResult` / `Discover` | 多根目录、可递归的自动发现 |
| `SkillHotReloader` | 基于修改时间轮询的热重载器 |
| `SkillToolProvider` / `RegisterTools` | `tools.ToolProvider` 适配（提供 `use_skill`） |
| `RegistryAdapter` / `PromptRegistryAdapter` | prompt Section 注入适配 |
| `StoreAdapter` / `ConfigStoreAdapter` | 配置持久化适配 |
| `DirectInjector` | 直拼模式：把技能内容拼接到 system prompt 字符串 |

## 使用示例

```go
mgr := skill.NewSkillManager(
    skill.NewPromptRegistryAdapter(registerSection, unregisterSection), // 可为 nil
    skill.NewConfigStoreAdapter(configStore),                           // 可为 nil
    logger,
)

// 从文件系统加载并注册
loader := skill.NewLoader("./skills", logger)
count, err := loader.LoadAndRegister(mgr)

// 注册技能清单段落 + use_skill 工具
sec := mgr.BuildTriggerSection(150)
_ = skill.RegisterTools(toolMgr, mgr)

// 查询与开关
infos := mgr.List()          // []SkillInfo，按名称排序
_ = mgr.Enable("pdf")        // 幂等，自动持久化
_ = mgr.Disable("pdf")
_ = mgr.Toggle("pdf")
names := mgr.EnabledNames()
```

`agent/bot.SetupSkills` 已封装以上接线（Registry + Loader + trigger Section + 工具注册）。

### 自动发现与热重载

```go
cfg := skill.DefaultDiscoveryConfig()
cfg.RootDirs = []string{"./skills", "./custom-skills"}
cfg.MaxDepth = 1
cfg.EnableHotReload = true

result := skill.Discover(cfg) // Discovered / Skipped / Errors

r := skill.NewSkillHotReloader(cfg, logger)
r.Start(func(res *skill.DiscoveryResult) { /* 重新注册 */ })
defer r.Stop()
```

## use_skill 工具

存在已启用技能时，`SkillToolProvider` 才向 LLM 暴露 `use_skill` 工具；主 Agent 与子 Agent 共用同一套技能工具。

调用 `use_skill(command: "<skill 名>")` 后：

1. 校验技能存在且已启用、正文非空（否则返回错误，并在未找到时附带可用技能列表）
2. 将技能正文注入 prompt Registry（多轮持久化）
3. 返回 `{status, skill, content}`，并按需附带 `scripts`、`references`、`baseDir`

## 启用状态优先级

`Register` 时按 Store 记录 > 已有内存状态 > `SKILL.md` 的 `enabled` > 默认启用 的顺序决定。
`LoadEnabledStates()` 可在批量注册后统一应用 Store 中的状态，`SaveEnabledStates(ctx)` 批量回写。

## 已废弃

- `TriggerIfNeeded`：旧的 `<use_skill: name>` 文本标签协议，已由 `use_skill` 工具替代
- `InjectSkillContent`：请改用 `UseSkill`（同时返回内容并注入 Registry）
