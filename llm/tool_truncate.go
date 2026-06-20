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

	// 执行截断 — 从头部保留
	direction := "head"
	maxLines := cfg.MaxLines
	maxBytes := cfg.MaxBytes

	var kept []string
	keptBytes := 0
	hitBytes := false

	for i := 0; i < len(lines) && i < maxLines; i++ {
		lineSize := len(lines[i])
		if i > 0 {
			lineSize++ // newline
		}
		if keptBytes+lineSize > maxBytes {
			hitBytes = true
			break
		}
		kept = append(kept, lines[i])
		keptBytes += lineSize
	}

	removed := len(lines) - len(kept)
	unit := "lines"
	if hitBytes {
		removed = totalBytes - keptBytes
		unit = "bytes"
	}

	preview := strings.Join(kept, "\n")

	hint := fmt.Sprintf(
		"\n\n... [%d %s truncated] ...\n\n"+
			"⚠️ Output was truncated. Use more specific parameters (e.g., narrower search pattern, "+
			"offset/limit for file reads, or grep for specific content) to get focused results. "+
			"Do not retry with the same parameters expecting full output.",
		removed, unit,
	)

	_ = direction // head is the only supported direction currently

	return TruncationResult{
		Output:       preview + hint,
		Truncated:    true,
		OriginalSize: totalBytes,
	}
}
