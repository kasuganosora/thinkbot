package bot

import (
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/skill"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 技能系统装配（Composition Root）
//
// 本文件是组合根的一部分，负责将 skill 领域模块接入 Bot 的工具与提示词基础设施。
//
// DDD 分层关系：
//
//	┌──────────────────────────────────────────────────────┐
//	│ agent/bot/skill.go  ← 此文件（组装层 / Composition Root） │
//	│   连接 skill ↔ tools ↔ prompt                         │
//	├──────────────────────────────────────────────────────┤
//	│ skill/             ← 领域层（Skill 生命周期管理）        │
//	│   不依赖 agent/bot、agent/tools                        │
//	├──────────────────────────────────────────────────────┤
//	│ agent/tools/       ← 基础设施层（ToolManager/Provider） │
//	│ agent/prompt/      ← 基础设施层（prompt.Registry）      │
//	└──────────────────────────────────────────────────────┘
//
// 适配器依赖方向：
//   - skill → llm（工具定义）
//   - skill → agent/tools（SkillToolProvider 实现 ToolProvider 接口）
//   - agent/bot → skill + agent/tools + agent/prompt（组合根组合所有依赖）
// ============================================================================

// SkillWireConfig 是技能系统的装配参数。
type SkillWireConfig struct {
	// SkillsDir 技能文件系统根目录路径。
	// 为空时跳过文件加载（仅管理运行时注册的技能）。
	SkillsDir string

	// Tools ToolManager，用于注册 use_skill 工具提供者。
	// 为 nil 时跳过工具注册（技能仍可通过 prompt 注入工作）。
	Tools *tools.ToolManager

	// Prompt prompt.Registry，用于注册触发提示词和技能内容 Section。
	// 必须非 nil。
	Prompt *prompt.Registry

	// Store 配置持久化适配器（可选）。
	// nil 时不持久化启用状态（仅内存管理）。
	// 通常传入 skill.NewConfigStoreAdapter(configStore)。
	Store skill.StoreAdapter

	// Logger 日志记录器。为 nil 时使用 noop logger。
	Logger *zap.SugaredLogger
}

// SetupSkills 在 Bot 组装层完成技能系统的完整接线。
//
// 该函数是组合根的一部分，负责将以下依赖连接到一起：
//
//  1. prompt.Registry ← SkillManager.RegistryAdapter
//     技能 Content 通过 prompt Section 自动注入 system prompt
//
//  2. 文件系统 → SkillManager
//     扫描 SkillsDir 下所有 SKILL.md 并注册
//
//  3. prompt.Registry ← 触发提示词
//     告知 LLM 可用技能列表 + use_skill 调用指令
//
//  4. ToolManager ← use_skill 工具提供者
//     LLM 通过 function calling 调用 use_skill 加载技能
//
// 返回已初始化的 *SkillManager，调用方可用于运行时 enable/disable 操作。
//
// 典型用法：
//
//	mgr, err := bot.SetupSkills(bot.SkillWireConfig{
//	    SkillsDir: "./skills",
//	    Tools:     toolMgr,
//	    Prompt:    promptReg,
//	    Store:     skill.NewConfigStoreAdapter(cfgStore),
//	    Logger:    logger,
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer mgr.SaveEnabledStates(ctx)
func SetupSkills(cfg SkillWireConfig) (*skill.SkillManager, error) {
	if cfg.Prompt == nil {
		return nil, errs.New("skill wire: Prompt registry is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	botSkillLogger := logger.With("component", "skill_wire")

	// 1. 创建 prompt RegistryAdapter（桥接 prompt.Registry ↔ skill.RegistryAdapter）
	regAdapter := skill.NewPromptRegistryAdapter(
		func(name string, order int, content string, enabled bool) {
			cfg.Prompt.Register(prompt.Section{
				Name:    name,
				Order:   order,
				Content: content,
				Enabled: enabled,
			})
		},
		cfg.Prompt.Unregister,
	)

	// 2. 创建 SkillManager（注入 Registry + Store 适配器）
	mgr := skill.NewSkillManager(regAdapter, cfg.Store, botSkillLogger)

	// 3. 从文件系统加载技能
	if cfg.SkillsDir != "" {
		loader := skill.NewLoader(cfg.SkillsDir, botSkillLogger)
		count, err := loader.LoadAndRegister(mgr)
		if err != nil {
			return nil, errs.Wrapf(err, "skill wire: load from %q", cfg.SkillsDir)
		}
		botSkillLogger.Debugw("skills loaded from filesystem",
			"dir", cfg.SkillsDir,
			"count", count,
		)
	}

	// 4. 注册触发提示词段落（告知 LLM 可用技能列表 + use_skill 调用方式）
	//    Order=150 落在行为规则区域（100-199）
	triggerSection := mgr.BuildTriggerSection(150)
	cfg.Prompt.Register(prompt.Section{
		Name:    triggerSection.Name,
		Order:   triggerSection.Order,
		Content: triggerSection.Content,
		Enabled: triggerSection.Enabled,
	})

	// 5. 注册 use_skill 工具提供者到 ToolManager
	if cfg.Tools != nil {
		if err := skill.RegisterTools(cfg.Tools, mgr); err != nil {
			return nil, errs.Wrap(err, "skill wire: register tools")
		}
	}

	botSkillLogger.Infow("skills system wired",
		"dir", cfg.SkillsDir,
		"total", len(mgr.List()),
		"enabled", len(mgr.EnabledNames()),
		"has_tool_provider", cfg.Tools != nil,
	)

	return mgr, nil
}
