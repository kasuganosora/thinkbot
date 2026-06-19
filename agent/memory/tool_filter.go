package memory

import (
	"context"
	"regexp"
	"strings"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 工具输出过滤器 — 在记忆写入前 / 上下文构建前清理工具调用结果
//
// Agent 在多轮对话中使用 function calling 时，会产生大量工具调用结果
// （搜索结果、文件内容、API 返回 JSON 等）。这些内容对后续检索和记忆巩固
// 几乎没有信息价值，但会消耗大量 token 预算。
//
// 与 StripThinking（移除 <think> 标签）类似，本模块在两个层面提供过滤：
//   1. StripToolMessages — 从 []llm.Message 中移除 tool 角色消息和工具相关 part
//   2. StripToolOutputFromText — 从文本中移除序列化的工具输出标签
//   3. ToolOutputFilterStore — Store 装饰器，写入前自动过滤 Entry.Content
// ============================================================================

// --- 消息级过滤 ---

// StripToolMessages 从消息序列中移除工具相关的消息和内容块。
//
// 处理逻辑：
//  1. 移除 Role=tool 的整条消息（工具结果消息）
//  2. 从剩余消息的 Content 中移除 ToolResultPart 和 ToolCallPart
//  3. 跳过过滤后 Content 为空的消息（仅含工具内容的消息被丢弃）
//
// 适用于：在将对话历史发送给 LLM 做记忆巩固 / 摘要前，剔除冗长的工具输出，
// 大幅减少 token 消耗。
func StripToolMessages(messages []llm.Message) []llm.Message {
	result := make([]llm.Message, 0, len(messages))

	for _, msg := range messages {
		// 跳过 tool 角色消息（工具结果）
		if msg.Role == llm.MessageRoleTool {
			continue
		}

		// 过滤 Content 中的工具相关 part
		msg.Content = stripToolParts(msg.Content)

		// 跳过过滤后为空的消息
		if len(msg.Content) == 0 {
			continue
		}

		result = append(result, msg)
	}

	return result
}

// stripToolParts 从消息内容块列表中移除 ToolResultPart 和 ToolCallPart。
func stripToolParts(parts []llm.MessagePart) []llm.MessagePart {
	filtered := parts[:0:0]
	for _, part := range parts {
		switch part.(type) {
		case llm.ToolResultPart, llm.ToolCallPart:
			// 跳过工具结果和工具调用
			continue
		default:
			filtered = append(filtered, part)
		}
	}
	return filtered
}

// --- 文本级过滤 ---

// toolCallTagRe 匹配 <tool_call>...</tool_call> 和 <function_call>...</function_call> 块。
// 某些 LLM 供应商（如 Anthropic）使用这些标签序列化工具调用。
var toolCallTagRe = regexp.MustCompile(`(?is)<(?:tool_call|function_call)>\s*.*?\s*</(?:tool_call|function_call)>`)

// toolResultTagRe 匹配 <tool_result>...</tool_result> 块。
var toolResultTagRe = regexp.MustCompile(`(?is)<tool_result[^>]*>\s*.*?\s*</tool_result>`)

// unclosedToolTagRe 匹配只有开标签没有闭标签的工具块（流式截断场景）。
var unclosedToolTagRe = regexp.MustCompile(`(?is)<(?:tool_call|function_call|tool_result)[^>]*>.*$`)

// StripToolOutputFromText 从文本中移除工具调用 / 工具结果标签及其内容。
//
// 处理逻辑：
//  1. 移除 <tool_call>...</tool_call> / <function_call>...</function_call>
//  2. 移除 <tool_result>...</tool_result>
//  3. 移除未闭合的工具标签（截断的流式输出）
//  4. 清理多余空白
//
// 如果移除后内容为空，返回空字符串。
func StripToolOutputFromText(text string) string {
	cleaned := toolResultTagRe.ReplaceAllString(text, "")
	cleaned = toolCallTagRe.ReplaceAllString(cleaned, "")
	cleaned = unclosedToolTagRe.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

// --- 组合入口 ---

// StripToolOutput 对文本执行完整的工具输出清理。
// 依次应用 StripReasoningArray → StripToolOutputFromText。
// 这是记忆写入前应调用的主入口（与 StripThinking 配合使用）。
func StripToolOutput(text string) string {
	text = StripReasoningArray(text)
	return StripToolOutputFromText(text)
}

// ============================================================================
// ToolOutputFilterStore — 自动清理工具输出的 Store 装饰器
// ============================================================================

// ToolOutputFilterStore 包装一个底层 Store，在 Append 前自动对 Entry.Content
// 执行 StripToolOutput 清理。
//
// 使用方式：
//
//	repo := memory.NewMemoryRepository()
//	filtered := memory.NewToolOutputFilterStore(repo)
//	// 后续所有 Append 都会自动清理工具输出
//
// 与 ThinkFilterStore 可以组合使用：
//
//	store := memory.NewThinkFilterStore(memory.NewToolOutputFilterStore(repo))
type ToolOutputFilterStore struct {
	inner Store
}

// NewToolOutputFilterStore 创建工具输出过滤 Store 装饰器。
func NewToolOutputFilterStore(inner Store) *ToolOutputFilterStore {
	return &ToolOutputFilterStore{inner: inner}
}

// Append 在写入前清理 Entry.Content 中的工具输出。
func (s *ToolOutputFilterStore) Append(ctx context.Context, entry Entry) error {
	entry.Content = StripToolOutput(entry.Content)
	return s.inner.Append(ctx, entry)
}

// Delete 透传到底层 Store。
func (s *ToolOutputFilterStore) Delete(ctx context.Context, scope Scope, entryID string) error {
	return s.inner.Delete(ctx, scope, entryID)
}

// Clear 透传到底层 Store。
func (s *ToolOutputFilterStore) Clear(ctx context.Context, scope Scope) error {
	return s.inner.Clear(ctx, scope)
}
