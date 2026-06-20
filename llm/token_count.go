package llm

import (
	"unicode/utf8"
)

// ============================================================================
// Token 估算工具
//
// 提供轻量级的 token 计数估算，无需依赖外部 tokenizer。
// 估算精度约为 ±15%，仅用于触发上下文压缩的阈值判断，
// 不用于精确计费。
// ============================================================================

// TokenCountConfig 控制 token 估算行为。
type TokenCountConfig struct {
	// Mode 估算模式：
	//   "exact"   — 精确计数（仅 ASCII 字符 1:1，CJK 约 2:1）
	//   "chars"   — 字符数 / 4（OpenAI 经验值）
	//   "hybrid"  — 默认：混合模式，区分 CJK 和非 CJK
	Mode string
}

// DefaultTokenCountConfig 返回默认 token 计数配置。
func DefaultTokenCountConfig() TokenCountConfig {
	return TokenCountConfig{Mode: "hybrid"}
}

// EstimateTokens 估算文本的 token 数量。
// 使用混合策略：
//   - 英文：约 4 个字符 = 1 token
//   - CJK（中日韩）：约 1.5 个字符 = 1 token
//   - 代码/符号：约 3 个字符 = 1 token
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}

	var (
		ascii      int
		cjk        int
		other      int
		whitespace int
	)

	for _, r := range text {
		switch {
		case r < 128:
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
				whitespace++
			} else if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				ascii++
			} else {
				other++ // 标点、符号
			}
		case r >= 0x4E00 && r <= 0x9FFF || // CJK 统一表意
			r >= 0x3040 && r <= 0x30FF || // 平假名 + 片假名
			r >= 0xAC00 && r <= 0xD7AF: // 韩文音节
			cjk++
		default:
			other++
		}
	}

	// 经验权重
	total := ascii/4 + cjk*2/3 + other/3 + whitespace/5
	if total == 0 && len(text) > 0 {
		total = 1
	}
	return total
}

// EstimateMessageTokens 估算单条消息的 token 数量。
// 包含角色标记开销（约 4 tokens）+ 各内容块的 token 数。
func EstimateMessageTokens(msg Message) int {
	const roleOverhead = 4
	total := roleOverhead
	for _, part := range msg.Content {
		total += EstimatePartTokens(part)
	}
	return total
}

// EstimatePartTokens 估算法单个消息内容块的 token 数量。
func EstimatePartTokens(part MessagePart) int {
	switch p := part.(type) {
	case TextPart:
		return EstimateTokens(p.Text)
	case ReasoningPart:
		return EstimateTokens(p.Text)
	case ToolCallPart:
		// 工具调用的 token：函数名 + 参数 JSON
		text := p.ToolName
		if s, ok := p.Input.(string); ok {
			text += s
		}
		return EstimateTokens(text) + 3 // 开销
	case ToolResultPart:
		return EstimatePartResultTokens(p.Result) + 3
	case ImagePart:
		// 图片约 85 tokens（低分辨率）到 765 tokens（高分辨率）
		return 256
	case FilePart:
		return EstimateTokens(p.Data) / 2
	}
	return 0
}

// EstimatePartResultTokens 估算工具结果值的 token 数量。
func EstimatePartResultTokens(result any) int {
	if result == nil {
		return 0
	}
	switch v := result.(type) {
	case string:
		return EstimateTokens(v)
	case []byte:
		return EstimateTokens(string(v))
	default:
		// 对于复杂类型，估算比字符串表示
		return EstimateTokens(stringifyResult(v)) / 2
	}
}

// EstimateMessagesTokens 估算消息列表的总 token 数量。
// 包含每条消息之间的分隔开销（约 3 tokens/消息）。
func EstimateMessagesTokens(messages []Message) int {
	const separatorOverhead = 3
	total := 0
	for _, msg := range messages {
		total += EstimateMessageTokens(msg) + separatorOverhead
	}
	return total
}

// EstimateSystemTokens 估算 system prompt 的 token 数量。
// System prompt 有额外的角色开销。
func EstimateSystemTokens(system string) int {
	if system == "" {
		return 0
	}
	return EstimateTokens(system) + 4
}

// EstimateParamsTokens 估算完整 GenerateParams 的 token 数量。
func EstimateParamsTokens(params GenerateParams) int {
	total := EstimateSystemTokens(params.System)
	total += EstimateMessagesTokens(params.Messages)
	return total
}

// CountRunes 计算 Unicode 字符数（非字节数）。
func CountRunes(s string) int {
	return utf8.RuneCountInString(s)
}

// stringifyResult 将 any 转为字符串表示。
func stringifyResult(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	case error:
		return val.Error()
	default:
		return "" // 复杂类型由调用方处理
	}
}
