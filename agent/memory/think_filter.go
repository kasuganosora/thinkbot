package memory

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// ============================================================================
// Think 标签过滤器 — 在记忆写入前清理 LLM 深度思考内容
//
// 某些 LLM（如 DeepSeek-R1、GLM、QwQ 等）会将推理过程以 <think>...</think>
// 或 <thinking>...</thinking> 标签嵌入到回复文本中。这些推理内容对人类用户
// 没有直接价值，存储到记忆中会浪费存储空间和检索时的 token 预算。
//
// 本模块在写入记忆前移除这些标签及其内容，仅保留最终回复文本。
// 实现参考了 Memoh 项目的 FilterThinkingTags / FilterReasoningArray。
// ============================================================================

// thinkTagRe 匹配 <think>...</think> 和 <thinking>...</thinking> 块。
// 标志说明：
//   - i: 不区分大小写（某些模型输出 <Think> 或 <THINKING>）
//   - s: 使 . 匹配换行符（思考内容通常是多行的）
var thinkTagRe = regexp.MustCompile(`(?is)<think(?:ing)?>\s*.*?\s*</think(?:ing)?>`)

// unclosedThinkRe 匹配只有开标签没有闭标签的 <think>/<thinking>（流式截断场景）。
var unclosedThinkRe = regexp.MustCompile(`(?is)<think(?:ing)?>.*$`)

// StripThinkTags 从文本中移除 <think>...</think> 和 <thinking>...</thinking> 块。
//
// 处理逻辑：
//  1. 移除完整的 think/thinking 标签对及其内容
//  2. 移除未闭合的 think/thinking 开标签（截断的流式输出）
//  3. 清理多余空白，返回 TrimSpace 后的结果
//
// 如果移除后内容为空（即文本只包含思考内容），返回空字符串。
func StripThinkTags(text string) string {
	cleaned := thinkTagRe.ReplaceAllString(text, "")
	cleaned = unclosedThinkRe.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

// reasoningPart 对应某些 API（如智谱 GLM）在 content 字段中发出的 JSON 推理数组。
type reasoningPart struct {
	Text string `json:"text"`
	Type string `json:"type"`
}

// StripReasoningArray 检测并剥离原始 JSON 推理数组。
//
// 某些 API（如智谱 GLM）在上下文溢出或特殊模式下，会在 content 字段中
// 发出形如 [{"text":"...","type":"reasoning"},{"text":"...","type":"text"}]
// 的 JSON 数组，而非普通文本。
//
// 本函数提取其中 type="text" 的部分并用换行连接；
// 如果输入不是推理数组格式，则原样返回。
func StripReasoningArray(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "[{") || !strings.HasSuffix(trimmed, "}]") {
		return text
	}

	var parts []reasoningPart
	if err := json.Unmarshal([]byte(trimmed), &parts); err != nil {
		return text
	}
	if len(parts) == 0 {
		return text
	}

	hasReasoning := false
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "reasoning":
			hasReasoning = true
		default:
			// 未知类型，不是推理数组
			return text
		}
	}

	if !hasReasoning {
		return text
	}

	return strings.Join(texts, "\n")
}

// StripThinking 对文本执行完整的思考内容清理。
// 依次应用 StripReasoningArray → StripThinkTags。
// 这是记忆写入前应调用的主入口。
func StripThinking(text string) string {
	text = StripReasoningArray(text)
	return StripThinkTags(text)
}

// ============================================================================
// ThinkFilterStore — 自动清理思考内容的 Store 装饰器
// ============================================================================

// ThinkFilterStore 包装一个底层 Store，在 Append 前自动对 Entry.Content
// 执行 StripThinking 清理。
//
// 使用方式：
//
//	repo := memory.NewMemoryRepository()
//	filtered := memory.NewThinkFilterStore(repo)
//	// filtered 满足 Store 接口，后续所有 Append 都会自动清理 think 标签
type ThinkFilterStore struct {
	inner Store
}

// NewThinkFilterStore 创建思考内容过滤 Store 装饰器。
func NewThinkFilterStore(inner Store) *ThinkFilterStore {
	return &ThinkFilterStore{inner: inner}
}

// Append 在写入前清理 Entry.Content 中的思考内容。
func (s *ThinkFilterStore) Append(ctx context.Context, entry Entry) error {
	entry.Content = StripThinking(entry.Content)
	return s.inner.Append(ctx, entry)
}

// Delete 透传到底层 Store。
func (s *ThinkFilterStore) Delete(ctx context.Context, scope Scope, entryID string) error {
	return s.inner.Delete(ctx, scope, entryID)
}

// Clear 透传到底层 Store。
func (s *ThinkFilterStore) Clear(ctx context.Context, scope Scope) error {
	return s.inner.Clear(ctx, scope)
}
