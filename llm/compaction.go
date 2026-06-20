package llm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Context Window 压缩系统
//
// 四层压缩策略：
//
//  1. Pruning（工具输出裁剪）：
//     从最新消息向回扫描，保留最近 PRUNE_PROTECT tokens 的工具输出。
//     保护区内不裁剪；保护区外的旧输出替换为 "[compacted]" 占位符。
//     受保护的工具（如 skill）的输出永不裁剪。
//     仅当可裁剪量 > PRUNE_MINIMUM 时才执行裁剪。
//
//  2. Compaction（对话摘要）：
//     当总 token 超过阈值时，用 LLM 生成旧消息的结构化摘要，
//     保留最近 N 轮完整对话 + 摘要替代旧消息。
//     支持增量摘要：如果已有之前的摘要，则更新而非重新生成。
//
//  3. Error-triggered compaction（错误触发压缩）：
//     当 provider 返回 context overflow 错误时，自动触发压缩流程。
//
//  4. Mid-conversation system message：
//     在对话中插入系统消息（如日期变更提醒），而非修改 system prompt。
// ============================================================================

// Pruning thresholds.
const (
	// PruneMinimum 只有可裁剪量超过此值时才执行裁剪。
	PruneMinimum = 20000

	// PruneProtect 保护区内最近工具输出不被裁剪的 token 数量。
	PruneProtect = 40000

	// DoomLoopThreshold 连续压缩次数上限，防止无限压缩循环。
	DoomLoopThreshold = 3

	// DefaultTailTurns 默认保留的最近对话轮数。
	DefaultTailTurns = 2

	// MinPreserveRecentTokens 最少保留的最近消息 token 数。
	MinPreserveRecentTokens = 2000

	// MaxPreserveRecentTokens 最多保留的最近消息 token 数。
	MaxPreserveRecentTokens = 8000
)

// ProtectedTools 是输出永不裁剪的工具名列表。
var ProtectedTools = map[string]bool{
	"skill": true,
}

// CompactionConfig 配置上下文压缩行为。
type CompactionConfig struct {
	// MaxTokens 上下文窗口的 token 上限。
	// 当估算的总 token 超过此值时触发压缩。
	MaxTokens int

	// ReservedTokens 为系统消息和新回复预留的 token 数量。
	// 可用空间 = MaxTokens - ReservedTokens。
	// 默认 20000。
	ReservedTokens int

	// TailTokens 压缩时保留的最近 token 数量。
	// 这些最近的消息不会被摘要化。
	// 默认 8000。
	TailTokens int

	// TailTurns 保留的最近完整对话轮数。
	// 默认 2。
	TailTurns int

	// MinMessagesToCompact 触发压缩的最小消息数量。
	// 少于这些消息时不压缩（不值得）。
	// 默认 6。
	MinMessagesToCompact int

	// SummaryMaxTokens 摘要的最大 token 数。
	// 默认 4096。
	SummaryMaxTokens int

	// ToolOutputThreshold 单个工具输出的 token 阈值。
	// 超过此值的工具输出在 pruning 阶段会被裁剪。
	// 默认 500。
	ToolOutputThreshold int

	// CompactionPrompt 生成摘要时使用的 system prompt。
	// 为空时使用默认 prompt。
	CompactionPrompt string

	// Auto 是否启用自动压缩。
	// 默认 true。
	Auto bool
}

// DefaultCompactionConfig 返回默认压缩配置。
// 适合 128K context window 的模型。
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		MaxTokens:            128000,
		ReservedTokens:       20000,
		TailTokens:           8000,
		TailTurns:            DefaultTailTurns,
		MinMessagesToCompact: 6,
		SummaryMaxTokens:     4096,
		ToolOutputThreshold:  500,
		Auto:                 true,
	}
}

// CompactionSystemPrompt 是压缩摘要的系统提示词。
const CompactionSystemPrompt = `You are an anchored context summarization assistant for coding sessions.

Summarize only the conversation history you are given. The newest turns may be kept verbatim outside your summary, so focus on the older context that still matters for continuing the work.

If the prompt includes a <previous-summary> block, treat it as the current anchored summary. Update it with the new history by preserving still-true details, removing stale details, and merging in new facts.

Always follow the exact output structure requested by the user prompt. Keep every section, preserve exact file paths and identifiers when known, and prefer terse bullets over paragraphs.

Do not answer the conversation itself. Do not mention that you are summarizing, compacting, or merging context. Respond in the same language as the conversation.`

// SummaryTemplate 是摘要输出的结构化模板。
const SummaryTemplate = `Output exactly the Markdown structure shown inside <template> and keep the section order unchanged. Do not include the <template> tags in your response.
<template>
## Goal
- [single-sentence task summary]

## Constraints & Preferences
- [user constraints, preferences, specs, or "(none)"]

## Progress
### Done
- [completed work or "(none)"]

### In Progress
- [current work or "(none)"]

### Blocked
- [blockers or "(none)"]

## Key Decisions
- [decision and why, or "(none)"]

## Next Steps
- [ordered next actions or "(none)"]

## Critical Context
- [important technical facts, errors, open questions, or "(none)"]

## Relevant Files
- [file or directory path: why it matters, or "(none)"]
</template>

Rules:
- Keep every section, even when empty.
- Use terse bullets, not prose paragraphs.
- Preserve exact file paths, commands, error strings, and identifiers when known.
- Do not mention the summary process or that context was compacted.`

// Compactor 执行上下文压缩。
type Compactor struct {
	config CompactionConfig

	mu              sync.Mutex
	previousSummary string // 上次生成的摘要（用于增量更新）
	compactionCount int    // 连续压缩次数（doom loop 预防）
}

// NewCompactor 创建上下文压缩器。
func NewCompactor(config CompactionConfig) *Compactor {
	if config.MaxTokens <= 0 {
		config = DefaultCompactionConfig()
	}
	if config.ReservedTokens <= 0 {
		config.ReservedTokens = 20000
	}
	if config.TailTokens <= 0 {
		config.TailTokens = 8000
	}
	if config.TailTurns <= 0 {
		config.TailTurns = DefaultTailTurns
	}
	if config.MinMessagesToCompact <= 0 {
		config.MinMessagesToCompact = 6
	}
	if config.SummaryMaxTokens <= 0 {
		config.SummaryMaxTokens = 4096
	}
	if config.ToolOutputThreshold <= 0 {
		config.ToolOutputThreshold = 500
	}
	return &Compactor{config: config}
}

// Config 返回压缩配置。
func (c *Compactor) Config() CompactionConfig {
	return c.config
}

// UsableTokens 返回可用 token 数（MaxTokens - ReservedTokens）。
func (c *Compactor) UsableTokens() int {
	return max(c.config.MaxTokens-c.config.ReservedTokens, c.config.TailTokens)
}

// IsOverflow 检查参数是否超过 token 上限。
func (c *Compactor) IsOverflow(params GenerateParams) bool {
	total := EstimateParamsTokens(params)
	return total >= c.UsableTokens()
}

// IsOverflowByUsage 使用实际 API 返回的 token 用量判断是否溢出。
// 这比估算更准确，因为使用的是 provider 报告的实际 token 数。
func (c *Compactor) IsOverflowByUsage(usage *Usage) bool {
	if usage == nil {
		return false
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.InputTokens + usage.OutputTokens + usage.CachedInputTokens
	}
	return total >= c.UsableTokens()
}

// ShouldCompact 判断是否需要压缩。
func (c *Compactor) ShouldCompact(params GenerateParams) bool {
	if !c.config.Auto {
		return false
	}
	if c.isDoomLoop() {
		return false
	}
	if len(params.Messages) < c.config.MinMessagesToCompact {
		return false
	}
	return c.IsOverflow(params)
}

// isDoomLoop 检查是否陷入了无限压缩循环。
func (c *Compactor) isDoomLoop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.compactionCount >= DoomLoopThreshold
}

// resetDoomLoop 重置压缩计数器。
func (c *Compactor) resetDoomLoop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.compactionCount = 0
}

// PruneToolOutputs 裁剪旧的工具输出。
//
// 策略（参照 prune 逻辑）：
//   - 从最新消息向回扫描，累计工具输出的 token 总量
//   - 跳过受保护工具（如 skill）的输出
//   - 跳过最近 2 轮对话内的工具输出
//   - 仅当保护区外可裁剪量 > PruneMinimum 时才执行裁剪
//   - 被裁剪的输出替换为 "[compacted]" 占位符
func (c *Compactor) PruneToolOutputs(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}

	result := make([]Message, len(messages))
	copy(result, messages)

	// 从后向前累计 token，确定裁剪边界点
	accumulated := 0
	cutoffIdx := len(result)
	turns := 0

	for i := len(result) - 1; i >= 0; i-- {
		// 计算对话轮次（跳过最近 2 轮）
		if result[i].Role == MessageRoleUser {
			turns++
		}
		if turns < 2 {
			continue // 保护最近 2 轮
		}

		msgTokens := EstimateMessageTokens(result[i])
		accumulated += msgTokens

		if accumulated > PruneProtect {
			cutoffIdx = i + 1
			break
		}
	}

	// 计算保护区外可裁剪的总量
	prunableTokens := 0
	for i := 0; i < cutoffIdx && i < len(result); i++ {
		for _, part := range result[i].Content {
			if tr, ok := part.(ToolResultPart); ok {
				if ProtectedTools[tr.ToolName] {
					continue // 受保护工具跳过
				}
				prunableTokens += EstimatePartResultTokens(tr.Result)
			}
		}
	}

	// 仅当可裁剪量足够大时才裁剪（避免微小裁剪没有意义）
	if prunableTokens <= PruneMinimum {
		return result
	}

	// 执行裁剪
	for i := 0; i < cutoffIdx && i < len(result); i++ {
		result[i] = c.compactMessageToolOutputs(result[i])
	}

	return result
}

// compactMessageToolOutputs 裁剪单条消息中过大的工具输出。
func (c *Compactor) compactMessageToolOutputs(msg Message) Message {
	newParts := make([]MessagePart, 0, len(msg.Content))
	for _, part := range msg.Content {
		if tr, ok := part.(ToolResultPart); ok {
			// 受保护工具不裁剪
			if ProtectedTools[tr.ToolName] {
				newParts = append(newParts, tr)
				continue
			}
			tokens := EstimatePartResultTokens(tr.Result)
			if tokens > c.config.ToolOutputThreshold {
				originalLen := 0
				if s, ok := tr.Result.(string); ok {
					originalLen = len(s)
				}
				tr.Result = fmt.Sprintf("[compacted: original %d tokens, %d bytes]", tokens, originalLen)
			}
			newParts = append(newParts, tr)
		} else {
			newParts = append(newParts, part)
		}
	}
	return Message{Role: msg.Role, Content: newParts, Usage: msg.Usage}
}

// Compact 执行完整的上下文压缩流程。
//
// 流程：
//  1. 裁剪旧工具输出（PruneToolOutputs）
//  2. 如果仍然溢出，生成旧消息摘要
//  3. 用摘要 + 最近消息替代旧消息
//
// provider 非空时使用 LLM 生成摘要；为 nil 时仅做 pruning。
func (c *Compactor) Compact(ctx context.Context, params GenerateParams, provider Provider) (GenerateParams, error) {
	// 增加压缩计数
	c.mu.Lock()
	c.compactionCount++
	c.mu.Unlock()

	// Step 1: 裁剪工具输出
	pruned := c.PruneToolOutputs(params.Messages)
	params.Messages = pruned

	if !c.IsOverflow(params) {
		return params, nil
	}

	// Step 2: 需要摘要压缩
	if provider == nil {
		return params, nil
	}

	summaryParams, err := c.summarizeMessages(ctx, params, provider)
	if err != nil {
		return params, fmt.Errorf("compaction: %w", err)
	}

	return summaryParams, nil
}

// summarizeMessages 使用 LLM 生成旧消息的增量摘要。
//
// 策略：
//   - 按 turn-based 方式保留最近的对话轮次
//   - 增量更新之前的摘要（如果有）
//   - 使用结构化 Markdown 模板生成摘要
func (c *Compactor) summarizeMessages(ctx context.Context, params GenerateParams, provider Provider) (GenerateParams, error) {
	messages := params.Messages

	// 按 turn-based 方式确定 head/tail 分割点
	splitIdx := c.selectTailSplit(messages)

	if splitIdx <= 0 {
		// 没有消息可以摘要化（tail 太大或消息太少）
		return params, nil
	}

	oldMessages := messages[:splitIdx]
	tailMessages := messages[splitIdx:]

	// 计算保留区域的 token 预算
	budget := c.preserveRecentBudget()
	tailTokens := EstimateMessagesTokens(tailMessages)
	if tailTokens > budget {
		// tail 太大，尝试进一步分割
		// 按 token 从后向前重新确定 splitIdx
		splitIdx = c.selectTailSplitByTokens(messages, budget)
		if splitIdx <= 0 || splitIdx >= len(messages) {
			return params, nil
		}
		oldMessages = messages[:splitIdx]
		tailMessages = messages[splitIdx:]
	}

	// 构建摘要提示词
	prompt := c.buildSummaryPrompt(oldMessages)

	summaryMessages := []Message{UserMessage(prompt)}

	summaryParams := GenerateParams{
		Model:    params.Model,
		System:   CompactionSystemPrompt,
		Messages: summaryMessages,
	}
	if summaryParams.Model == nil {
		summaryParams.Model = params.Model
	}
	maxTokens := c.config.SummaryMaxTokens
	summaryParams.MaxTokens = &maxTokens

	// 调用 LLM 生成摘要
	temp := 0.3
	summaryParams.Temperature = &temp

	result, err := provider.DoGenerate(ctx, summaryParams)
	if err != nil {
		return params, fmt.Errorf("summarize: %w", err)
	}

	summary := result.Text
	if summary == "" {
		return params, nil
	}

	// 保存摘要供下次增量更新
	c.mu.Lock()
	c.previousSummary = summary
	c.mu.Unlock()

	// 构建压缩后的消息列表：摘要消息 + tail 消息
	compactedMessages := make([]Message, 0, 1+len(tailMessages))

	// 添加摘要作为系统上下文消息
	compactedMessages = append(compactedMessages, SystemMessage(
		fmt.Sprintf("[Conversation Summary]\n%s\n[End of Summary]", summary),
	))

	// 保留最近的完整消息
	compactedMessages = append(compactedMessages, tailMessages...)

	params.Messages = compactedMessages
	return params, nil
}

// selectTailSplit 按 turn-based 方式确定 head/tail 分割点。
// 返回 head 的结束索引（即 tail 的起始索引）。
//
// 保留最近 TailTurns 轮完整对话（用户消息开始的一轮）。
func (c *Compactor) selectTailSplit(messages []Message) int {
	limit := c.config.TailTurns
	if limit <= 0 {
		return len(messages) // 全部保留
	}

	turnCount := 0
	splitIdx := len(messages)

	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == MessageRoleUser {
			turnCount++
			if turnCount >= limit {
				splitIdx = i
				break
			}
		}
	}

	if splitIdx == len(messages) {
		return len(messages)
	}
	return splitIdx
}

// selectTailSplitByTokens 按 token 预算确定分割点。
// 从后向前累计 token，直到超过 budget 为止。
func (c *Compactor) selectTailSplitByTokens(messages []Message, budget int) int {
	accumulated := 0
	for i := len(messages) - 1; i >= 0; i-- {
		msgTokens := EstimateMessageTokens(messages[i])
		if accumulated+msgTokens > budget {
			return i + 1
		}
		accumulated += msgTokens
	}
	return 0
}

// preserveRecentBudget 计算保留最近消息的 token 预算。
// 默认为可用空间的 25%，限制在 [2000, 8000] 范围内。
func (c *Compactor) preserveRecentBudget() int {
	budget := c.UsableTokens() / 4
	return min(MaxPreserveRecentTokens, max(MinPreserveRecentTokens, budget))
}

// buildSummaryPrompt 构建摘要请求的用户提示词。
//
// 如果有之前的摘要，指令 LLM 更新它；否则创建新摘要。
// 使用结构化 Markdown 模板确保摘要质量。
func (c *Compactor) buildSummaryPrompt(oldMessages []Message) string {
	var sb strings.Builder

	// 将旧消息格式化为文本
	sb.WriteString("Conversation to summarize:\n\n")
	for _, msg := range oldMessages {
		role := string(msg.Role)
		text := TextFromParts(msg.Content)
		if text == "" {
			continue
		}
		fmt.Fprintf(&sb, "[%s]: %s\n\n", role, text)
	}

	sb.WriteString("\n---\n\n")

	// 检查是否有之前的摘要（增量更新）
	c.mu.Lock()
	prevSummary := c.previousSummary
	c.mu.Unlock()

	if prevSummary != "" {
		sb.WriteString("Update the anchored summary below using the conversation history above.\n")
		sb.WriteString("Preserve still-true details, remove stale details, and merge in the new facts.\n")
		sb.WriteString("<previous-summary>\n")
		sb.WriteString(prevSummary)
		sb.WriteString("\n</previous-summary>\n\n")
	} else {
		sb.WriteString("Create a new anchored summary from the conversation history.\n\n")
	}

	sb.WriteString(SummaryTemplate)
	return sb.String()
}

// ============================================================================
// Context Overflow 错误检测
// ============================================================================

// contextOverflowPatterns 匹配各 provider 的 context overflow 错误消息。
var contextOverflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)prompt is too long`),
	regexp.MustCompile(`(?i)input is too long for requested model`),
	regexp.MustCompile(`(?i)exceeds the context window`),
	regexp.MustCompile(`(?i)input token count.*exceeds the maximum`),
	regexp.MustCompile(`(?i)maximum prompt length is \d+`),
	regexp.MustCompile(`(?i)reduce the length of the messages`),
	regexp.MustCompile(`(?i)maximum context length is \d+ tokens`),
	regexp.MustCompile(`(?i)exceeds the limit of \d+`),
	regexp.MustCompile(`(?i)exceeds the available context size`),
	regexp.MustCompile(`(?i)greater than the context length`),
	regexp.MustCompile(`(?i)context window exceeds limit`),
	regexp.MustCompile(`(?i)exceeded model token limit`),
	regexp.MustCompile(`(?i)context[_ ]length[_ ]exceeded`),
	regexp.MustCompile(`(?i)request entity too large`),
	regexp.MustCompile(`(?i)context length is only \d+ tokens`),
	regexp.MustCompile(`(?i)input length.*exceeds.*context length`),
	regexp.MustCompile(`(?i)prompt too long; exceeded (?:max )?context length`),
	regexp.MustCompile(`(?i)too large for model with \d+ maximum context length`),
	regexp.MustCompile(`(?i)model_context_window_exceeded`),
}

// statusOverflowPattern 匹配 400/413 状态码（无 body 时）。
var statusOverflowPattern = regexp.MustCompile(`(?i)^4(00|13)\s*(status code)?\s*\(no body\)`)

// IsContextOverflow 检查错误消息是否表示 context overflow。
func IsContextOverflow(message string) bool {
	if message == "" {
		return false
	}
	for _, p := range contextOverflowPatterns {
		if p.MatchString(message) {
			return true
		}
	}
	return statusOverflowPattern.MatchString(message)
}

// IsContextOverflowError 检查 error 是否为 context overflow。
// 支持 *LLMError（通过 Reason 或 Message）和原始 error。
func IsContextOverflowError(err error) bool {
	if err == nil {
		return false
	}

	// 检查是否为 *LLMError
	if llmErr, ok := AsLLMError(err); ok {
		if llmErr.Reason == ErrorReasonContextOverflow {
			return true
		}
		return IsContextOverflow(llmErr.Message)
	}

	// 检查原始错误消息
	return IsContextOverflow(err.Error())
}

// ============================================================================
// Mid-Conversation System Message（中间对话系统消息）
// ============================================================================

// MidConversationMessage 是在对话中间插入的系统消息。
type MidConversationMessage struct {
	// Type 消息类型标识（如 "date_change"、"context_update"）。
	Type string
	// Content 消息内容。
	Content string
	// Timestamp 插入时间。
	Timestamp time.Time
}

// NewDateChangeMessage 创建日期变更提醒消息。
func NewDateChangeMessage(currentDate string) MidConversationMessage {
	return MidConversationMessage{
		Type:      "date_change",
		Content:   fmt.Sprintf("[System Note] The current date is now %s. Please use this as reference for any date-related queries.", currentDate),
		Timestamp: time.Now(),
	}
}

// ToMessage 将 MidConversationMessage 转换为 LLM Message。
func (m MidConversationMessage) ToMessage() Message {
	return SystemMessage(m.Content)
}

// InsertMidConversationMessages 在消息列表末尾插入中间对话系统消息。
// 用于在每轮对话前注入日期变更、上下文更新等信息。
func InsertMidConversationMessages(messages []Message, midMsgs ...MidConversationMessage) []Message {
	if len(midMsgs) == 0 {
		return messages
	}
	result := make([]Message, 0, len(messages)+len(midMsgs))
	result = append(result, messages...)
	for _, m := range midMsgs {
		result = append(result, m.ToMessage())
	}
	return result
}

// ============================================================================
// PrepareStep hook：自动上下文压缩
// ============================================================================

// CompactionPrepareStep 返回一个 PrepareStep hook 函数。
// 在 OrchestrateStream/OrchestrateGenerate 的每一步之前自动执行上下文压缩。
//
// 使用方式：
//
//	cfg := &llm.OrchestrateConfig{
//	    Params: params,
//	    MaxSteps: 10,
//	    PrepareStep: llm.CompactionPrepareStep(compactor),
//	}
//	result, err := llm.OrchestrateStream(ctx, provider, cfg)
func CompactionPrepareStep(compactor *Compactor) func(*GenerateParams) *GenerateParams {
	return func(params *GenerateParams) *GenerateParams {
		if compactor == nil {
			return nil
		}
		if !compactor.ShouldCompact(*params) {
			return nil
		}

		// 无 LLM provider 的 prune-only 模式
		pruned := compactor.PruneToolOutputs(params.Messages)
		newParams := *params
		newParams.Messages = pruned
		return &newParams
	}
}

// CompactionPrepareStepWithProvider 返回一个带 LLM 摘要能力的 PrepareStep hook。
func CompactionPrepareStepWithProvider(compactor *Compactor, provider Provider) func(context.Context) func(*GenerateParams) *GenerateParams {
	return func(ctx context.Context) func(*GenerateParams) *GenerateParams {
		return func(params *GenerateParams) *GenerateParams {
			if compactor == nil || !compactor.ShouldCompact(*params) {
				return nil
			}

			newParams, err := compactor.Compact(ctx, *params, provider)
			if err != nil {
				// 压缩失败，降级为 prune-only
				newParams = *params
				newParams.Messages = compactor.PruneToolOutputs(params.Messages)
			}
			return &newParams
		}
	}
}
