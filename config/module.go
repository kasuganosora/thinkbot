package config

import (
	"context"
	"encoding/json"
	"os"
	"strings"

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
}

// GetBotLLMAssignment 读取指定 Bot 的 LLM 角色分配。
// 键格式：bot.<bot_id>.main、bot.<bot_id>.light
func (b *Builder) GetBotLLMAssignment(botID string) BotLLMAssignment {
	a := BotLLMAssignment{
		Main:  b.store.GetString(BotLLMKey(botID, "main"), ""),
		Light: b.store.GetString(BotLLMKey(botID, "light"), ""),
	}
	if a.Light == "" {
		a.Light = a.Main
	}
	return a
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
