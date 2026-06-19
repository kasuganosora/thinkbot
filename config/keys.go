package config

import (
	"fmt"
	"strings"
)

// 标准配置键前缀。
const (
	PrefixDB       = "db"
	PrefixLLM      = "llm"
	PrefixBot      = "bot"
	PrefixChannel  = "channel"
	PrefixLog      = "log"
	PrefixMemory   = "memory"
	PrefixTracing    = "tracing"
	PrefixWorkflow   = "workflow"
	PrefixEngagement = "engagement"
	PrefixSoul       = "soul"
	PrefixTools      = "tools"
)

// Bot 键。
const (
	KeyBotSystemPrompt = "bot.system_prompt"
	KeyBotModel        = "bot.model"
	KeyBotTemperature  = "bot.temperature"
	KeyBotMaxTokens    = "bot.max_tokens"
	KeyBotWorkers      = "bot.workers"
)

// 数据库键。
const (
	KeyDBPath = "db.path"
)

// 日志键。
const (
	KeyLogLevel = "log.level"
)

// Workflow 键。
const (
	KeyWorkflowMaxParallel       = "workflow.max_parallel"
	KeyWorkflowMaxRetries        = "workflow.max_retries"
	KeyWorkflowMaxIterations     = "workflow.max_iterations"
	KeyWorkflowRetryInitialMS    = "workflow.retry_initial_ms"
	KeyWorkflowRetryMaxMS        = "workflow.retry_max_ms"
	KeyWorkflowScheduleInterval  = "workflow.schedule_interval_ms"
	KeyWorkflowAnalyzerTemp      = "workflow.analyzer_temperature"
	KeyWorkflowAnalyzerMaxTokens = "workflow.analyzer_max_tokens"
)

// Engagement 键。
const (
	KeyEngagementEnabled            = "engagement.enabled"
	KeyEngagementChannels           = "engagement.channels"
	KeyEngagementReplyProbability   = "engagement.reply_probability"
	KeyEngagementCooldown           = "engagement.cooldown"
	KeyEngagementRateLimitCapacity  = "engagement.rate_limit_capacity"
	KeyEngagementRateLimitInterval  = "engagement.rate_limit_interval"
	KeyEngagementKeywords           = "engagement.keywords"
	KeyEngagementLLMJudgeEnabled    = "engagement.llm_judge_enabled"
	KeyEngagementBlockedUsers       = "engagement.blocked_users"
	KeyEngagementBlockedSources     = "engagement.blocked_sources"
	KeyEngagementMinLength          = "engagement.min_length"
	KeyEngagementMaxLength          = "engagement.max_length"
	KeyEngagementBackoffBaseSeconds = "engagement.backoff_base_seconds"
	KeyEngagementBackoffCapSeconds  = "engagement.backoff_cap_seconds"
	KeyEngagementBackoffStartCount  = "engagement.backoff_start_count"
	KeyEngagementBurstInterval      = "engagement.burst_interval_seconds"
	KeyEngagementWaitTimeout        = "engagement.wait_timeout_seconds"
	KeyEngagementBackoffBypass      = "engagement.backoff_bypass_pending"
)

// Soul 键。
//
// 约定优于配置：SOUL.md 默认从二进制目录自动加载，文件存在即生效，
// 无需 enabled 开关。以下配置项仅用于可选的运行时调整。
const (
	// KeySoulReloadInterval 文件变更检测轮询间隔（默认 5s，0=禁用热重载）。
	KeySoulReloadInterval = "soul.reload_interval"

	// KeySoulPromptDir 额外 prompt 段落目录（可选，存放 {order}_{name}.md 文件）。
	KeySoulPromptDir = "soul.prompt_dir"
)

// ToolPolicyKey 返回指定 bot 的工具权限策略 JSON 的数据库键。
// 格式：tools.<bot_id>.policy
// 值为 ToolPolicy 的 JSON 字符串。
func ToolPolicyKey(botID string) string {
	return "tools." + botID + ".policy"
}

// LLMConfigKey 返回存储 LLM 配置 JSON 的数据库键。
// 格式：llm.<llm_id>
// 例如：llm.main、llm.light、llm.claude
func LLMConfigKey(llmID string) string {
	return "llm." + llmID
}

// BotLLMKey 返回 Bot 的 LLM 角色分配键。
// role 为 "main" 或 "light"。
// 格式：bot.<bot_id>.<role>
// 例如：bot.mybot.main、bot.mybot.light
func BotLLMKey(botID, role string) string {
	return "bot." + botID + "." + role
}

// EnvKeyToConfigKey 将环境变量名转换为配置键。
// 规则：小写化，下划线 _ → 点号 .
func EnvKeyToConfigKey(envKey string) string {
	lower := strings.ToLower(envKey)
	return strings.ReplaceAll(lower, "_", ".")
}

// ConfigKeyToEnvKey 将配置键转换为环境变量名。
func ConfigKeyToEnvKey(configKey string) string {
	upper := strings.ToUpper(configKey)
	return strings.ReplaceAll(upper, ".", "_")
}

// ErrInvalidKey 配置键格式错误。
var ErrInvalidKey = fmt.Errorf("config: invalid key format")

// ValidateKey 检查配置键是否符合规范（小写字母/数字/点号）。
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidKey)
	}
	for _, ch := range key {
		if !(ch >= 'a' && ch <= 'z') &&
			!(ch >= '0' && ch <= '9') &&
			ch != '.' && ch != '_' {
			return fmt.Errorf("%w: key %q contains invalid character %q", ErrInvalidKey, key, ch)
		}
	}
	return nil
}
