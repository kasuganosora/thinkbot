package config

import (
	"fmt"
	"strings"
)

// 标准配置键前缀。
const (
	PrefixDB         = "db"
	PrefixBot        = "bot"
	PrefixChannel    = "channel"
	PrefixLog        = "log"
	PrefixMemory     = "memory"
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
	KeyWorkflowMaxParallel      = "workflow.max_parallel"
	KeyWorkflowMaxRetries       = "workflow.max_retries"
	KeyWorkflowMaxIterations    = "workflow.max_iterations"
	KeyWorkflowRetryInitialMS   = "workflow.retry_initial_ms"
	KeyWorkflowRetryMaxMS       = "workflow.retry_max_ms"
	KeyWorkflowScheduleInterval = "workflow.schedule_interval_ms"
	KeyWorkflowAnalyzerTemp     = "workflow.analyzer_temperature"
	// 注意：这里曾有 workflow.analyzer_max_tokens，已移除。
	// 分析器的输出预算不再单独配置，统一跟随 bot 所选模型的 maxTokens
	// （provider.<x>.models[].maxTokens → ModelDef.MaxTokens）。
	// 独立旋钮曾被播种为 8192，与 glm-5.2 的 128K 能力脱节，导致 DAG JSON
	// 被硬截断、analyzer 连续 5 次解析失败，故取消该配置面。
	// KeyWorkflowAnalyzerStuckTimeout 需求分析器（流式 LLM 调用）的卡死看门狗阈值（秒，默认 180，即 3 分钟）。
	// 分析器改用流式委托 + 卡死看门狗：只要 LLM 持续输出 token（哪怕慢）就不杀，
	// 只有连续无 token 超过该时长（且已过首 token 宽限期）才判卡死终止。硬上限 = 该值 ×3 派生。
	// 这是「看门狗判断真卡死」而非「固定超时一刀切」——正常处理超长 prompt（如 86 个 lint 问题）不会被打断。
	KeyWorkflowAnalyzerStuckTimeout = "workflow.analyzer_stuck_timeout"
	// KeyWorkflowAnalyzerMaxDuration 需求分析阶段的「总时长上限」（毫秒，默认 600000=10 分钟）。
	// 与上面的「卡死看门狗」（单次调用无 token 才杀）不同，这是分析阶段整轮的硬上限：
	// GLM 频繁退化时空分析器会反复重试，最坏可达数十分钟「分析中」黑洞。该上限保证
	// 整轮分析无论重试多少次都在该时长内结束（成功或明确失败），前端不再无限转圈。
	KeyWorkflowAnalyzerMaxDuration = "workflow.analyzer_max_duration_ms"
	// KeyWorkflowGoalMaxIterations 目标模式（闭环循环）的全局最大迭代轮数（默认 5）。
	// review 节点在节点级迭代（MaxIterations）仍不通过时，回退到 Feedback 目标节点重新执行，
	// 每轮闭环计数 +1；达到该上限仍不通过则工作流失败。0 表示使用代码兜底默认（5）。
	KeyWorkflowGoalMaxIterations = "workflow.goal_max_iterations"
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
	KeyEngagementProfile            = "engagement.profile"
	KeyEngagementThreshold          = "engagement.engagement_threshold"
	KeyEngagementAutoAdjustFreq     = "engagement.auto_adjust_frequency"
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

	// KeySandboxBackend 沙箱后端："auto"（默认，有 Docker 用容器隔离否则 local）|"docker"|"local"。
	KeySandboxBackend = "sandbox.backend"

	// KeySandboxImage 沙箱 Docker 镜像（docker 模式下 per-bot 长期容器使用）。
	KeySandboxImage = "sandbox.image"

	// KeySandboxRequireDocker 是否在 auto 模式下强制要求 Docker 可用。
	// true：auto 模式下探测不到 Docker 直接报错，不降级到 local（避免无隔离裸跑）；
	// false（默认）：auto 模式下探测不到 Docker 则降级 local 进程执行。
	// 注意：Backend 显式设为 "docker" 时本就强制要求 Docker，与本键无关。
	KeySandboxRequireDocker = "sandbox.require_docker"

	// KeySandboxTimeout bot 在沙箱里执行单条命令的「硬上限」秒数。
	// 默认 0 表示自动 = 卡死阈值 × 3（见 hardTimeoutFactor），不写死固定时长；
	// 设为正整数时显式覆盖该硬上限。作为卡死看门狗的最终兜底：即便命令一直在输出，
	// 超过它也会被强制终止，防止无限挂起。
	// 注意：单条命令默认不再用固定超时一刀切杀掉——真正决定是否终止的是卡死看门狗
	// （见 KeySandboxStuckTimeout）：只要命令持续有输出（哪怕慢）就不杀，只有长时间无输出
	// 才判定卡死。本键是「无论如何都不能超过」的总时长上限。
	KeySandboxTimeout = "sandbox.timeout"

	// KeySandboxStuckTimeout 卡死看门狗阈值（秒，默认 300，即 5 分钟）。
	// 命令连续无 stdout/stderr 输出超过该时长，且已过启动宽限期、进程仍存活，
	// 则判定为「卡死（无进展）」并终止。这是区分「编译慢」与「死锁卡死」的关键：
	// 正常运行的慢命令（持续输出）靠本阈值放行，不会误杀。
	KeySandboxStuckTimeout = "sandbox.stuck_timeout"
)

// ToolOutput 键：工具输出截断 + 落盘指针（借鉴 opencode 的 token 优化）。
const (
	// KeyToolOutputMaxLines 工具输出截断的行数阈值（默认 500）。
	// 超过此行数且超过字节阈值时，输出被截断为「头+尾预览+指针」。
	KeyToolOutputMaxLines = "tool_output.max_lines"

	// KeyToolOutputMaxBytes 工具输出截断的字节阈值（默认 51200，即 50KB）。
	KeyToolOutputMaxBytes = "tool_output.max_bytes"

	// KeyToolOutputOffload 是否启用落盘指针（默认 true）。
	// 开启且输出被截断时，完整原文写入 bot 工作空间的 tool_output 子目录，
	// 主上下文仅留预览+指针+子 agent 委托提示；关闭则纯 head+tail 截断。
	KeyToolOutputOffload = "tool_output.offload"

	// KeyToolOutputSubdir 落盘文件所在的子目录（相对工作空间根，默认 "tool-output"）。
	KeyToolOutputSubdir = "tool_output.subdir"
)

// System 键。
const (
	// KeySystemTimezone 系统时区（IANA 时区标识符，如 "Asia/Shanghai"、"UTC"）。
	// 为空时使用服务器本地时区（time.Local）。
	// 影响范围：bot 的时间感知、Docker 沙箱容器的 TZ 环境变量。
	KeySystemTimezone = "system.timezone"
)

// Pipeline 键：pipeline 装配模式（对应 deepseek-harness 的 agent preset / 插件花名册）。
const (
	// KeyPipelineMode 全局默认 pipeline 模式（当 bot 未单独配置时生效）。
	// 取值："standard"（默认）/ "lurk-only" / "code"。
	KeyPipelineMode = "pipeline.mode"
)

// BotPipelineModeKey 返回指定 bot 的 pipeline 模式键：bot.<bot_id>.pipeline_mode。
func BotPipelineModeKey(botID string) string {
	return "bot." + botID + ".pipeline_mode"
}

// BotReplyControlKey 返回指定 bot 的「回复控制门控」开关键：bot.<bot_id>.reply_control。
// 取值 "true" 时启用：模型必须在回复结尾追加结构化控制 JSON（@@REPLY_CONTROL@@{"send":bool}），
// 缺失/解析失败/send:false → 不出站（fail-closed）。用于治理「模型决定不互动却把独白当回复发出」。
// 默认关闭（"false"），避免影响未显式开启的 bot。
func BotReplyControlKey(botID string) string {
	return "bot." + botID + ".reply_control"
}

// Memory 回灌（backfill）键：历史 chat_messages → L0 守卫。
// 回灌是一次性 bootstrap：把历史对话补灌进记忆 L0，使 dreaming 能处理从未进入记忆
// 系统的历史 backlog。陷阱在于「清空 tiered/memory 表后，每次启动若 tiered 为空又会从
// chat_messages 回潮」。水位线（watermark）独立于 tiered_memories 持久化，记录已补灌的
// 最大 chat_message.id，使上述回潮彻底阻断；要强制重新回灌只需删除该水位线键。
const (
	// KeyMemoryBackfillEnabled 全局回灌总开关默认值。
	// per-bot 键 bot.<bot_id>.memory.backfill.enabled 可覆盖。
	KeyMemoryBackfillEnabled = "memory.backfill.enabled"
)

// BotMemoryBackfillEnabledKey 返回指定 bot 的回灌开关键。
func BotMemoryBackfillEnabledKey(botID string) string {
	return "bot." + botID + ".memory.backfill.enabled"
}

// BotMemoryBackfillWatermarkKey 返回指定 bot 的回灌水位线键（chat_messages 时代遗留）。
// 注意：事件流改造后回灌改用 BotMemoryBackfillEventWatermarkKey（事件流 id 空间），
// 本键不再用于回灌，保留仅为兼容旧部署已写入的配置项。
func BotMemoryBackfillWatermarkKey(botID string) string {
	return "bot." + botID + ".memory.backfill.watermark"
}

// BotMemoryBackfillEventWatermarkKey 返回指定 bot 的事件流回灌水位线键。
// 值为已补灌进 L0 的最大 user_message_events.id（十进制字符串），独立于 tiered_memories。
// 与旧 chat_messages 时代的水位线键（BotMemoryBackfillWatermarkKey）隔离，避免 id 空间
// 冲突导致首次回灌漏读历史。
func BotMemoryBackfillEventWatermarkKey(botID string) string {
	return "bot." + botID + ".memory.backfill.event_watermark"
}

// ToolPolicyKey 返回指定 bot 的工具权限策略 JSON 的数据库键。
// 格式：tools.<bot_id>.policy
// 值为 ToolPolicy 的 JSON 字符串。
func ToolPolicyKey(botID string) string {
	return "tools." + botID + ".policy"
}

// BotDreamingKey 返回指定 bot 的梦境巩固配置键。
// sub 为具体配置项名称（如 "enabled"、"schedule"）。
// 格式：bot.<bot_id>.dreaming.<sub>
// 例如：bot.mybot.dreaming.enabled → "true"
func BotDreamingKey(botID, sub string) string {
	return "bot." + botID + ".dreaming." + sub
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

// BotTokenQuotaKey 返回 Bot 级月 Token 额度配置键。
// 格式：bot.<bot_id>.token_quota
// 值为 int64（tokens/月），0 = 不限制。
func BotTokenQuotaKey(botID string) string {
	return "bot." + botID + ".token_quota"
}

// BotTokenQuotaChannelKey 返回 channel 级 Token 额度配置键。
// 格式：bot.<bot_id>.token_quota.channel.<channel_type>
// 例如：bot.mybot.token_quota.channel.telegram → "500000"
func BotTokenQuotaChannelKey(botID, channelType string) string {
	return "bot." + botID + ".token_quota.channel." + channelType
}

// BotTokenQuotaChatKey 返回 chat 级 Token 额度配置键（最细粒度）。
// 格式：bot.<bot_id>.token_quota.channel.<channel_type>.<chat_id>
// 例如：bot.mybot.token_quota.channel.telegram.-123456 → "100000"
func BotTokenQuotaChatKey(botID, channelType, chatID string) string {
	return "bot." + botID + ".token_quota.channel." + channelType + "." + chatID
}

// SystemTokenQuotaKey 返回系统级月 Token 额度配置键。
// 格式：system.token_quota
// 例如：system.token_quota → "2000000"
func SystemTokenQuotaKey() string {
	return "system.token_quota"
}

// BotAdaptiveEngagementKey 返回 Bot 级自适应 engagement 配置键。
// sub 为具体配置项名称（如 "enabled"、"channel.<type>.enabled"）。
// 格式：bot.<bot_id>.engagement.adaptive.<sub>
// 例如：bot.mybot.engagement.adaptive.enabled → "true"
//
// 层级继承关系（从粗到细）：
//
//	bot.<id>.engagement.adaptive.enabled                     → Bot 全局开关
//	bot.<id>.engagement.adaptive.channel.<type>.enabled      → Channel 类型级开关
//	bot.<id>.engagement.adaptive.channel.<type>.<chatid>.enabled → 具体群/单聊级开关
func BotAdaptiveEngagementKey(botID, sub string) string {
	return "bot." + botID + ".engagement.adaptive." + sub
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

// ValidateKey 检查配置键是否符合规范（小写字母/数字/点号/连字符/下划线）。
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidKey)
	}
	for _, ch := range key {
		if (ch < 'a' || ch > 'z') &&
			(ch < '0' || ch > '9') &&
			ch != '.' && ch != '_' && ch != '-' {
			return fmt.Errorf("%w: key %q contains invalid character %q", ErrInvalidKey, key, ch)
		}
	}
	return nil
}
