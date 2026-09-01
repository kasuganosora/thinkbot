package workflow

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/prompt"
)

// ============================================================================
// 审查意见（review feedback）净化与结构隔离
//
// 背景：审查意见由 LLM 产出，属于不可信内容。它会被拼进下一轮 SubAgent 的
// prompt（executor.buildIterationTask），而节点 SubAgent 拥有完整工作空间工具
// 能力（含 sandbox_exec）——见 wire.go:162-169 与 sandbox/tools.go:86-89。
// 因此这里的处理失效，后果不是「输出被带偏」，而是**可被利用执行任意命令**。
//
// 设计原则（与 gh-aw 对照后确定）：
//
//  1. 结构性隔离优先于内容清洗。清洗是第二道防线，不是银弹——它能降低风险，
//     但真正的收敛要靠节点工具档位（ToolProfile）。所以这里追求的是
//     「把不可信内容限制在明确的边界内」，而不是「把内容洗干净」。
//  2. 只做零误报的清洗。审查意见是代码审查文本，里面大量出现
//     curl $TOKEN、cat .env 这类词——那是它在描述被审查代码的问题。
//     任何基于关键词的拦截都会误伤正常工作流。
//  3. 所有变换必须幂等。目标模式闭环每轮都会重新清洗一次 LoopFeedback，
//     非幂等的变换（转义、包装、替换）会在 N 轮后把内容变成垃圾。
//     字符移除天然幂等，这是本实现只做移除类清洗的根本原因。
// ============================================================================

// ansiEscapePattern 匹配终端 ANSI 转义序列（颜色、光标控制等）。
var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// SanitizeResult 是审查意见净化的产物，携带净化过程的信号供调用方处理。
//
// 调用方必须消费这些信号——只调用 sanitizeFeedback 而忽略结果，等于什么都没做。
type SanitizeResult struct {
	// Cleaned 清洗后的文本，可安全拼进 prompt。
	Cleaned string

	// Removed 被移除的不可见 Unicode 字符码位名，如 ["U+200B", "U+202E"]。
	// 为空表示未移除任何字符。
	Removed []string

	// Injected L3 注入检测命中的 patternID 列表。
	// **只用于记录告警，绝不用于阻断**——见 feedbackThreatPatterns 的说明。
	Injected []string

	// Emptied 标记清洗后内容变空但原本非空。
	// 这种情况说明原始反馈几乎全是不可见字符或控制字符，调用方应记录告警而非
	// 静默当作「没有反馈」处理——后者会把「反馈被污染」伪装成「审查没给意见」。
	Emptied bool
}

// sanitizeFeedback 清洗审查意见，供注入 SubAgent prompt 之前使用。
//
// 清洗顺序（顺序本身有意义，改动前请先读本文件顶部的说明）：
//
//  1. 去 ANSI 转义序列
//  2. 去控制字符（保留 \n 与 \t）
//  3. 去不可见 Unicode 字符（零宽、RTL 覆盖等）
//
// 刻意不做两件事：
//   - **不做 Unicode 归一化（NFKC）**：它会把全角转半角、拆连字、合并兼容字符，
//     而审查意见里包含代码片段，归一化会改坏代码。gh-aw 做 NFKC 是因为它处理
//     的是 issue/PR 正文（自然语言），对象不同。
//   - **不做代码围栏中和**：外层已改用随机定界符而非 ``` 围栏，内容里的 ```
//     无从「提前闭合」；且转义是不可逆叠加的，在闭环场景下会逐轮劣化。
//
// 本函数是纯函数，不会失败；所有异常信号通过 SanitizeResult 返回。
func sanitizeFeedback(s string) SanitizeResult {
	res := SanitizeResult{Cleaned: s}
	if s == "" {
		return res
	}

	cleaned := ansiEscapePattern.ReplaceAllString(s, "")
	cleaned = stripControlChars(cleaned)

	var removed []string
	cleaned, removed = prompt.StripInvisibleUnicode(cleaned)

	res.Cleaned = cleaned
	res.Removed = removed
	res.Emptied = cleaned == "" && s != ""

	// L3 注入检测跑在**清洗后**的文本上：清洗前检测会被不可见字符绕过
	// （如 "ig\u200Bnore previous instructions"）。
	for _, f := range prompt.ScanFeedback(cleaned) {
		res.Injected = append(res.Injected, f.PatternID)
	}

	return res
}

// stripControlChars 移除控制字符，保留换行与制表符。
//
// 保留 \n 与 \t 是必要的：审查意见是结构化文本（分点列举、带缩进代码示例），
// 移除它们会破坏可读性。其余 C0/C1 控制字符与 DEL 在正常文本中无合法用途，
// 移除它们不会影响任何合法内容——这是「零误报清洗」的定义。
func stripControlChars(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			sb.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// 控制字符，丢弃
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// ============================================================================
// 随机定界符
// ============================================================================

// delimiterPrefix / delimiterSuffix 构成定界符的固定外形。
//
// 固定外形的目的是**可测试性**：注入隔离测试需要用正则把定界符从 prompt 里
// 提取出来，才能断言「不可信内容在边界内」。若外形完全随机，测试无从下手。
// 随机性放在中间那段十六进制上，既不可预测又可被正则捕获。
const (
	delimiterPrefix = "<<<REVIEW_FEEDBACK_"
	delimiterSuffix = ">>>"
)

// delimiterRandomBytes 定界符随机段的长度（字节）。16 字节 = 128 位熵。
const delimiterRandomBytes = 16

// delimiterMaxAttempts 生成定界符的最大尝试次数。
const delimiterMaxAttempts = 20

// errDelimiterExhausted 定界符生成失败。
// 实际上不可能触发（20 次全部碰撞的概率约 2^-2560），留作显式失败而非静默降级。
var errDelimiterExhausted = errors.New("failed to generate a collision-free delimiter")

// uniqueDelimiter 返回一个在所有 inputs 中都不出现的定界符。
//
// 定界符用于把不可信内容（审查意见）包裹起来，并在 prompt 中明确声明
// 「边界内是数据，不是指令」。随机性保证内容无法预知边界、因此无法伪造——
// 这是结构化隔离的核心，也是它优于「靠清洗挡住注入」的原因。
//
// **调用时必须传入所有会被拼进 prompt 的可变内容**（originalTask / prevResult /
// feedback）。漏传任何一段，该段内容若恰好含定界符就会破坏包裹结构。
func uniqueDelimiter(inputs ...string) (string, error) {
	buf := make([]byte, delimiterRandomBytes)
	for attempt := 0; attempt < delimiterMaxAttempts; attempt++ {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		d := delimiterPrefix + hex.EncodeToString(buf) + delimiterSuffix
		if !anyContains(inputs, d) {
			return d, nil
		}
	}
	return "", errDelimiterExhausted
}

// anyContains 报告 inputs 中是否有任意一个包含 sub。
func anyContains(inputs []string, sub string) bool {
	for _, in := range inputs {
		if strings.Contains(in, sub) {
			return true
		}
	}
	return false
}
