package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ============================================================================
// Tool Output Truncation
//
// 当工具输出超过阈值时，截断为预览 + 截断提示，
// 帮助 LLM 知道输出被裁剪了。
//
// 设计原则：
//   - 透明：工具执行逻辑不受影响，截断发生在结果返回给 LLM 之前
//   - 可配置：支持全局配置 MaxLines / MaxBytes
//   - 智能提示：截断时告知 LLM 输出被裁剪，建议用更精确的参数重新调用
// ============================================================================

// TruncationConfig 控制工具输出的截断行为。
type TruncationConfig struct {
	// MaxLines 工具输出结果的最大行数。超过此值时截断。
	// 默认 500 行。
	MaxLines int

	// MaxBytes 工具输出结果的最大字节数。超过此值时截断。
	// 默认 50 * 1024 (50 KB)。
	MaxBytes int
}

// DefaultTruncationConfig 返回合理的默认截断配置。
func DefaultTruncationConfig() TruncationConfig {
	return TruncationConfig{
		MaxLines: 500,
		MaxBytes: 50 * 1024,
	}
}

// TruncationResult 是截断后的结果。
type TruncationResult struct {
	// Output 截断后的输出（可能是原始值或截断后的字符串）。
	Output any

	// Truncated 是否发生了截断。
	Truncated bool

	// OriginalSize 原始输出的字节数（估算）。
	OriginalSize int
}

// TruncateOutput 对工具输出应用截断。
//
// 如果输出是字符串，直接截断；如果是其他类型，先 JSON 序列化再截断。
// 当输出在阈值内时原样返回。
func TruncateOutput(output any, cfg TruncationConfig) TruncationResult {
	if cfg.MaxLines <= 0 {
		cfg.MaxLines = DefaultTruncationConfig().MaxLines
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultTruncationConfig().MaxBytes
	}

	// 将输出转为文本表示
	text, ok := output.(string)
	if !ok {
		// 非 string 类型：尝试 JSON 序列化
		data, err := json.Marshal(output)
		if err != nil {
			// 序列化失败，转为 fmt 字符串
			text = fmt.Sprintf("%v", output)
		} else {
			text = string(data)
		}
	}

	totalBytes := len(text)
	lines := strings.Split(text, "\n")

	// 在阈值内，不需要截断
	if len(lines) <= cfg.MaxLines && totalBytes <= cfg.MaxBytes {
		return TruncationResult{
			Output:       output,
			Truncated:    false,
			OriginalSize: totalBytes,
		}
	}

	// 执行截断 — 头部 + 尾部保留，中间省略。
	// 相比纯头部截断，保留尾部能让 LLM 看到输出的结尾（如错误信息、
	// 汇总行、退出码通常出现在末尾），避免因丢尾部而漏掉关键信息。
	//
	// 预算分配：头部占约 2/3，尾部占约 1/3（头部通常包含更多上下文）。
	maxLines := cfg.MaxLines
	maxBytes := cfg.MaxBytes

	headLineBudget := maxLines * 2 / 3
	tailLineBudget := maxLines - headLineBudget
	headByteBudget := maxBytes * 2 / 3
	tailByteBudget := maxBytes - headByteBudget

	// 头部：从前往后累积。每个元素含其后的 newline（与尾部口径一致），
	// 这样 headBytes 与 tailBytes 的字节预算分配对称，removedBytes 估算也更准。
	var head []string
	headBytes := 0
	for i := 0; i < len(lines) && len(head) < headLineBudget; i++ {
		lineSize := len(lines[i]) + 1 // 含 newline
		if headBytes+lineSize > headByteBudget {
			break
		}
		head = append(head, lines[i])
		headBytes += lineSize
	}

	// 尾部：从后往前累积（避免与头部重叠）。
	var tail []string
	tailBytes := 0
	for i := len(lines) - 1; i >= len(head) && len(tail) < tailLineBudget; i-- {
		lineSize := len(lines[i]) + 1 // 含 newline
		if tailBytes+lineSize > tailByteBudget {
			break
		}
		tail = append(tail, lines[i])
		tailBytes += lineSize
	}
	// tail 是逆序收集的，反转回正序。
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}

	// 字节级兜底：当输入是「无换行的超长单行」（压缩 JSON / 单行 HTML / 长 base64 等），
	// head 和 tail 都会因首行即超字节预算而落空，纯按行截断会整块丢弃内容。
	// 此时退回字节切片：保留前 headByteBudget 与后 tailByteBudget 字节，中间省略。
	if len(head) == 0 && len(tail) == 0 && totalBytes > 0 {
		headBytes = headByteBudget
		if headBytes > totalBytes {
			headBytes = totalBytes
		}
		tailBytes = tailByteBudget
		if tailBytes > totalBytes-headBytes {
			tailBytes = totalBytes - headBytes
		}
		headText := text[:headBytes]
		tailText := ""
		if tailBytes > 0 {
			tailText = text[totalBytes-tailBytes:]
		}
		hint := truncationHint(totalBytes-headBytes-tailBytes, -1)
		return TruncationResult{
			Output:       headText + hint + "\n\n" + tailText,
			Truncated:    true,
			OriginalSize: totalBytes,
		}
	}

	removedLines := len(lines) - len(head) - len(tail)
	removedBytes := totalBytes - headBytes - tailBytes

	headText := strings.Join(head, "\n")
	tailText := strings.Join(tail, "\n")

	hint := truncationHint(removedBytes, removedLines)

	return TruncationResult{
		Output:       headText + hint + "\n\n" + tailText,
		Truncated:    true,
		OriginalSize: totalBytes,
	}
}

// truncationHint 生成统一的截断提示。
// removedBytes 为必填（估算省略的字节数）；removedLines 为 -1 表示未知行数
// （字节级兜底场景，因不按行截断）。提示面向 LLM，告知输出被裁剪并建议更精确的参数。
func truncationHint(removedBytes, removedLines int) string {
	marker := "... [middle omitted"
	if removedLines >= 0 {
		marker += fmt.Sprintf(": %d lines / ~%d bytes", removedLines, removedBytes)
	} else {
		marker += fmt.Sprintf(": ~%d bytes", removedBytes)
	}
	marker += "; head and tail preserved] ..."
	return "\n\n" + marker + "\n\n" +
		"IMPORTANT: This output was truncated — the head and tail are shown, the middle was omitted. " +
		"To reach the omitted content you MUST narrow the request: use a smaller search scope, " +
		"read in pages with offset/limit, or grep for a specific keyword. " +
		"NEVER retry the same call with identical parameters expecting the full output."
}
