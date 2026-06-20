package sandbox

import (
	"time"

	"go.uber.org/zap"
)

// ============================================================================
// wire.go — 组合根（Setup 函数）
//
// 两套装配路径：
//   - SetupSandbox: 临时会话级工作空间（SandboxManager，per-session，自动清理）
//   - SetupBotWorkspace: 持久化 per-bot 工作空间（BotWorkspaceManager，per-bot，不清理）
// ============================================================================

// ---------------------------------------------------------------------------
// 临时会话级沙箱
// ---------------------------------------------------------------------------

// WireConfig 是 SetupSandbox 的配置。
type WireConfig struct {
	// Config 沙箱配置。空值使用 DefaultConfig()。
	Config Config

	// Logger 日志器。空值使用 nop logger。
	Logger *zap.SugaredLogger

	// IdleTTL 工作空间闲置过期时间。零值使用默认 30 分钟。
	IdleTTL time.Duration
}

// SetupResult 是 SetupSandbox 的返回结果。
type SetupResult struct {
	Manager *SandboxManager
	// Close 销毁函数，调用后清理所有资源。
	Close func() error
}

// SetupSandbox 一站式创建临时沙箱子系统。
func SetupSandbox(cfg WireConfig) (*SetupResult, error) {
	sbCfg := fillDefaults(cfg.Config)

	sb, err := NewSandbox(sbCfg, cfg.Logger)
	if err != nil {
		return nil, err
	}

	mgr := NewSandboxManager(sb, cfg.Logger, cfg.IdleTTL)

	return &SetupResult{
		Manager: mgr,
		Close: func() error {
			return mgr.Close()
		},
	}, nil
}

// ---------------------------------------------------------------------------
// 持久化 per-bot 工作空间
// ---------------------------------------------------------------------------

// BotWorkspaceWireConfig 是 SetupBotWorkspace 的配置。
type BotWorkspaceWireConfig struct {
	// BaseDir bot 工作空间的根目录（如 "data/workspaces"）。
	// 为空时使用 "data/workspaces"。
	BaseDir string

	// Config 沙箱配置（Backend/Image/Limits 等）。空值使用 DefaultConfig()。
	Config Config

	// Logger 日志器。空值使用 nop logger。
	Logger *zap.SugaredLogger
}

// BotWorkspaceSetupResult 是 SetupBotWorkspace 的返回结果。
type BotWorkspaceSetupResult struct {
	Manager *BotWorkspaceManager
	// Close 清理函数（不删除 bot 数据文件，仅清除内存引用）。
	Close func() error
}

// SetupBotWorkspace 一站式创建持久化 per-bot 工作空间子系统。
//
// 创建 BotWorkspaceManager → 返回 Manager 和清理函数。
// 文件存储在 {BaseDir}/{botID}/，持久化，不自动清理。
// 命令执行通过 Docker 临时容器（隔离）或本地进程（降级）。
//
// 使用示例：
//
//	result, err := sandbox.SetupBotWorkspace(sandbox.BotWorkspaceWireConfig{
//	    BaseDir: "data/workspaces",
//	    Config:  sandbox.DefaultConfig(),
//	    Logger:  logger,
//	})
//	if err != nil {
//	    return err
//	}
//	defer result.Close()
//
//	// 注册工具到 ToolManager
//	sandbox.RegisterBotWorkspaceTools(toolMgr, result.Manager)
//
//	// 获取 bot 的 SOUL.md 路径
//	botDir, _ := result.Manager.BotDir("mybot")
//	soulPath := filepath.Join(botDir, "SOUL.md")
func SetupBotWorkspace(cfg BotWorkspaceWireConfig) (*BotWorkspaceSetupResult, error) {
	sbCfg := fillDefaults(cfg.Config)

	mgr, err := NewBotWorkspaceManager(cfg.BaseDir, sbCfg, cfg.Logger)
	if err != nil {
		return nil, err
	}

	return &BotWorkspaceSetupResult{
		Manager: mgr,
		Close: func() error {
			return mgr.Close()
		},
	}, nil
}

// fillDefaults 填充 Config 的默认值（不覆盖已设字段）。
func fillDefaults(cfg Config) Config {
	if cfg.Backend == "" {
		cfg.Backend = "auto"
	}
	def := DefaultConfig()
	if cfg.Image == "" {
		cfg.Image = def.Image
	}
	if cfg.MemoryLimit == "" {
		cfg.MemoryLimit = def.MemoryLimit
	}
	if cfg.CPULimit == "" {
		cfg.CPULimit = def.CPULimit
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = def.Timeout
	}
	if cfg.MaxOutput == 0 {
		cfg.MaxOutput = def.MaxOutput
	}
	if cfg.MaxFileWrite == 0 {
		cfg.MaxFileWrite = def.MaxFileWrite
	}
	if cfg.Timezone == "" {
		cfg.Timezone = def.Timezone
	}
	return cfg
}
