# skill — 技能系统

从文件系统动态发现和加载技能（Skill），为 Bot 提供可扩展的专业能力。

## 功能

- **技能发现**：扫描指定目录下的 `SKILL.md` 文件，自动发现可用技能
- **动态加载**：运行时加载/卸载技能，支持热重载（文件变更自动刷新）
- **提示词适配**：将技能描述转换为系统提示词段落
- **工具注入**：技能可声明需要的工具，自动注册到 ToolManager
- **多根目录**：支持从多个目录加载技能（内置 + 自定义）

## 关键类型

| 类型 | 说明 |
|------|------|
| `Manager` | 技能管理器（发现/加载/卸载） |
| `Skill` | 技能定义（名称/描述/提示词/工具） |
| `Loader` | 技能加载器 |
| `Config` | 技能系统配置（RootDirs/EnableHotReload） |
| `SkillHotReloader` | 热重载监视器 |

## 使用示例

```go
mgr := skill.NewManager(skill.Config{
    RootDirs: []string{"./skills", "./custom-skills"},
}, logger)
mgr.LoadAll()

// 获取已加载的技能
skills := mgr.List()
```
