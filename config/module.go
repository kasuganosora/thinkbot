package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/util/errs"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"
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

			// 全局出口代理：若配置了 system.proxy，则写入 HTTP_PROXY/HTTPS_PROXY
			// 环境变量，使主机侧所有使用默认 Transport 的 HTTP 客户端（LLM、channel 等）
			// 统一走部署侧代理。NO_PROXY 默认放行本地地址，避免自环请求被代理拦截。
			// 须在配置加载完成后、发起任何出站请求前执行一次。
			if proxy := GlobalProxy(store); proxy != "" {
				_ = os.Setenv("HTTP_PROXY", proxy)
				_ = os.Setenv("HTTPS_PROXY", proxy)
				_ = os.Setenv("http_proxy", proxy)
				_ = os.Setenv("https_proxy", proxy)
				if os.Getenv("NO_PROXY") == "" {
					_ = os.Setenv("NO_PROXY", "localhost,127.0.0.1")
					_ = os.Setenv("no_proxy", "localhost,127.0.0.1")
				}
				logger.Infow("global proxy enabled (host side)", "proxy", proxy)
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			return nil
		},
	})
}

// GlobalProxy 返回生效的全局出口代理 URL。
// 优先取数据库配置 system.proxy；若为空则回退到同名环境变量 SYSTEM_PROXY
// （EnvKeyToConfigKey("SYSTEM_PROXY") == "system.proxy"），兼容 .env 直接注入。
// 两者皆空时返回 ""（表示直连）。
func GlobalProxy(store *Store) string {
	if p := store.GetString(KeySystemProxy, ""); p != "" {
		return p
	}
	return os.Getenv(ConfigKeyToEnvKey(KeySystemProxy))
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

	// Temperature 采样温度（默认 0.7，nil 表示使用默认值，0 表示确定性输出）。
	Temperature *float64 `json:"temperature,omitempty"`

	// MaxTokens 最大输出 token 数（默认 4096）。
	MaxTokens int `json:"max_tokens,omitempty"`

	// Multimodal 标记此模型是否支持多模态输入（图片/音频/视频）。
	// 为 true 时，MultimodalStage 不会对此 bot 的消息做辅助转写。
	Multimodal bool `json:"multimodal,omitempty"`
}

// GetLLMModel 从 provider 系统查找 LLM 模型配置。
// 模型由 provider.* 配置定义，每个 provider 包含一个 models 列表。
func (b *Builder) GetLLMModel(llmID string) (ModelDef, bool) {
	return b.resolveProviderModel(llmID)
}

// resolveProviderModel 扫描所有 provider.* 配置，从中查找指定模型 ID。
func (b *Builder) resolveProviderModel(modelID string) (ModelDef, bool) {
	rawProviders := b.store.GetByPrefix("provider.")
	for _, raw := range rawProviders {
		if raw == "" {
			continue
		}
		var prov struct {
			Enabled    bool   `json:"enabled"`
			ClientType string `json:"clientType"`
			BaseURL    string `json:"baseUrl"`
			APIKey     string `json:"apiKey"`
			Models     []struct {
				ID            string   `json:"id"`
				Name          string   `json:"name"`
				ContextLength int      `json:"contextLength"`
				Multimodal    bool     `json:"multimodal"`
				Temperature   float64  `json:"temperature"`
				MaxTokens     int      `json:"maxTokens"`
				Capabilities  []string `json:"capabilities"`
			} `json:"models"`
		}
		if err := json.Unmarshal([]byte(raw), &prov); err != nil || !prov.Enabled {
			continue
		}
		for _, m := range prov.Models {
			if m.ID == modelID {
				t := 0.7
				if m.Temperature > 0 {
					t = m.Temperature
				}
				mt := m.MaxTokens
				if mt == 0 {
					mt = 4096
				}
				return fillModelDefaults(ModelDef{
					Provider:    mapClientType(prov.ClientType),
					Model:       m.ID,
					APIKey:      prov.APIKey,
					BaseURL:     prov.BaseURL,
					ChatPath:    resolveChatPath(prov.ClientType, prov.BaseURL),
					Temperature: &t,
					MaxTokens:   mt,
					Multimodal:  m.Multimodal,
				}), true
			}
		}
	}
	return ModelDef{}, false
}

// mapClientType 将前端 Provider 的 clientType 映射为 llm.Provider 工厂所需的 provider 字符串。
func mapClientType(clientType string) string {
	switch strings.ToLower(clientType) {
	case "openai compatible", "openai":
		return "openai"
	case "anthropic compatible", "anthropic":
		return "anthropic"
	case "google", "gemini":
		return "google"
	case "grok":
		return "grok"
	default:
		return "openai"
	}
}

// resolveChatPath 根据 provider 类型和 baseUrl 推断对应的 chat API 路径。
//
// 注意：baseURL 可能已经包含版本段（如智谱 GLM 的
// "https://open.bigmodel.cn/api/coding/paas/v4"），此时若再拼 "/v4/..." 会
// 导致路径重复（.../v4/v4/chat/completions → 404）。因此当 baseURL 已含
// 版本段时只返回相对路径 "/chat/completions"。
func resolveChatPath(clientType, baseURL string) string {
	switch strings.ToLower(clientType) {
	case "anthropic compatible", "anthropic":
		return "/v1/messages"
	case "google", "gemini":
		return ""
	default: // OpenAI Compatible
		// baseURL 已自带版本段（/v1、/v4 等）时不再重复前缀
		if hasVersionSegment(baseURL) {
			return "/chat/completions"
		}
		if strings.Contains(baseURL, "bigmodel") {
			return "/v4/chat/completions"
		}
		return "/v1/chat/completions"
	}
}

// hasVersionSegment 判断 baseURL 是否已包含 /vN 形式的版本段。
func hasVersionSegment(baseURL string) bool {
	for _, seg := range []string{"/v1", "/v2", "/v3", "/v4", "/v5"} {
		if strings.Contains(baseURL, seg) {
			return true
		}
	}
	return false
}

// fillModelDefaults 填充 ModelDef 的默认值。
func fillModelDefaults(def ModelDef) ModelDef {
	if def.Temperature == nil {
		def.Temperature = float64Ptr(0.7)
	}
	if def.MaxTokens == 0 {
		def.MaxTokens = 8192
	}
	return def
}

// float64Ptr 返回 float64 的指针（用于 Temperature 默认值）。
func float64Ptr(v float64) *float64 {
	return &v
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
// 默认 data/thinkbot.db（落在 data 卷，配合 docker 的 ./data:/app/data 持久化）。
// 主程序在 cmd/main.go 打开数据库时经环境变量 DB_PATH（⇄ db.path）实际消费本键。
func (b *Builder) GetDBPath() string {
	return b.store.GetString(KeyDBPath, "data/thinkbot.db")
}

// GetLogLevel 返回日志级别（默认 info，由 cmd/main.go 经 LOG_LEVEL 环境变量消费）。
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

	// 注意：这里曾有 AnalyzerMaxTokens（workflow.analyzer_max_tokens），已移除。
	// 分析器输出预算统一跟随 bot 所选模型的 MaxTokens，不再单独配置。

	// AnalyzerStuckTimeoutMS 需求分析器流式 LLM 调用的卡死看门狗阈值（毫秒）。
	// 默认 180000（3 分钟）。连续性说明见 KeyWorkflowAnalyzerStuckTimeout。
	AnalyzerStuckTimeoutMS int `json:"analyzerStuckTimeoutMs"`

	// AnalyzerMaxDurationMS 需求分析阶段「整轮总时长上限」（毫秒）。默认 600000（10 分钟）。
	// 兜底防止 GLM 退化时分析器无限重试把「分析中」拖成数十分钟黑洞；超过该时长分析阶段
	// 整体失败并给出明确报错，前端可立即看到结果而非一直转圈。
	AnalyzerMaxDurationMS int `json:"analyzerMaxDurationMs"`

	// GoalMaxIterations 目标模式（闭环循环）的全局最大迭代轮数。默认 5。
	// review 节点在节点级迭代仍不通过时回退到 Feedback 目标节点重跑，每轮 +1；
	// 达到上限仍不通过则工作流失败。0 表示使用代码兜底默认。
	GoalMaxIterations int `json:"goalMaxIterations"`
}

// DefaultWorkflowConfig 返回引擎默认配置值。
func DefaultWorkflowConfig() WorkflowConfig {
	return WorkflowConfig{
		MaxParallel:            3,
		MaxRetries:             2,
		MaxIterations:          3,
		RetryInitialMS:         500,
		RetryMaxMS:             10000,
		ScheduleIntervalMS:     200,
		AnalyzerTemperature:    0.3,
		AnalyzerStuckTimeoutMS: 180000,
		AnalyzerMaxDurationMS:  600000,
		GoalMaxIterations:      5,
	}
}

// GetWorkflowConfig 从 Store 读取工作流配置，未设置的字段自动填充默认值。
func (b *Builder) GetWorkflowConfig() WorkflowConfig {
	d := DefaultWorkflowConfig()
	return WorkflowConfig{
		MaxParallel:            b.store.GetInt(KeyWorkflowMaxParallel, d.MaxParallel),
		MaxRetries:             b.store.GetInt(KeyWorkflowMaxRetries, d.MaxRetries),
		MaxIterations:          b.store.GetInt(KeyWorkflowMaxIterations, d.MaxIterations),
		RetryInitialMS:         b.store.GetInt(KeyWorkflowRetryInitialMS, d.RetryInitialMS),
		RetryMaxMS:             b.store.GetInt(KeyWorkflowRetryMaxMS, d.RetryMaxMS),
		ScheduleIntervalMS:     b.store.GetInt(KeyWorkflowScheduleInterval, d.ScheduleIntervalMS),
		AnalyzerTemperature:    b.store.GetFloat64(KeyWorkflowAnalyzerTemp, d.AnalyzerTemperature),
		AnalyzerStuckTimeoutMS: b.store.GetInt(KeyWorkflowAnalyzerStuckTimeout, d.AnalyzerStuckTimeoutMS),
		AnalyzerMaxDurationMS:  b.store.GetInt(KeyWorkflowAnalyzerMaxDuration, d.AnalyzerMaxDurationMS),
		GoalMaxIterations:      b.store.GetInt(KeyWorkflowGoalMaxIterations, d.GoalMaxIterations),
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
		{Key: KeyWorkflowAnalyzerStuckTimeout, Category: "Workflow", Description: "需求分析器（流式 LLM）卡死看门狗阈值秒数（默认 180=3 分钟）。连续无 token 超该时长判卡死终止；硬上限=该值×10（subagent.delegateHardTimeoutFactor=10）。靠看门狗判断真卡死，不写死固定超时。"},
		{Key: KeyWorkflowAnalyzerMaxDuration, Category: "Workflow", Description: "需求分析阶段「整轮总时长上限」毫秒（默认 600000=10 分钟）。兜底防止 GLM 退化时分析器无限重试把「分析中」拖成数十分钟黑洞；超过该时长分析阶段整体失败并明确报错。"},
		{Key: KeyWorkflowGoalMaxIterations, Category: "Workflow", Description: "目标模式（闭环循环）的全局最大迭代轮数（默认 5）。review 节点在节点级迭代仍不通过时，回退到其 Feedback 目标节点重跑并注入审查意见，形成「工作→审查→修复→审查」循环；达到该上限仍不通过则工作流失败。"},
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

	// Profile 参与预设角色名（observer/lurker/active/moderator）。
	// 设置后会覆盖 ReplyProbability、EngagementThreshold、BackoffStartCount、RateLimitCapacity。
	// 空字符串表示不使用预设角色。
	Profile string `json:"profile"`

	// EngagementThreshold Tier 2 LLM 快判评分阈值（0-100，0=禁用评分模式）。
	// 启用时，LLM 返回 0-100 分数，仅分数 ≥ 阈值才参与。
	// 更高 = 更挑剔。论文研究二中实测 HIGH(90)/MEDIUM(75)/LOW(50) 三档有效。
	// 0 表示使用传统 YES/NO 模式。
	EngagementThreshold int `json:"engagementThreshold"`

	// AutoAdjustFrequency 是否根据群组活跃度自动调整参与频率。
	// 启用后 TimingGate 会基于观察到的消息间隔动态调整 FrequencyMultiplier，
	// 使 Bot 在活跃群组更积极、在安静群组更低调。
	AutoAdjustFrequency bool `json:"autoAdjustFrequency"`
}

// DefaultEngagementConfig 返回主动参与模块的默认配置值。
func DefaultEngagementConfig() EngagementConfig {
	return EngagementConfig{
		Enabled:                   false,
		ReplyProbability:          0.15,
		Cooldown:                  0,
		RateLimitCapacity:         3,
		RateLimitInterval:         1 * time.Hour,
		BackoffBaseSeconds:        10.0,
		BackoffCapSeconds:         300.0,
		BackoffStartCount:         3,
		BurstIntervalSeconds:      5.0,
		WaitTimeoutSeconds:        30.0,
		BackoffBypassPendingCount: 0,
		Profile:                   "",
		EngagementThreshold:       0,
		AutoAdjustFrequency:       false,
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
		Profile:                   b.store.GetString(KeyEngagementProfile, d.Profile),
		EngagementThreshold:       b.store.GetInt(KeyEngagementThreshold, d.EngagementThreshold),
		AutoAdjustFrequency:       b.store.GetBool(KeyEngagementAutoAdjustFreq, d.AutoAdjustFrequency),
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
		{Key: KeyEngagementProfile, Category: "Engagement", Description: "参与预设角色名（observer/lurker/active/moderator，空=不自定义角色）"},
		{Key: KeyEngagementThreshold, Category: "Engagement", Description: "LLM 快判评分阈值 0-100（默认 0=传统 YES/NO 模式，更高=更挑剔）"},
		{Key: KeyEngagementAutoAdjustFreq, Category: "Engagement", Description: "是否自动根据群组活跃度调整参与频率（默认 false）"},
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
		{Key: KeySandboxBackend, Category: "Workspace", Description: "沙箱后端：auto（默认，有 Docker 用容器隔离否则 local）| docker（强制容器隔离，不可用则报错）| local（强制本地进程，无隔离）。DooD 部署建议设为 docker。"},
		{Key: KeySandboxImage, Category: "Workspace", Description: "沙箱 Docker 镜像（docker 模式下 per-bot 长期容器使用，默认 alpine:latest）。"},
		{Key: KeySandboxRequireDocker, Category: "Workspace", Description: "auto 模式下是否强制要求 Docker 可用：true 则探测不到 Docker 直接报错而非降级 local（避免无隔离裸跑）；false（默认）则降级 local。"},
		{Key: KeySandboxTimeout, Category: "Workspace", Description: "bot 在沙箱里执行单条命令的「硬上限」秒数。默认 0 表示自动 = 卡死阈值 × 3（默认即 15 分钟），不写死固定时长；设为正整数时显式覆盖该硬上限。作为卡死看门狗的最终兜底：哪怕命令一直在输出，超过它也会被强制终止。正常慢命令靠卡死看门狗放行，不会误杀。"},
		{Key: KeySandboxStuckTimeout, Category: "Workspace", Description: "卡死看门狗阈值（秒，默认 300，即 5 分钟）。命令连续无输出超过该时长即判定卡死并终止。这是区分「编译慢」与「死锁卡死」的关键：只要命令持续有输出（哪怕慢）就不杀。"},
	}
}

// --- ToolOutput 配置（工具输出截断 + 落盘指针）---

// ToolOutputConfig 是工具输出截断 + 落盘指针的可序列化配置（与 llm 包的同名
// 结构字段对齐，但定义在 config 包内以避免 config↔llm 循环依赖；调用方负责转换）。
type ToolOutputConfig struct {
	// MaxLines 截断行数阈值（0=回退默认）。
	MaxLines int
	// MaxBytes 截断字节阈值（0=回退默认）。
	MaxBytes int
	// OffloadEnabled 是否启用落盘指针。
	OffloadEnabled bool
	// OffloadSubdir 落盘子目录（相对工作空间根）。
	OffloadSubdir string
}

// GetToolOutputConfig 返回工具输出截断 + 落盘指针的配置。
// 零值字段由调用方回退到 llm.DefaultToolOutputConfig() 的默认值。
func (b *Builder) GetToolOutputConfig() ToolOutputConfig {
	return ToolOutputConfig{
		MaxLines:       b.store.GetInt(KeyToolOutputMaxLines, 0),
		MaxBytes:       b.store.GetInt(KeyToolOutputMaxBytes, 0),
		OffloadEnabled: b.store.GetBool(KeyToolOutputOffload, true),
		OffloadSubdir:  b.store.GetString(KeyToolOutputSubdir, "tool-output"),
	}
}

// ToolOutputMetaSpecs 返回工具输出配置项的元数据。
func ToolOutputMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeyToolOutputMaxLines, Category: "ToolOutput", Description: "工具输出截断的行数阈值（默认 500）。超过此行数且超过字节阈值时，输出被截断为头+尾预览+指针。"},
		{Key: KeyToolOutputMaxBytes, Category: "ToolOutput", Description: "工具输出截断的字节阈值（默认 51200，即 50KB）。"},
		{Key: KeyToolOutputOffload, Category: "ToolOutput", Description: "是否启用落盘指针（默认 true）。开启且输出被截断时，完整原文写入工作空间 tool-output 子目录，主上下文仅留预览+指针+子 agent 委托提示；关闭则纯 head+tail 截断。"},
		{Key: KeyToolOutputSubdir, Category: "ToolOutput", Description: "落盘文件所在子目录（相对工作空间根，默认 tool-output）。"},
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
		{Key: KeySystemProxy, Category: "System", Description: "全局出口代理（URL，如 http://user:pass@host:port、socks5://host:port）。为空时直连（默认）。设置后主机侧所有出站请求（LLM、channel 等）与 bot Docker 容器内的请求统一走该代理，是「全站出口收敛 / SSRF 防护」的最简开关。"},
	}
}

// --- MemoryWindow 配置（记忆召回窗口 / 上下文预算）---

// MemoryWindowConfig 描述记忆召回窗口的全部可调参数。
// 这些参数原硬编码在 agent/memory/window.go 的 DefaultWindowConfig()，
// 现集中到配置模块，用户可在前端「系统配置」页编辑并持久化。
// 未配置的字段自动使用 DefaultMemoryWindowConfig() 的值。
//
// 注意：记忆窗口在 bot 初始化时（NewWindow）读取一次并缓存于 bot 生命周期内，
// 因此修改后需重启 bot 才生效（与多数需要重启的基础设施配置一致）。
type MemoryWindowConfig struct {
	// MaxContextTokens 模型最大上下文窗口（token 数）。GLM-5.2=128000。
	MaxContextTokens int
	// ReservedTokens 为 system prompt / tool 定义等固定内容预留的 token 数。
	ReservedTokens int
	// OutputReserve 为 LLM 输出预留的 token 数。
	OutputReserve int
	// BudgetRatio memory 可使用的窗口比例（0.0~1.0）。
	BudgetRatio float64
	// MaxMemoryTokens memory 注入的硬上限（token 数）。
	MaxMemoryTokens int
	// CompressThreshold 触发压缩的阈值比例（0.0~1.0）。
	CompressThreshold float64
}

// DefaultMemoryWindowConfig 返回记忆窗口的默认配置值（与 GLM-5.2 128K 对齐）。
// 这是集中后的唯一来源；agent/memory/window.go 的 DefaultWindowConfig() 仅作
// 无配置时的内部兜底，二者应保持同步。
func DefaultMemoryWindowConfig() MemoryWindowConfig {
	return MemoryWindowConfig{
		MaxContextTokens:  128000,
		ReservedTokens:    2000,
		OutputReserve:     4096,
		BudgetRatio:       0.15,
		MaxMemoryTokens:   7281, // ≈21843 字符（×3 估算）的记忆注入硬上限。
		CompressThreshold: 0.8,
	}
}

// GetMemoryWindowConfig 从 Store 读取记忆窗口配置，未设置的字段自动填充默认值。
func (b *Builder) GetMemoryWindowConfig() MemoryWindowConfig {
	d := DefaultMemoryWindowConfig()
	return MemoryWindowConfig{
		MaxContextTokens:  b.store.GetInt(KeyMemoryWindowMaxContextTokens, d.MaxContextTokens),
		ReservedTokens:    b.store.GetInt(KeyMemoryWindowReservedTokens, d.ReservedTokens),
		OutputReserve:     b.store.GetInt(KeyMemoryWindowOutputReserve, d.OutputReserve),
		BudgetRatio:       b.store.GetFloat64(KeyMemoryWindowBudgetRatio, d.BudgetRatio),
		MaxMemoryTokens:   b.store.GetInt(KeyMemoryWindowMaxMemoryTokens, d.MaxMemoryTokens),
		CompressThreshold: b.store.GetFloat64(KeyMemoryWindowCompressThreshold, d.CompressThreshold),
	}
}

// MemoryWindowMetaSpecs 返回记忆窗口配置项的元数据，用于注册到前端设置界面。
func MemoryWindowMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeyMemoryWindowMaxContextTokens, Category: "MemoryWindow", Description: "模型最大上下文窗口（token 数）。GLM-5.2=128000。记忆预算 = (此值 - 预留 - 输出预留) × 预算比例，再受 max_memory_tokens 硬上限约束。修改后需重启 bot 生效。"},
		{Key: KeyMemoryWindowReservedTokens, Category: "MemoryWindow", Description: "为 system prompt / tool 定义等固定内容预留的 token 数（默认 2000）。修改后需重启 bot 生效。"},
		{Key: KeyMemoryWindowOutputReserve, Category: "MemoryWindow", Description: "为 LLM 输出预留的 token 数（默认 4096）。修改后需重启 bot 生效。"},
		{Key: KeyMemoryWindowBudgetRatio, Category: "MemoryWindow", Description: "memory 可使用的窗口比例 0.0~1.0（默认 0.15）。修改后需重启 bot 生效。"},
		{Key: KeyMemoryWindowMaxMemoryTokens, Category: "MemoryWindow", Description: "记忆注入的硬上限 token 数（默认 7281，约 21843 字符）。无论可用空间多大，实际注入的 memory context 不超过此值。修改后需重启 bot 生效。"},
		{Key: KeyMemoryWindowCompressThreshold, Category: "MemoryWindow", Description: "触发记忆压缩的阈值比例 0.0~1.0（默认 0.8）。修改后需重启 bot 生效。"},
	}
}

// --- Pipeline 模式配置 ---

// GetPipelineMode 返回指定 bot 的 pipeline 装配模式。
// 优先读取 per-bot 键 bot.<bot_id>.pipeline_mode，缺失时回退全局 pipeline.mode（默认 "standard"）。
// 取值："standard"（默认，完整链路）/ "lurk-only"（只学习不发言）/ "code"（启用 run_code 式代码编排工具）。
// 该模式经由 pipeline.Builder.WithMode 传入装配器，是 stage / tool 花名册的驱动源
// （对应 harness 的 agent preset / agent.cordis.yml 插件清单）。
func (b *Builder) GetPipelineMode(botID string) string {
	if m := b.store.GetString(BotPipelineModeKey(botID), ""); m != "" {
		return m
	}
	return b.store.GetString(KeyPipelineMode, "standard")
}

// GetMemoryBackfillEnabled 读取指定 bot 的回灌开关：per-bot 优先，否则取全局
// memory.backfill.enabled（默认 true）。关闭后即使 tiered 记忆被清空也不会从历史
// chat_messages 回灌（除非显式删除水位线键强制重灌）。
func (b *Builder) GetMemoryBackfillEnabled(botID string) bool {
	return b.store.GetBool(
		BotMemoryBackfillEnabledKey(botID),
		b.store.GetBool(KeyMemoryBackfillEnabled, true),
	)
}

// PipelineModeMetaSpecs 返回 pipeline 模式配置项的元数据。
func PipelineModeMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeyPipelineMode, Category: "Pipeline", Description: "全局默认 pipeline 装配模式（默认 standard）。取值 standard / lurk-only / code。可用 bot.<botID>.pipeline_mode 按 bot 覆盖。"},
		{Key: "bot.<botID>.pipeline_mode", Category: "Pipeline", Description: "单 bot 覆盖 pipeline 模式。取值同 pipeline.mode；未设置时回退全局默认值。"},
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

// --- Dreaming 配置 ---

// DreamingConfig 描述 per-bot 梦境巩固系统的配置。
type DreamingConfig struct {
	// Enabled 是否启用梦境巩固。
	Enabled bool `json:"enabled"`
	// Schedule cron 调度表达式（默认 "0 3 * * *"，即每天凌晨 3 点）。
	Schedule string `json:"schedule"`
}

// DefaultDreamingConfig 返回梦境巩固的默认配置。
func DefaultDreamingConfig() DreamingConfig {
	return DreamingConfig{
		Enabled:  false,
		Schedule: "0 3 * * *",
	}
}

// GetDreamingConfig 读取指定 Bot 的梦境巩固配置。
// 键格式：bot.<bot_id>.dreaming.enabled / bot.<bot_id>.dreaming.schedule
func (b *Builder) GetDreamingConfig(botID string) DreamingConfig {
	d := DefaultDreamingConfig()
	return DreamingConfig{
		Enabled:  b.store.GetBool(BotDreamingKey(botID, "enabled"), d.Enabled),
		Schedule: b.store.GetString(BotDreamingKey(botID, "schedule"), d.Schedule),
	}
}

// SetDreamingConfig 持久化指定 Bot 的梦境巩固配置。
func (b *Builder) SetDreamingConfig(ctx context.Context, botID string, cfg DreamingConfig) error {
	if err := b.store.Set(ctx, BotDreamingKey(botID, "enabled"), boolStr(cfg.Enabled)); err != nil {
		return err
	}
	if cfg.Schedule != "" {
		return b.store.Set(ctx, BotDreamingKey(botID, "schedule"), cfg.Schedule)
	}
	return nil
}

// DreamingMetaSpecs 返回梦境巩固配置项的元数据。
func DreamingMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: "bot.dreaming.enabled", Category: "Dreaming", Description: "是否启用梦境巩固（后台记忆整理，默认关闭）。键格式 bot.<botID>.dreaming.enabled"},
		{Key: "bot.dreaming.schedule", Category: "Dreaming", Description: "梦境巩固的 cron 调度表达式（默认 0 3 * * *，即每天凌晨 3 点）。键格式 bot.<botID>.dreaming.schedule"},
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// defaultTimezoneName 返回服务器本地时区的 IANA 名称或偏移描述，供前端展示默认值。
// Windows 上 time.Local.String() 返回 "Local"，Zone() 返回缩写（如 CST），
// 都不是有效的 IANA 标识符，所以从 UTC 偏移推算一个可读的固定时区名称。
func defaultTimezoneName() string {
	// 尝试获取 IANA 名称（Linux 上通常有效）
	name, _ := time.Now().Zone()
	if name != "" && name != "Local" && name != "UTC" {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	// 回退：从偏移推算 Etc/GMT 格式
	_, offset := time.Now().Zone()
	if offset == 0 {
		return "UTC"
	}
	hours := offset / 3600
	if offset%3600 == 0 {
		// IANA 的 Etc/GMT 系列符号是反的：Etc/GMT-8 = UTC+8
		if hours > 0 {
			return fmt.Sprintf("Etc/GMT-%d", hours)
		}
		return fmt.Sprintf("Etc/GMT+%d", -hours)
	}
	return "UTC"
}

// ============================================================================
// 配置项元数据聚合 — 供启动时注册和前端设置界面使用
// ============================================================================

// APIMetaSpecs 返回 API 配置项的元数据。
func APIMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeyAPIAddr, Category: "API", Description: "HTTP 服务器监听地址（默认 :8080）"},
		{Key: KeyAPICORSOrigins, Category: "API", Description: "允许的 CORS 来源列表（逗号分隔，为空时仅允许 localhost）"},
		{Key: KeyAPICookieSecure, Category: "API", Description: "Cookie 是否仅通过 HTTPS 传输（默认 true）"},
		{Key: KeyChatContextLimit, Category: "API", Description: "LLM 上下文加载的最大历史消息数（默认 20）"},
	}
}

// BotMetaSpecs 返回全局 Bot 配置项的元数据。
func BotMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeyBotSystemPrompt, Category: "Bot", Description: "全局系统 prompt 覆盖（可选，优先级低于 SOUL.md）"},
		{Key: KeyBotModel, Category: "Bot", Description: "默认模型名（已被 per-bot LLM 分配取代，仅作回退）"},
		{Key: KeyBotTemperature, Category: "Bot", Description: "采样温度（默认 0.7，0 表示确定性输出）"},
		{Key: KeyBotMaxTokens, Category: "Bot", Description: "最大输出 token 数（默认 4096）"},
		{Key: KeyBotWorkers, Category: "Bot", Description: "Bot 并发 worker 数（默认 4）"},
	}
}

// DBMetaSpecs 返回数据库配置项的元数据。
func DBMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeyDBPath, Category: "Database", Description: "SQLite 数据库文件路径（默认 data/thinkbot.db，落在 data 卷）"},
	}
}

// LogMetaSpecs 返回日志配置项的元数据。
func LogMetaSpecs() []MetaSpec {
	return []MetaSpec{
		{Key: KeyLogLevel, Category: "Log", Description: "日志级别：debug / info / warn / error（默认 info）"},
	}
}

// AllMetaSpecs 汇总所有模块的配置项元数据。
func AllMetaSpecs() []MetaSpec {
	var specs []MetaSpec
	specs = append(specs, APIMetaSpecs()...)
	specs = append(specs, BotMetaSpecs()...)
	specs = append(specs, DBMetaSpecs()...)
	specs = append(specs, LogMetaSpecs()...)
	specs = append(specs, WorkflowMetaSpecs()...)
	specs = append(specs, EngagementMetaSpecs()...)
	specs = append(specs, SoulMetaSpecs()...)
	specs = append(specs, WorkspaceMetaSpecs()...)
	specs = append(specs, SystemMetaSpecs()...)
	specs = append(specs, DreamingMetaSpecs()...)
	specs = append(specs, ToolPolicyMetaSpecs()...)
	specs = append(specs, MemoryWindowMetaSpecs()...)
	return specs
}

// GlobalMetaSpecs 仅返回适合在系统设置页面展示的全局配置项。
// 排除 Bot / Soul / Engagement / Dreaming / ToolPolicy 等 per-bot 配置，
// 以及 Database / Workspace 等需要重启才生效的基础设施配置。
// 记忆窗口（MemoryWindow）是全局共享的模型上下文预算配置，
// 虽需重启 bot 生效，但属于用户应在前端可调的模型参数，故纳入。
func GlobalMetaSpecs() []MetaSpec {
	return append(SystemMetaSpecs(), MemoryWindowMetaSpecs()...)
}

// DefaultMap 返回所有配置项的默认值映射，供前端设置界面填充空值。
func DefaultMap() map[string]string {
	return map[string]string{
		// API
		KeyAPIAddr:          ":8080",
		KeyAPICORSOrigins:   "",
		KeyAPICookieSecure:  "true",
		KeyChatContextLimit: "20",
		// Bot
		KeyBotSystemPrompt: "",
		KeyBotModel:        "",
		KeyBotTemperature:  "0.7",
		KeyBotMaxTokens:    "4096",
		KeyBotWorkers:      "4",
		// Database
		KeyDBPath: "data/thinkbot.db",
		// Log
		KeyLogLevel: "info",
		// System
		KeySystemTimezone: defaultTimezoneName(),
		// Workflow
		KeyWorkflowMaxParallel:      "3",
		KeyWorkflowMaxRetries:       "2",
		KeyWorkflowMaxIterations:    "3",
		KeyWorkflowRetryInitialMS:   "500",
		KeyWorkflowRetryMaxMS:       "10000",
		KeyWorkflowScheduleInterval: "200",
		KeyWorkflowAnalyzerTemp:     "0.3",
		// 需求分析器输出长度 cap（针对「生成 DAG JSON」这一具体任务的输出上限）。
		//
		// 注意：这是「思考 + 正文」共享的总输出预算，不是「正文可用长度」。
		// 思考型模型（GLM-4.6+ 服务端默认开启 thinking、o 系列、Claude thinking 等）
		// 的 reasoning 内容同样从 max_tokens 里扣，且会先于正文产出。
		// 曾经设为 8192 导致真实故障：思考吃掉大半预算后，DAG JSON 写到一半被硬截断，
		// 解析报 "unexpected end of JSON input"，5 次重试全挂、workflow 直接 failed。
		// 因此这里必须按「思考预算 + JSON 正文」之和留足余量：DAG JSON 本身通常数 KB，
		// 32768 可覆盖复杂需求下的长思考，同时仍远低于模型 128K 上限，不会浪费预算。
		// 留空/0 时回退到当前模型 ModelDef.MaxTokens（真实能力值，如 glm-5.2=128K）。
		KeyWorkflowAnalyzerStuckTimeout: "180",
		KeyWorkflowAnalyzerMaxDuration:  "600000",
		KeyWorkflowGoalMaxIterations:    "5",
		// Engagement
		KeyEngagementEnabled:            "false",
		KeyEngagementReplyProbability:   "0.15",
		KeyEngagementRateLimitCapacity:  "3",
		KeyEngagementRateLimitInterval:  "1h",
		KeyEngagementBackoffBaseSeconds: "10",
		KeyEngagementBackoffCapSeconds:  "300",
		KeyEngagementBackoffStartCount:  "3",
		KeyEngagementBurstInterval:      "5",
		KeyEngagementWaitTimeout:        "30",
		KeyEngagementBackoffBypass:      "0",
		KeyEngagementThreshold:          "0",
		// Soul
		KeySoulReloadInterval: "5s",
		// Workspace
		KeyWorkspaceDir:         "data/workspaces",
		KeySandboxBackend:       "auto",
		KeySandboxImage:         "alpine:latest",
		KeySandboxRequireDocker: "true",
		KeySandboxTimeout:       "0",
		KeySandboxStuckTimeout:  "300",
		// Dreaming
		"bot.dreaming.enabled":  "false",
		"bot.dreaming.schedule": "0 3 * * *",
		// MemoryWindow
		KeyMemoryWindowMaxContextTokens:  "128000",
		KeyMemoryWindowReservedTokens:    "2000",
		KeyMemoryWindowOutputReserve:     "4096",
		KeyMemoryWindowBudgetRatio:       "0.15",
		KeyMemoryWindowMaxMemoryTokens:   "7281",
		KeyMemoryWindowCompressThreshold: "0.8",
	}
}
