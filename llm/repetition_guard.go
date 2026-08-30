package llm

// ============================================================================
// Repetition Guard — 检测 LLM 输出的重复退化（repetition collapse）
// ============================================================================
//
// 典型症状：模型输出前半段正常文本后，突然陷入短模式无限循环，例如：
//
//	"……双方都移除了\u201c联合公众号\u201d标签在无人注意到\n")"
//	"NN BB NN BB NN BB NN BB NN BB NN ..."    ← 从这里开始发疯
//
// 本模块提供增量（流式）和一次性（非流式）两种检测接口。检测到退化后截断至
// 最后一个「正常」位置，防止垃圾文本进入回复 / 记忆 / 出站链路。
//
// 算法：在尾部滑动窗口内搜索「短周期高重复」模式——若一个长度 ≤ MaxCycle
// 的子串连续重复 ≥ MinRepeats 次，判定为退化，从重复起点处截断。

import (
	"strings"
)

// RepetitionGuard 增量检测器：每次 Feed 一段 delta，内部累积全文并检测。
//
// 用法：
//
//	guard := NewRepetitionGuard()
//	for delta := range stream {
//	    if !guard.Feed(delta.Text) { break } // 已触发，停止消费
//	}
//	clean := guard.Text() // 截断后的干净文本
type RepetitionGuard struct {
	buf       string // 累积的全量文本
	triggered bool   // 是否已触发截断
	cutIdx    int    // 截断位置（首个重复周期的起始偏移）
}

// NewRepetitionGuard 创建一个新的检测器实例。
func NewRepetitionGuard() *RepetitionGuard {
	return &RepetitionGuard{}
}

// Feed 追加一段文本增量并检测是否出现重复退化。
//
// 返回值：
//   - true  = 文本健康，可继续接收
//   - false = 已检测到重复退化，调用方应停止向此 guard 投喂新文本
//
// 首次触发后，内部 buf 会自动截断至退化边界；后续 Feed 调用均为 no-op 且返回 false。
func (g *RepetitionGuard) Feed(delta string) bool {
	if g.triggered {
		return false
	}
	g.buf += delta

	// 累积量不足时跳过检测（短输出不可能形成有意义的重复）
	if len(g.buf) < minDetectLen {
		return true
	}

	if idx := detectRepetitionStart(g.buf); idx >= 0 {
		g.triggered = true
		g.cutIdx = idx
		g.buf = g.buf[:idx]
		return false
	}
	return true
}

// Text 返回经截断后的干净文本。
// 若未触发过退化检测，返回原始累积文本的完整副本。
func (g *RepetitionGuard) Text() string {
	return g.buf
}

// Triggered 报告是否已检测到重复退化。
func (g *RepetitionGuard) Triggered() bool {
	return g.triggered
}

// CutIndex 返回截断位置（仅在 Triggered=true 时有意义）。
func (g *RepetitionGuard) CutIndex() int {
	return g.cutIdx
}

// ============================================================================
// 一次性静态检测（用于非流式生成完成后的安检）
// ============================================================================

// DetectStaticRepetition 对完整文本做一次性重复退化检测。
//
// 返回 (cleanText, truncated)：
//   - cleanText  = 截断后的文本（或原文，若无退化）
//   - truncated = 是否执行了截断
func DetectStaticRepetition(text string) (string, bool) {
	if len(text) < minDetectLen {
		return text, false
	}
	idx := detectRepetitionStart(text)
	if idx < 0 {
		return text, false
	}
	return text[:idx], true
}

// ============================================================================
// 检测算法核心
// ============================================================================

const (
	// minDetectLen 开始检测的最小累积长度。低于此值的输出不检测，
	// 避免把正常的短句/列表误判为重复。
	minDetectLen = 60

	// maxCycle 检测的最大周期长度（字符数）。超过此长度的重复模式
	// 不视为「退化」，而是正常的内容相似性（如排比句）。
	maxCycle = 14

	// minRepeats 判定退化的最小连续重复次数（长周期默认值）。
	// 一个模式必须连续出现这么多次才触发截断——降低误判率
	// （正常文本也可能偶然重复 2~3 次）。
	minRepeats = 5

	// tailWindowSize 尾部窗口大小。只检查最近这些字符内的重复，
	// 因为退化总是发生在输出的末尾（模型从某处开始「卡死」）。
	tailWindowSize = 300
)

// minRepeatsForCycle 返回给定周期长度下判定退化所需的连续重复次数。
//
// 背景：统一按 minRepeats=5 判定时，短周期（1–2 字符）在正常文本里
// 频繁偶然命中——笑声「哈哈哈哈哈」、Misskey 自定义表情名
// （:ai_maze_hehehehehe: 含 5 个 "he"）、重复标点「。。。。。」。
// 2026-08-29 实测因此误截断一条 528 字符的正常回复（cut_index 526，
// 只截掉 2 字符但污染了判定与告警噪音）。
//
// 周期越短越容易误报，故要求越多次；周期 ≥5 时重复 5 次已足够异常，
// 保持原阈值。真正的退化（如 "NN BB NN BB…" 循环几十次）远超各档阈值，
// 仍会被正常捕获。
func minRepeatsForCycle(cycleLen int) int {
	switch {
	case cycleLen <= 2:
		return 15
	case cycleLen <= 4:
		return 10
	default:
		return minRepeats
	}
}

// detectRepetitionStart 在 text 中寻找重复退化的起始位置。
// 返回 ≥0 表示找到，值为重复起点的字节偏移；返回 -1 表示未发现退化。
func detectRepetitionStart(text string) int {
	// 只取尾部窗口——退化发生在末尾
	tail := text
	if len(tail) > tailWindowSize {
		tail = tail[len(tail)-tailWindowSize:]
	}

	// 按周期长度从短到长尝试（短周期更可能是真正的退化信号）
	for cycleLen := 1; cycleLen <= maxCycle; cycleLen++ {
		// 所需最小长度：cycleLen × 该周期档位要求的重复次数
		repeats := minRepeatsForCycle(cycleLen)
		minLen := cycleLen * repeats
		if len(tail) < minLen {
			continue
		}

		// 从后往前扫描，找第一个满足条件的重复起点
		for start := len(tail) - minLen; start >= 0; start-- {
			if isRepeatingCycle(tail, start, cycleLen, repeats) {
				// 找到一个匹配后，向左扩展找到重复区域的**真正**起始位置
				// （避免在重复区域内部截断，保留更多正常文本）
				start = extendRepetitionLeft(tail, start, cycleLen)

				globalStart := len(text) - len(tail) + start
				if globalStart < 20 {
					globalStart = 0
				}
				return globalStart
			}
		}
	}
	return -1
}

// isRepeatingCycle 检查 s 从 start 位置开始是否由 pattern（s[start:start+cycleLen]）
// 连续重复构成（至少 repeats-1 次后续完整匹配）。
// repeats 由 minRepeatsForCycle 按周期长度给出，短周期要求更多次以压误报。
//
// 前置条件：len(s) >= start + cycleLen * repeats。
func isRepeatingCycle(s string, start, cycleLen, repeats int) bool {
	pattern := s[start : start+cycleLen]

	// 排除纯空白模式（如连续空格/换行——属于格式问题而非语义退化）
	if strings.TrimSpace(pattern) == "" {
		return false
	}

	// 验证后续 repeats-1 次完整匹配
	for i := 1; i < repeats; i++ {
		pos := start + i*cycleLen
		end := pos + cycleLen
		if end > len(s) {
			break // 尾部不足一次完整匹配，不计入
		}
		if s[pos:end] != pattern {
			return false
		}
	}
	return true
}

// extendRepetitionLeft 从已确认的重复位置 start 向左扩展，找到重复区域的真正左边界。
//
// 前置：s[start: start+cycleLen*minRepeatsForCycle(cycleLen)] 已通过 isRepeatingCycle 验证。
// 返回向左扩展后的新起点（≤ start）。
func extendRepetitionLeft(s string, start, cycleLen int) int {
	if start == 0 {
		return 0
	}
	pattern := s[start : start+cycleLen]

	for {
		prevStart := start - cycleLen
		if prevStart < 0 {
			break
		}
		// 左侧相邻的 cycleLen 字符也匹配同一模式 → 重复区域可继续左扩
		if s[prevStart:start] != pattern {
			break
		}
		start = prevStart
	}
	return start
}
