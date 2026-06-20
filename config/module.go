package config

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// fx Module — config 依赖注入
// ============================================================================

// ConfigParams 是创建 Store 所需的依赖。
type ConfigParams struct {
	fx.In

	DB     *gorm.DB `optional:"true"`
	Logger *zap.SugaredLogger
}

// Module 是 config 的 fx 模块。
//
// 使用方式：
//
//	app := fx.New(
//	    config.Module,
//	    // ...其他模块
//	)
//
// 配置加载顺序（在 OnStart 钩子中执行）：
//  1. 创建 Store，加载 .env 文件（默认 ".env"，可通过 ENV CONFIG_FILE 覆盖）
//  2. AutoMigrate 配置表
//  3. 从数据库加载缓存
var Module = fx.Module("config",
	fx.Provide(NewStoreFromParams),
	fx.Invoke(registerConfigLifecycle),
)

// NewStoreFromParams 是 fx 可注入的 Store 构造函数。
func NewStoreFromParams(p ConfigParams) (*Store, error) {
	store := NewStore(p.DB)

	// .env 文件路径：优先环境变量 CONFIG_FILE，默认 ".env"
	envFile := ".env"
	if v, ok := os.LookupEnv("CONFIG_FILE"); ok && v != "" {
		envFile = v
	}

	if err := store.LoadEnvFile(envFile); err != nil {
		p.Logger.Warnw("config: failed to load .env file",
			"path", envFile, "err", err)
	} else {
		p.Logger.Debugw("config: loaded .env file", "path", envFile)
	}

	return store, nil
}

// registerConfigLifecycle 绑定 Store 的启动生命周期。
func registerConfigLifecycle(lc fx.Lifecycle, store *Store, logger *zap.SugaredLogger) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := store.Migrate(); err != nil {
				return errs.Wrap(err, "config: migrate")
			}
			if err := store.Reload(ctx); err != nil {
				logger.Warnw("config: failed to load from database", "err", err)
			}
			logger.Infow("config store initialized")
			return nil
		},
		OnStop: func(ctx context.Context) error {
			return nil
		},
	})
}

// ============================================================================
// Builder — 帮助其他模块从 Store 构建 typed 配置对象
// ============================================================================

// Builder 提供从 Store 构建 typed 配置对象的便捷方法。
type Builder struct {
	store  *Store
	logger *zap.SugaredLogger
}

// NewBuilder 创建配置构建器。
func NewBuilder(store *Store, logger *zap.SugaredLogger) *Builder {
	return &Builder{store: store, logger: logger}
}

// Store 返回底层 Store。
func (b *Builder) Store() *Store { return b.store }

// --- LLM 配置 ---

// ModelDef 描述一个命名的 LLM 模型配置。
// 在数据库中存储为单行 JSON：键 llm.<id>，值为此结构体的 JSON。
// 由上层模块（如 bot）负责转换为具体的 llm.Provider 实例。
type ModelDef struct {
	// Provider 后端类型：openai|anthropic|google|grok|bigmodel。
	Provider string `json:"provider"`

	// Model 模型名称（如 gpt-4o、claude-sonnet-4-20250514）。
	Model string `json:"model"`

	// APIKey API 密钥。
	APIKey string `json:"api_key"`

	// BaseURL 自定义 API 地址（可选）。
	BaseURL string `json:"base_url,omitempty"`

	// ChatPath Chat Completions 端点路径（可选）。
	// 仅对 OpenAI 兼容的 Chat 模式供应商有意义（如 bigmodel）。
	// 默认为 /v1/chat/completions。
	ChatPath string `json:"chat_path,omitempty"`

	// Temperature 采样温度（默认 0.7）。
	Temperature float64 `json:"temperature,omitempty"`

	// MaxTokens 最大输出 token 数（默认 4096）。
	MaxTokens int `json:"max_tokens,omitempty"`

	// Multimodal 标记此模型是否支持多模态输入（图片/音频/视频）。
	// 为 true 时，MultimodalStage 不会对此 bot 的消息做辅助转写。
	Multimodal bool `json:"multimodal,omitempty"`
}

// GetLLMModel 从数据库读取单个 LLM 配置（JSON）。
// 键格式：llm.<llm_id>
func (b *Builder) GetLLMModel(llmID string) (ModelDef, bool) {
	raw, ok := b.store.Get(LLMConfigKey(llmID))
	if !ok || raw == "" {
		return ModelDef{}, false
	}

	var def ModelDef
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		return ModelDef{}, false
	}

	// 填充默认值
	if def.Temperature == 0 {
		def.Temperature = 0.7
	}
	if def.MaxTokens == 0 {
		def.MaxTokens = 4096
	}
	return def, true
}

// GetAllLLMModels 读取所有已定义的 LLM 配置。
// 扫描 llm.* 前缀，解析每个值为 JSON，返回以 id 为键的 map。
func (b *Builder) GetAllLLMModels() map[string]ModelDef {
	raw := b.store.GetByPrefix("llm.")
	result := make(map[string]ModelDef, len(raw))
	for id, jsonStr := range raw {
		if jsonStr == "" {
			continue
		}
		var def ModelDef
		if err := json.Unmarshal([]byte(jsonStr), &def); err != nil {
			continue
		}
		if def.Temperature == 0 {
			def.Temperature = 0.7
		}
		if def.MaxTokens == 0 {
			def.MaxTokens = 4096
		}
		result[id] = def
	}
	return result
}

// BotLLMAssignment 描述一个 Bot 的 LLM 角色分配。
type BotLLMAssignment struct {
	// Main 主力 LLM ID（深度对话、工具调用）。
	Main string `json:"main"`

	// Light 低成本 LLM ID（标题提取、简单分类）。
	// 为空时回退到 Main。
	Light string `json:"light"`

	// Vision 多模态辅助 LLM ID。
	// 当 Main 模型不支持多模态时，用此模型将图片/音频/视频转为文字描述。
	// 为空时表示不启用多模态转写。
	Vision string `json:"vision"`
}

// GetBotLLMAssignment 读取指定 Bot 的 LLM 角色分配。
// 键格式：bot.<bot_id>.main、bot.<bot_id>.light
func (b *Builder) GetBotLLMAssignment(botID string) BotLLMAssignment {
	a := BotLLMAssignment{
		Main:   b.store.GetString(BotLLMKey(botID, "main"), ""),
		Light:  b.store.GetString(BotLLMKey(botID, "light"), ""),
		Vision: b.store.GetString(BotLLMKey(botID, "vision"), ""),
	}
	if a.Light == "" {
		a.Light = a.Main
	}
	return a
}

// GetBotTimezone 返回指定 Bot 的时区标识符。
// 优先级：bot.<bot_id>.timezone → system.timezone → $TZ 环境变量 → 服务器本地时区。
// 即每个 Bot 可独立设置时区，未设置时继承全局 system.timezone。
func (b *Builder) GetBotTimezone(botID string) string {
	// 1. per-bot 覆盖
	if tz := b.store.GetString(BotTimezoneKey(botID), ""); tz != "" {
		return tz
	}
	// 2. 全局 system.timezone（含 $TZ / 本地降级）
	return b.GetTimezone()
}

// GetBotTimezoneLocation 返回指定 Bot 的时区 *time.Location。
// 如果配置的时区无效，降级到 time.Local。
func (b *Builder) GetBotTimezoneLocation(botID string) *time.Location {
	tz := b.GetBotTimezone(botID)
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Local
	}
	return loc
}

// --- Channel 配置 ---

// --- Channel 配置 ---

// ChannelConfig 描述一个通用的 Channel 配置。
type ChannelConfig struct {
	Name  string
	Type  string // misskey, telegram
	Token string
	Host  string // misskey
	Extra map[string]string
}

// GetChannelConfigs 读取所有已配置的 Channel。
// Channel 通过 channel.{name}.* 前缀配置。
func (b *Builder) GetChannelConfigs() []ChannelConfig {
	all := b.store.GetByPrefix("channel.")

	channels := make(map[string]map[string]string)
	for key, val := range all {
		parts := splitFirst(key, ".")
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		if channels[name] == nil {
			channels[name] = make(map[string]string)
		}
		channels[name][parts[1]] = val
	}

	result := make([]ChannelConfig, 0, len(channels))
	for name, props := range channels {
		result = append(result, ChannelConfig{
			Name:  name,
			Type:  props["type"],
			Token: props["token"],
			Host:  props["host"],
			Extra: props,
		})
	}
	return result
}

// --- Bot 配置 ---

// BotSettings 描述 Bot 级别配置。
type BotSettings struct {
	SystemPrompt string
	Model        string
	Temperature  float64
	MaxTokens    int
	Workers      int
}

// GetBotSettings 读取全局 Bot 配置。
func (b *Builder) GetBotSettings() BotSettings {
	return BotSettings{
		SystemPrompt: b.store.GetString(KeyBotSystemPrompt, ""),
		Model:        b.store.GetString(KeyBotModel, ""),
		Temperature:  b.store.GetFloat64(KeyBotTemperature, 0.7),
		MaxTokens:    b.store.GetInt(KeyBotMaxTokens, 4096),
		Workers:      b.store.GetInt(KeyBotWorkers, 4),
	}
}

// --- 数据库 & 日志 ---

// GetDBPath 返回数据库文件路径。
func (b *Builder) GetDBPath() string {
	return b.store.GetString(KeyDBPath, "thinkbot.db")
}

// GetLogLevel 返回日志级别。
func (b *Builder) GetLogLevel() string {
	return b.store.GetString(KeyLogLevel, "info")
}

// --- Workflow 配置 ---

// WorkflowConfig 描述工作流引擎的全部可调参数。
// 未配置的字段自动使用 DefaultWorkflowConfig() 的值。
type WorkflowConfig struct {
	// MaxParallel 同一工作流中最大并行执行的节点数。
	MaxParallel int `json:"maxParallel"`

	// MaxRetries 单个节点执行出错时的最大重试次数。
	MaxRetries int `json:"maxRetries"`

	// MaxIterations Review 不通过时的最大迭代轮数。
	MaxIterations int `json:"maxIterations"`

	// RetryInitialMS 重试指数退避的初始等待毫秒。
	RetryInitialMS int `json:"retryInitialMs"`

	// RetryMaxMS 重试指数退避的最大等待毫秒。
	RetryMaxMS int `json:"retryMaxMs"`

	// ScheduleIntervalMS 调度器主循环轮询间隔毫秒。
	ScheduleIntervalMS int `json:"scheduleIntervalMs"`

	// AnalyzerTemperature 需求分析器 LLM 温度。
	AnalyzerTemperature float64 `json:"analyzerTemperature"`

	// AnalyzerMaxTokens 需求分析器 LLM 最大 token 数。
	AnalyzerMaxTokens int `json:"analyzerMaxTokens"`
}

// DefaultWorkflowConfig 返回引擎默认配置值。
func DefaultWorkflowConfig() WorkflowConfig {
	return WorkflowConfig{
		MaxParallel:        3,
		MaxRetries:         2,
		MaxIterations:      3,
		RetryInitialMS:     500,
		RetryMaxMS:         10000,
		ScheduleIntervalMS: 200,
		AnalyzerTemperature: 0.3,
		AnalyzerMaxTokens:   4096,
	}
}

// GetWorkflowConfig 从 Store 读取工作流配置，未设置的字段自动填充默认值。
func (b *Builder) GetWorkflowConfig() WorkflowConfig {
	d := DefaultWorkflowConfig()
	return WorkflowConfig{
		MaxParallel:        b.store.GetInt(KeyWorkflowMaxParallel, d.MaxParallel),
		MaxRetries:         b.store.GetInt(KeyWorkflowMaxRetries, d.MaxRetries),
		MaxIterations:      b.store.GetInt(KeyWorkflowMaxIterations, d.MaxIterations),
		RetryInitialMS:     b.store.GetInt(KeyWorkflowRetryInitialMS, d.RetryInitialMS),
		RetryMaxMS:         b.store.GetInt(KeyWorkflowRetryMaxMS, d.RetryMaxMS),
		ScheduleIntervalMS: b.store.GetInt(KeyWorkflowScheduleInterval, d.ScheduleIntervalMS),
		AnalyzerTemperature: b.store.GetFloat64(KeyWorkflowAnalyzerTemp, d.AnalyzerTemperature),
		AnalyzerMaxTokens:   b.store.GetInt(KeyWorkflowAnalyzerMaxTokens, d.AnalyzerMaxTokens),
	}
}

// WorkflowMetaSpecs 返回工作流配置项的元数据，用于 RegisterMany 注册到前端设置界面。
func WorkflowMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeyWorkflowMaxParallel, Category: "Workflow", Description: "同一工作流中最大并行执行的子任务数（默认 3）"},
		{Key: KeyWorkflowMaxRetries, Category: "Workflow", Description: "子任务执行出错时的最大重试次数（默认 2）"},
		{Key: KeyWorkflowMaxIterations, Category: "Workflow", Description: "审查不通过时的最大迭代轮数（默认 3）"},
		{Key: KeyWorkflowRetryInitialMS, Category: "Workflow", Description: "重试指数退避的初始等待毫秒（默认 500）"},
		{Key: KeyWorkflowRetryMaxMS, Category: "Workflow", Description: "重试指数退避的最大等待毫秒（默认 10000）"},
		{Key: KeyWorkflowScheduleInterval, Category: "Workflow", Description: "调度器主循环轮询间隔毫秒（默认 200）"},
		{Key: KeyWorkflowAnalyzerTemp, Category: "Workflow", Description: "需求分析器 LLM 温度（默认 0.3）"},
		{Key: KeyWorkflowAnalyzerMaxTokens, Category: "Workflow", Description: "需求分析器 LLM 最大 token 数（默认 4096）"},
	}
}

// splitFirst 以 sep 分割，仅在第一个 sep 处分割。
func splitFirst(s, sep string) []string {
	before, after, found := strings.Cut(s, sep)
	if !found {
		return []string{s}
	}
	return []string{before, after}
}

// --- Engagement 配置 ---

// EngagementConfig 描述主动参与模块的全部可调参数。
// 未配置的字段自动使用 DefaultEngagementConfig() 的值。
type EngagementConfig struct {
	// Enabled 是否启用主动参与（总开关）。false 时所有时间线消息都不会被评估。
	Enabled bool `json:"enabled"`

	// Channels 允许主动参与的渠道列表（逗号分隔的 source 标识）。
	// 为空时禁用所有渠道。
	Channels []string `json:"channels"`

	// ReplyProbability 主动参与概率（0.0~1.0，默认 0.15）。
	ReplyProbability float64 `json:"replyProbability"`

	// Cooldown 同一用户冷却时间（默认 0，不限制）。
	Cooldown time.Duration `json:"cooldown"`

	// RateLimitCapacity 令牌桶容量——每小时最多主动参与次数（默认 3）。
	RateLimitCapacity int `json:"rateLimitCapacity"`

	// RateLimitInterval 令牌桶补充间隔（默认 1h）。
	RateLimitInterval time.Duration `json:"rateLimitInterval"`

	// Keywords 关键词列表——消息文本包含任一关键词才通过 Tier 1。
	// 为空时不做关键词过滤。
	Keywords []string `json:"keywords"`

	// LLMJudgeEnabled 是否启用 Tier 2 LLM 快判（默认 false）。
	LLMJudgeEnabled bool `json:"llmJudgeEnabled"`

	// BlockedUsers 被排除的用户 ID 列表。
	BlockedUsers []string `json:"blockedUsers"`

	// BlockedSources 被排除的消息来源列表。
	BlockedSources []string `json:"blockedSources"`

	// MinLength 消息最小长度（rune），0 表示无限制。
	MinLength int `json:"minLength"`

	// MaxLength 消息最大长度（rune），0 表示无限制。
	MaxLength int `json:"maxLength"`

	// BackoffBaseSeconds no_action 退避基准秒数（默认 10.0）。
	BackoffBaseSeconds float64 `json:"backoffBaseSeconds"`

	// BackoffCapSeconds 退避上限秒数（默认 300.0）。
	BackoffCapSeconds float64 `json:"backoffCapSeconds"`

	// BackoffStartCount 从第几次连续 decline 开始退避（默认 3）。
	BackoffStartCount int `json:"backoffStartCount"`

	// BurstIntervalSeconds 消息突发检测窗口秒数（默认 5.0）。
	BurstIntervalSeconds float64 `json:"burstIntervalSeconds"`

	// WaitTimeoutSeconds ActionWait 超时秒数（默认 30.0）。
	WaitTimeoutSeconds float64 `json:"waitTimeoutSeconds"`

	// BackoffBypassPendingCount 退避绕过阈值（默认 0=禁用）。
	BackoffBypassPendingCount int `json:"backoffBypassPendingCount"`
}

// DefaultEngagementConfig 返回主动参与模块的默认配置值。
func DefaultEngagementConfig() EngagementConfig {
	return EngagementConfig{
		Enabled:               false,
		ReplyProbability:      0.15,
		Cooldown:              0,
		RateLimitCapacity:     3,
		RateLimitInterval:     1 * time.Hour,
		BackoffBaseSeconds:    10.0,
		BackoffCapSeconds:     300.0,
		BackoffStartCount:     3,
		BurstIntervalSeconds:  5.0,
		WaitTimeoutSeconds:    30.0,
		BackoffBypassPendingCount: 0,
	}
}

// GetEngagementConfig 从 Store 读取主动参与配置，未设置的字段自动填充默认值。
func (b *Builder) GetEngagementConfig() EngagementConfig {
	d := DefaultEngagementConfig()
	return EngagementConfig{
		Enabled:                   b.store.GetBool(KeyEngagementEnabled, d.Enabled),
		Channels:                  b.store.GetStringSlice(KeyEngagementChannels, d.Channels),
		ReplyProbability:          b.store.GetFloat64(KeyEngagementReplyProbability, d.ReplyProbability),
		Cooldown:                  b.store.GetDuration(KeyEngagementCooldown, d.Cooldown),
		RateLimitCapacity:         b.store.GetInt(KeyEngagementRateLimitCapacity, d.RateLimitCapacity),
		RateLimitInterval:         b.store.GetDuration(KeyEngagementRateLimitInterval, d.RateLimitInterval),
		Keywords:                  b.store.GetStringSlice(KeyEngagementKeywords, d.Keywords),
		LLMJudgeEnabled:           b.store.GetBool(KeyEngagementLLMJudgeEnabled, d.LLMJudgeEnabled),
		BlockedUsers:              b.store.GetStringSlice(KeyEngagementBlockedUsers, d.BlockedUsers),
		BlockedSources:            b.store.GetStringSlice(KeyEngagementBlockedSources, d.BlockedSources),
		MinLength:                 b.store.GetInt(KeyEngagementMinLength, d.MinLength),
		MaxLength:                 b.store.GetInt(KeyEngagementMaxLength, d.MaxLength),
		BackoffBaseSeconds:        b.store.GetFloat64(KeyEngagementBackoffBaseSeconds, d.BackoffBaseSeconds),
		BackoffCapSeconds:         b.store.GetFloat64(KeyEngagementBackoffCapSeconds, d.BackoffCapSeconds),
		BackoffStartCount:         b.store.GetInt(KeyEngagementBackoffStartCount, d.BackoffStartCount),
		BurstIntervalSeconds:      b.store.GetFloat64(KeyEngagementBurstInterval, d.BurstIntervalSeconds),
		WaitTimeoutSeconds:        b.store.GetFloat64(KeyEngagementWaitTimeout, d.WaitTimeoutSeconds),
		BackoffBypassPendingCount: b.store.GetInt(KeyEngagementBackoffBypass, d.BackoffBypassPendingCount),
	}
}

// EngagementMetaSpecs 返回主动参与配置项的元数据，用于 RegisterMany 注册到前端设置界面。
func EngagementMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeyEngagementEnabled, Category: "Engagement", Description: "是否启用主动参与功能（总开关，默认关闭）"},
		{Key: KeyEngagementChannels, Category: "Engagement", Description: "允许主动参与的渠道列表（逗号分隔，如 misskey,telegram）"},
		{Key: KeyEngagementReplyProbability, Category: "Engagement", Description: "主动参与概率 0.0~1.0（默认 0.15）"},
		{Key: KeyEngagementCooldown, Category: "Engagement", Description: "同一用户冷却时间（如 10m，默认 0=不限制）"},
		{Key: KeyEngagementRateLimitCapacity, Category: "Engagement", Description: "令牌桶容量——最多主动参与次数（默认 3）"},
		{Key: KeyEngagementRateLimitInterval, Category: "Engagement", Description: "令牌桶补充间隔（如 1h，默认 1小时）"},
		{Key: KeyEngagementKeywords, Category: "Engagement", Description: "兴趣关键词列表（逗号分隔，为空则不做关键词过滤）"},
		{Key: KeyEngagementLLMJudgeEnabled, Category: "Engagement", Description: "是否启用 Tier 2 LLM 快判（默认关闭）"},
		{Key: KeyEngagementBlockedUsers, Category: "Engagement", Description: "被排除的用户 ID 列表（逗号分隔）"},
		{Key: KeyEngagementBlockedSources, Category: "Engagement", Description: "被排除的消息来源列表（逗号分隔）"},
		{Key: KeyEngagementMinLength, Category: "Engagement", Description: "消息最小长度 rune 数（默认 0=不限制）"},
		{Key: KeyEngagementMaxLength, Category: "Engagement", Description: "消息最大长度 rune 数（默认 0=不限制）"},
		{Key: KeyEngagementBackoffBaseSeconds, Category: "Engagement", Description: "no_action 退避基准秒数（默认 10.0）"},
		{Key: KeyEngagementBackoffCapSeconds, Category: "Engagement", Description: "退避上限秒数（默认 300.0）"},
		{Key: KeyEngagementBackoffStartCount, Category: "Engagement", Description: "从第几次连续不参与开始退避（默认 3）"},
		{Key: KeyEngagementBurstInterval, Category: "Engagement", Description: "消息突发检测窗口秒数（默认 5.0）"},
		{Key: KeyEngagementWaitTimeout, Category: "Engagement", Description: "ActionWait 超时秒数（默认 30.0）"},
		{Key: KeyEngagementBackoffBypass, Category: "Engagement", Description: "退避绕过阈值——待处理消息数（默认 0=禁用）"},
	}
}

// --- Workspace 配置 ---

// GetWorkspaceDir 返回 bot 工作空间根目录的物理路径。
// 默认 "data/workspaces"，每个 Bot 拥有独立子目录 {dir}/{botID}/。
func (b *Builder) GetWorkspaceDir() string {
	return b.store.GetString(KeyWorkspaceDir, "data/workspaces")
}

// WorkspaceMetaSpecs 返回工作空间配置项的元数据。
func WorkspaceMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeyWorkspaceDir, Category: "Workspace", Description: "Bot 工作空间根目录的物理路径（默认 data/workspaces）。每个 Bot 拥有独立子目录，文件持久化保存。"},
	}
}

// --- System 配置 ---

// GetTimezone 返回系统时区标识符（IANA 格式，如 "Asia/Shanghai"）。
// 如果配置未设置，返回服务器本地时区的名称。
// 用于 bot 时间感知和 Docker 沙箱容器的 TZ 环境变量。
func (b *Builder) GetTimezone() string {
	if tz := b.store.GetString(KeySystemTimezone, ""); tz != "" {
		return tz
	}
	// 降级到服务器本地时区
	if name, err := time.LoadLocation(""); err == nil {
		_ = name // time.LoadLocation("") 返回 time.Local，无法直接拿到名称
	}
	// 尝试从 TZ 环境变量获取
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	// 最终降级到 Local 的字符串表示
	return time.Local.String()
}

// GetTimezoneLocation 返回解析后的 *time.Location。
// 如果配置的时区无效，降级到 time.Local（服务器本地时区）。
func (b *Builder) GetTimezoneLocation() *time.Location {
	tz := b.GetTimezone()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Local
	}
	return loc
}

// SystemMetaSpecs 返回系统配置项的元数据。
func SystemMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeySystemTimezone, Category: "System", Description: "系统时区（IANA 标识符，如 Asia/Shanghai、UTC）。为空时使用服务器本地时区。影响 bot 时间感知和 Docker 沙箱容器时区。"},
	}
}

// --- Soul 配置 ---

// SoulConfig 描述 SOUL.md 人格文件的配置。
//
// 约定优于配置：SOUL.md 默认从二进制所在目录自动加载（文件存在即生效），
// 不需要任何开关。此配置仅用于可选的运行时调整。
type SoulConfig struct {
	// ReloadInterval 文件变更检测的轮询间隔。
	// 0 表示禁用热重载（仅在启动时加载一次）。
	// 推荐值：5s ~ 30s。
	ReloadInterval time.Duration `json:"reloadInterval"`

	// PromptDir 额外 prompt 段落目录（可选）。
	// 目录中的 {order}_{name}.md 文件会被加载为额外的 Section。
	// 为空时不加载额外段落。
	PromptDir string `json:"promptDir"`
}

// DefaultSoulConfig 返回 SOUL.md 模块的默认配置值。
func DefaultSoulConfig() SoulConfig {
	return SoulConfig{
		ReloadInterval: 5 * time.Second,
		PromptDir:      "",
	}
}

// GetSoulConfig 从 Store 读取 SOUL.md 配置，未设置的字段自动填充默认值。
func (b *Builder) GetSoulConfig() SoulConfig {
	d := DefaultSoulConfig()
	return SoulConfig{
		ReloadInterval: b.store.GetDuration(KeySoulReloadInterval, d.ReloadInterval),
		PromptDir:      b.store.GetString(KeySoulPromptDir, d.PromptDir),
	}
}

// SoulMetaSpecs 返回 SOUL.md 配置项的元数据，用于 RegisterMany 注册到前端设置界面。
func SoulMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeySoulReloadInterval, Category: "Soul", Description: "SOUL.md 文件变更检测轮询间隔（如 5s，0=禁用热重载）。文件位于二进制目录下，存在即生效。"},
		{Key: KeySoulPromptDir, Category: "Soul", Description: "额外 prompt 段落目录（可选，存放 {order}_{name}.md 文件）"},
	}
}

// --- Tools 工具权限策略配置 ---

// ToolPolicyConfig 是从 config.Store 读取工具权限策略的桥接类型。
// 策略以 JSON 形式存储在 tools.<botID>.policy 键中。
//
// 使用方式：
//
//	policyJSON := builder.GetToolPolicyJSON("mybot")
//	policy := tools.ParseToolPolicy(policyJSON)
type ToolPolicyConfig struct {
	// BotID bot 标识符。
	BotID string

	// PolicyJSON 策略的 JSON 字符串（tools.<botID>.policy 的值）。
	PolicyJSON string
}

// GetToolPolicy 读取指定 bot 的工具权限策略 JSON。
// 如果未配置，返回空字符串（表示全部放行）。
func (b *Builder) GetToolPolicy(botID string) ToolPolicyConfig {
	return ToolPolicyConfig{
		BotID:      botID,
		PolicyJSON: b.store.GetString(ToolPolicyKey(botID), ""),
	}
}

// SetToolPolicy 将工具权限策略 JSON 持久化到数据库。
func (b *Builder) SetToolPolicy(ctx context.Context, botID, policyJSON string) error {
	return b.store.Set(ctx, ToolPolicyKey(botID), policyJSON)
}

// ToolPolicyMetaSpecs 返回工具权限策略的元数据说明。
// 注意：实际键名包含 botID（动态），这里仅注册说明性的元数据。
func ToolPolicyMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: "tools.policy", Category: "Tools", Description: "工具黑名单权限策略（JSON）。键格式 tools.<botID>.policy。支持按 channel+chatType 禁用工具，并为特定用户开放白名单。"},
	}
}
