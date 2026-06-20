package bot

import (
	"maps"
	"time"
)

// ============================================================================
// BotConfig — Bot 级别配置
// ============================================================================

// BotConfig 是单个 Bot 的配置。
// 它携带 Bot 运行所需的所有参数，包括 LLM 配置、行为参数、扩展元数据等。
type BotConfig struct {
	// Workers 并发处理 worker 数量（默认 4）。
	// 每个 Bot 拥有独立的 worker pool。
	Workers int `json:"workers" yaml:"workers"`

	// IngressBufferSize Ingress 缓冲区大小（默认 256）。
	IngressBufferSize int `json:"ingressBufferSize" yaml:"ingressBufferSize"`

	// SystemPrompt Bot 的系统提示词（传给 LLM Stage）。
	SystemPrompt string `json:"systemPrompt" yaml:"systemPrompt"`

	// Model LLM 模型标识（如 "gpt-4o"、"claude-3.5-sonnet"）。
	Model string `json:"model" yaml:"model"`

	// Temperature LLM 温度参数。
	Temperature float64 `json:"temperature" yaml:"temperature"`

	// MaxTokens LLM 最大输出 token 数。
	MaxTokens int `json:"maxTokens" yaml:"maxTokens"`

	// LLMMain 主力 LLM 模型 ID（对应 config 中 llm.models.<id>）。
	// 用于深度对话、工具调用等核心任务。
	LLMMain string `json:"llmMain,omitempty" yaml:"llmMain,omitempty"`

	// LLMLight 低成本 LLM 模型 ID（对应 config 中 llm.models.<id>）。
	// 用于标题提取、简单分类等不需要深度思考的任务。
	// 为空时回退到 LLMMain。
	LLMLight string `json:"llmLight,omitempty" yaml:"llmLight,omitempty"`

	// Timezone Bot 的时区（如 "Asia/Shanghai"、"UTC"、"America/New_York"）。
	// 用于 Cron 定时任务、时间相关工具等。
	// 空字符串表示使用系统本地时区。
	// 无效时区名称会安全回退到 UTC。
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`

	// Extra 扩展配置（Stage 自定义参数等）。
	// Stage 可通过 Envelope.Get("bot.config") 访问整个 BotConfig，
	// 或通过此字段访问自定义参数。
	Extra map[string]any `json:"extra,omitempty" yaml:"extra,omitempty"`
}

// GetSystemPrompt 返回 Bot 的系统提示词。
// 此方法使 BotConfig 满足 prompt.Stage 中 fallbackPrompt 的 duck-type 接口。
func (c BotConfig) GetSystemPrompt() string {
	return c.SystemPrompt
}

// Location 解析 Timezone 字段为 *time.Location。
// 空字符串 → 系统本地时区。
// 无效名称 → UTC（安全回退）。
func (c BotConfig) Location() *time.Location {
	if c.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// DefaultBotConfig 返回合理的默认配置。
func DefaultBotConfig() BotConfig {
	return BotConfig{
		Workers:           4,
		IngressBufferSize: 256,
		Temperature:       0.7,
		MaxTokens:         4096,
	}
}

// Merge 将 other 中的非零值合并到当前配置中。
// 用于配置覆盖场景（如从文件加载后覆盖默认值）。
func (c BotConfig) Merge(other BotConfig) BotConfig {
	if other.Workers > 0 {
		c.Workers = other.Workers
	}
	if other.IngressBufferSize > 0 {
		c.IngressBufferSize = other.IngressBufferSize
	}
	if other.SystemPrompt != "" {
		c.SystemPrompt = other.SystemPrompt
	}
	if other.Model != "" {
		c.Model = other.Model
	}
	if other.Temperature != 0 {
		c.Temperature = other.Temperature
	}
	if other.MaxTokens > 0 {
		c.MaxTokens = other.MaxTokens
	}
	if other.LLMMain != "" {
		c.LLMMain = other.LLMMain
	}
	if other.LLMLight != "" {
		c.LLMLight = other.LLMLight
	}
	if other.Timezone != "" {
		c.Timezone = other.Timezone
	}
	if other.Extra != nil {
		if c.Extra == nil {
			c.Extra = make(map[string]any)
		}
		maps.Copy(c.Extra, other.Extra)
	}
	return c
}
