# skills/ — 技能定义

本目录存放技能（Skill）定义：每个一级子目录一个技能，核心文件为其中的 `SKILL.md`（YAML front matter + Markdown 正文）。`name`、`description` 为必填字段，技能名以 front matter 的 `name` 为准，目录名任意。技能目录还可附带可选的 `scripts/`、`references/`、`assets/` 资源目录。

当前包含：`apple-design/`、`emil-design-eng/`（两者均只有 `SKILL.md`，暂无资源子目录）。

- `apple-design`：Apple 风格界面与流体动效（弹簧动画、手势交互、半透明材质等）的设计知识，面向 Web 平台
- `emil-design-eng`：Emil Kowalski 的设计工程理念（UI 打磨、组件设计、动效取舍）

技能清单（名称 + 描述）注入 system prompt，正文经 `use_skill` 工具按需加载，详见 [`skill/README.md`](../skill/README.md)。

## 加载逻辑（以 `skill/` 包代码为准）

- `skill/loader.go`：`Loader.LoadAll` / `LoadAndRegister` 扫描本目录的一级子目录，读取各自的 `SKILL.md` 并注册到 `SkillManager`；缺少 `SKILL.md` 或必填字段的子目录会被跳过并记录告警，目录不存在时静默返回 0 条。
- `skill/discovery.go`：`Discover` 基于同一约定做自动发现，另支持多根目录、递归深度与可选热重载。
- 调用入口：每个 Bot 启动时 `BotService` 调用 `SetupSkills`，加载本目录（内置）以及 `data/skills/{botID}/`（托管）。管理台 `/api/skills` 仍扫描本目录作为全局目录。

注意：本目录与仓库中的 `skill/`（Go 源码包）同名但互不相关；两个技能的 `SKILL.md` 均由 git 跟踪，随仓库分发并在启动时加载。
