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
//
// 落盘指针（offload，借鉴 opencode 的 token 优化）：
//   - 当输出被截断且提供了 OffloadSink 时，会把【完整原文】写入 bot 的工作空间，
//     仅在返回给 LLM 的预览里附上「工作空间相对路径 + 指向 spawn 的委托提示」。
//   - 这样深入挖掘（grep 上万命中、读超长文件）的代价被隔离：主上下文只留预览+指针，
//     真正需要中间内容时，由 spawn 派生的子 agent 带着 grep/read(offset/limit)
//     去读取该文件——子 agent 在独立上下文工作，完整 dump 不会污染主线。
//   - 落盘失败不影响工具结果：退化成原有的 head+tail 截断（fail-safe）。
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

// ToolOutputConfig 工具输出截断 + 落盘指针的可配置项。
// 阈值部分由编排层（OrchestrateConfig.ToolOutput）携带；落盘开关与子目录在 Bot
// 装配时消费——关闭落盘时直接不注入 sink，行为等价于纯 head+tail 截断。
type ToolOutputConfig struct {
	// MaxLines 截断行数阈值（默认 500）。
	MaxLines int
	// MaxBytes 截断字节阈值（默认 50KB）。
	MaxBytes int
	// OffloadEnabled 是否启用落盘指针（默认 true）。
	OffloadEnabled bool
	// OffloadSubdir 落盘子目录（相对工作空间根，默认 "tool-output"）。
	OffloadSubdir string
}

// DefaultToolOutputConfig 返回合理的默认配置。
func DefaultToolOutputConfig() ToolOutputConfig {
	return ToolOutputConfig{
		MaxLines:       500,
		MaxBytes:       50 * 1024,
		OffloadEnabled: true,
		OffloadSubdir:  "tool-output",
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

	// OffloadPath 当启用落盘指针时，完整原文写入工作空间后的相对路径；
	// 空字符串表示未落盘（输出在阈值内、或 sink 未提供/写入失败）。
	OffloadPath string
}

// ToolOutputOffloadSink 把完整工具输出落盘到 bot 工作空间，返回供模型/子 agent
// 读取的「工作空间相对路径」（如 "tool-output/abc123.txt"）。
// botID 用于定位工作空间；toolCallID 用于生成唯一文件名。
// 返回错误时调用方应 fail-safe 退化成原有 head+tail 截断。
type ToolOutputOffloadSink func(botID, toolCallID string, content []byte) (savedRelPath string, err error)

// truncateOption 是 TruncateOutput 的可选行为修饰器。
type truncateOption struct {
	offloadBotID     string
	offloadToolCall  string
	offloadSink      ToolOutputOffloadSink
}

// TruncateOption 修饰 TruncateOutput 的行为。
type TruncateOption func(*truncateOption)

// WithOffload 启用落盘指针：当输出被截断时，把完整原文通过 sink 写入 bot 工作空间。
func WithOffload(botID, toolCallID string, sink ToolOutputOffloadSink) TruncateOption {
	return func(o *truncateOption) {
		o.offloadBotID = botID
		o.offloadToolCall = toolCallID
		o.offloadSink = sink
	}
}

// TruncateOutput 对工具输出应用截断。
//
// 如果输出是字符串，直接截断；如果是其他类型，先 JSON 序列化再截断。
// 当输出在阈值内时原样返回。
//
// opts 可携带 WithOffload 以启用落盘指针（见包文档）。
func TruncateOutput(output any, cfg TruncationConfig, opts ...TruncateOption) TruncationResult {
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

	// 完整原文（落盘用），在截断前捕获，确保写入的是未裁剪内容。
	fullText := text

	totalBytes := len(text)
	lines := strings.Split(text, "\n")

	result := TruncationResult{
		OriginalSize: totalBytes,
	}

	// 在阈值内，不需要截断
	if len(lines) <= cfg.MaxLines && totalBytes <= cfg.MaxBytes {
		result.Output = output
		return result
	}

	result.Truncated = true

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
		headText := fullText[:headBytes]
		tailText := ""
		if tailBytes > 0 {
			tailText = fullText[totalBytes-tailBytes:]
		}
		hint := truncationHint(totalBytes-headBytes-tailBytes, -1)
		preview := headText + hint + "\n\n" + tailText
		result.Output = applyOffload(fullText, preview, opts, &result)
		return result
	}

	removedLines := len(lines) - len(head) - len(tail)
	removedBytes := totalBytes - headBytes - tailBytes

	headText := strings.Join(head, "\n")
	tailText := strings.Join(tail, "\n")

	hint := truncationHint(removedBytes, removedLines)

	preview := headText + hint + "\n\n" + tailText
	result.Output = applyOffload(fullText, preview, opts, &result)
	return result
}

// applyOffload 在输出被截断的前提下，尝试把完整原文落盘到工作空间。
// 成功则将指针 + 子 agent 委托提示追加到 preview；任何失败都 fail-safe 返回原 preview。
func applyOffload(fullText, preview string, opts []TruncateOption, result *TruncationResult) string {
	o := &truncateOption{}
	for _, opt := range opts {
		opt(o)
	}
	if o.offloadSink == nil || o.offloadBotID == "" || o.offloadToolCall == "" {
		return preview
	}
	relPath, err := o.offloadSink(o.offloadBotID, o.offloadToolCall, []byte(fullText))
	if err != nil || relPath == "" {
		return preview
	}
	result.OffloadPath = relPath
	return preview + "\n\n" + offloadPointer(relPath, len(fullText))
}

// offloadPointer 生成落盘指针 + 子 agent 委托提示（面向 LLM，英文，与既有 hint 一致）。
func offloadPointer(relPath string, fullBytes int) string {
	return fmt.Sprintf(
		"[Full output (%d bytes) saved to workspace file: %s. "+
			"To inspect the omitted middle WITHOUT bloating this context, delegate to a sub-agent via the spawn tool "+
			"and have it grep/read this file with offset/limit. "+
			"You may also read it directly with the read tool using offset/limit.]",
		fullBytes, relPath,
	)
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
