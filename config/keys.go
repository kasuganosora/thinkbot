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
	PrefixWorkspace  = "workspace"
	PrefixSystem     = "system"
	PrefixAPI        = "api"
)

// API 键。
const (
	// KeyAPIAddr HTTP 服务器监听地址（默认 ":8080"）。
	KeyAPIAddr = "api.addr"

	// KeyAPICORSOrigins 允许的 CORS 来源列表，逗号分隔。
	// 为空时仅允许 localhost 来源（开发模式）。
	KeyAPICORSOrigins = "api.cors_origins"

	// KeyAPICookieSecure Cookie 是否仅通过 HTTPS 传输（默认 false）。
	KeyAPICookieSecure = "api.cookie_secure"

	// KeyChatContextLimit LLM 上下文加载的最大历史消息数（默认 20）。
	KeyChatContextLimit = "api.chat_context_limit"
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

// Workspace 键。
const (
	// KeyWorkspaceDir bot 工作空间根目录的物理路径（默认 "data/workspaces"）。
	// 每个 Bot 在此目录下拥有独立的子目录（{dir}/{botID}/），持久化存储文件。
	// SOUL.md、笔记、配置等数据保存在此目录，重启后不丢失。
	KeyWorkspaceDir = "workspace.dir"
)

// System 键。
const (
	// KeySystemTimezone 系统时区（IANA 时区标识符，如 "Asia/Shanghai"、"UTC"）。
	// 为空时使用服务器本地时区（time.Local）。
	// 影响范围：bot 的时间感知、Docker 沙箱容器的 TZ 环境变量。
	KeySystemTimezone = "system.timezone"
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

// BotTimezoneKey 返回指定 Bot 的时区配置键。
// 格式：bot.<bot_id>.timezone
// 例如：bot.mybot.timezone → "Asia/Shanghai"
func BotTimezoneKey(botID string) string {
	return "bot." + botID + ".timezone"
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
		if (ch < 'a' || ch > 'z') &&
			(ch < '0' || ch > '9') &&
			ch != '.' && ch != '_' {
			return fmt.Errorf("%w: key %q contains invalid character %q", ErrInvalidKey, key, ch)
		}
	}
	return nil
}
