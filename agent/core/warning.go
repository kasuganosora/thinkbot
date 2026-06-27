package core

import (
	"fmt"
	"strings"
)

// ============================================================================
// 延迟警告注入模式 (Deferred Warning Injection)
//
// 借鉴 deer-flow 的设计：中间件在 after_model 阶段检测出问题后，
// 将警告暂存到 Envelope KV，而不是立即修改消息列表。
// 下一个 Stage（通常是 LLMStage）在构建 system prompt 时读取并合并警告。
//
// 这种延迟注入确保：
//  1. 警告不会破坏 AIMessage(tool_calls) → ToolMessage 的配对
//  2. 多个中间件的警告可以合并，避免互相覆盖
//  3. 警告注入与具体 Stage 解耦
//
// 使用方式：
//
//	// 中间件中：
//	core.QueueWarning(env, core.Warning{Source: "token_budget", ...})
//
//	// LLMStage 在读取 system prompt 时：
//	prompt = core.MergeWarnings(env, prompt)
// ============================================================================

// Warning 表示一条待注入的警告消息。
type Warning struct {
	// Source 警告来源（如 "token_budget", "loop_detection"）。
	Source string `json:"source"`
	// Level 严重级别（"soft" / "hard"）。
	Level string `json:"level"`
	// Message 警告内容。
	Message string `json:"message"`
}

const (
	// WarningsKey 是 Envelope KV 中存储警告的键名。
	WarningsKey = "pipeline.warnings"
	// WarningLevelSoft 软警告：提示 LLM 注意但不强制停止。
	WarningLevelSoft = "soft"
	// WarningLevelHard 硬警告：强制 LLM 停止工具调用并产出最终答案。
	WarningLevelHard = "hard"
)

// QueueWarning 向 Envelope 追加一条延迟警告。
// 警告不会立即生效，在下游 Stage 读取时才会被注入。
func QueueWarning(env *Envelope, w Warning) {
	existing, _ := env.Get(WarningsKey)
	var warnings []Warning
	if existing != nil {
		warnings, _ = existing.([]Warning)
	}
	warnings = append(warnings, w)
	env.Set(WarningsKey, warnings)
}

// MergeWarnings 将队列中的警告合并到 system prompt 中。
// 返回合并后的完整 prompt，未消费的硬警告保留在 envelope 中。
//
// 约定：
//   - soft 警告：合并到 prompt 后从队列移除（已传递）
//   - hard 警告：合并到 prompt 但保留在队列（可能需要在后续轮次再次注入）
func MergeWarnings(env *Envelope, basePrompt string) string {
	v, ok := env.Get(WarningsKey)
	if !ok {
		return basePrompt
	}
	warnings, ok := v.([]Warning)
	if !ok || len(warnings) == 0 {
		return basePrompt
	}

	var sb strings.Builder
	sb.WriteString(basePrompt)

	remaining := make([]Warning, 0)
	hasContent := false

	for _, w := range warnings {
		prefix := ""
		switch w.Level {
		case WarningLevelHard:
			prefix = "[SYSTEM WARNING - URGENT] "
			remaining = append(remaining, w) // 硬警告保留
		default:
			prefix = "[SYSTEM WARNING] "
			// 软警告消费后不保留
		}
		fmt.Fprintf(&sb, "\n\n%s[%s] %s", prefix, w.Source, w.Message)
		hasContent = true
	}

	if hasContent {
		sb.WriteString("\n")
	}

	if len(remaining) > 0 {
		env.Set(WarningsKey, remaining)
	} else {
		env.Set(WarningsKey, nil)
	}

	return sb.String()
}

// HasHardWarning 检查是否存在硬警告。
func HasHardWarning(env *Envelope) bool {
	v, ok := env.Get(WarningsKey)
	if !ok {
		return false
	}
	warnings, ok := v.([]Warning)
	if !ok {
		return false
	}
	for _, w := range warnings {
		if w.Level == WarningLevelHard {
			return true
		}
	}
	return false
}

// ClearWarnings 清空所有延迟警告。
func ClearWarnings(env *Envelope) {
	env.Set(WarningsKey, nil)
}
