package memory

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// precompress — LLM 摘要前的确定性前置压缩层
//
// 设计来源：headroom 项目（headroomlabs-ai/headroom）的传输层压缩思路。
// 核心洞察：在把记忆条目整批塞给 LLM 做聚类+摘要之前，先用零延迟、确定性的
// 规则瘦一轮，让 GLM 吃更少、更聚焦的输入 —— 同时降本、降延迟、可能提质量。
//
// 与 headroom 的差异（已划清边界）：
//   - headroom 是「传输层字节压缩」且可逆（CCR 回退取原文）；thinkbot 是「语义层
//     记忆整理」，目标是跨会话丢弃低价值细节。因此本层只做「减冗余字节」，
//     绝不替 LLM 做语义取舍（不做有损抽样，超大数组仅抽样并保留计数）。
//   - 可逆回溯由 thinkbot 已有的 Expander + 摘要 EntryIDs 承担，本层不重复造。
//
// token 计量：使用 llm.EstimateTokens（混合 CJK/ASCII 权重，±15%）。
// 不引入外部 tokenizer（如 tiktoken 对 GLM 并不准，且违背纯 Go 哲学）；
// 回退校验依赖估算器足以判断「压缩后是否反而变大」。
// ============================================================================

// mustKeepPatterns 抽取脆弱 token，防止 LLM 摘要吞掉关键信息。
// 对应 headroom 的 must-keep 正则：高熵/结构敏感 token 永远保留。
var mustKeepPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), // UUID
	regexp.MustCompile(`(?:/[\w.\-]+){2,}`),                      // 文件路径
	regexp.MustCompile(`https?://[^\s"'<>]+`),                    // URL
	regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`), // IP
	regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}_\d{2,}\b`),          // 错误码如 ERR_NOT_FOUND_404
	// 金额：¥/$ 前无单词边界（符号非单词），故不加前导 \b；
	// RMB/USD 是单词，加 \b 防止误匹配 "xUSD100"。
	regexp.MustCompile(`(?:¥|\$)\s?\d+(?:\.\d+)?|\b(?:RMB|USD)\s?\d+(?:\.\d+)?`),
	regexp.MustCompile(`[\w.]+@[\w.]+\.\w+`), // 邮箱
}

const (
	precompressMaxArrayElems = 50 // JSON 数组超过此数则抽样
	precompressSampleElems   = 12 // 抽样保留的元素数
	maxKeepPerPattern        = 5  // 每个 must-keep 模式最多抽取的数量
)

// preprocessBatch 对一批记忆条目做 LLM 摘要前的确定性瘦身。
// 不修改入参，返回内容可能被压缩的新切片。
func (c *SemanticCompactor) preprocessBatch(entries []TieredEntry) []TieredEntry {
	out := make([]TieredEntry, len(entries))
	for i, e := range entries {
		ne := e
		ne.Content = preprocessContent(e.Content, c.config.Precompress)
		out[i] = ne
	}
	return out
}

// preprocessContent 对单条内容做确定性压缩，顺序：
//  1. JSON 紧凑化（去空白、超大数组抽样并保留计数）
//  2. 长文本去重连续重复行
//  3. 回退校验：仅看「结构性紧凑化」是否真的省了 token，失灵则回退原文
//  4. must-keep 脆弱 token 抽取为保护段（摘要不得丢失），但仅在「附加后总 token
//     仍小于原文」时附加，否则放弃 —— 保信息让位于「不增大上下文」，这是引入
//     前置压缩的首要目标（对应 headroom 的 net_mutation_gain：压缩后必须真的
//     变小，否则回退）。
//
// enabled=false 时原样返回（对应 CompactionConfig.Precompress 关闭）。
func preprocessContent(content string, enabled bool) string {
	if !enabled {
		return content
	}
	if content == "" {
		return content
	}
	originalTokens := llm.EstimateTokens(content)

	// 1+2. 结构性紧凑化
	core := content
	if compacted, ok := compactJSON(core); ok {
		core = compacted
	}
	core = dedupeRepeatedLines(core)

	// 3. 回退校验：仅针对结构性紧凑化本身
	if llm.EstimateTokens(core) >= originalTokens {
		core = content
	}

	// 4. must-keep 段：仅在「附加后总 token 仍小于原文」时附加，否则放弃
	if keep := extractMustKeep(content); len(keep) > 0 {
		cand := core + "\n\n[保留项-摘要不得丢失] " + strings.Join(keep, "; ")
		if llm.EstimateTokens(cand) < originalTokens {
			core = cand
		}
	}

	return core
}

// PreprocessContent 导出包级入口，供 SQLiteCompactor（agent/storage 包）复用
// 同一套「LLM 摘要前确定性瘦身」逻辑，保证测试路径（SemanticCompactor）与生产
// 路径（SQLiteCompactor）行为一致。
func PreprocessContent(content string, enabled bool) string {
	return preprocessContent(content, enabled)
}

// compactJSON 将 JSON 文本重新序列化为紧凑格式（去空白）。
// 对超大数组做抽样并在结果中保留总数，避免有损丢失语义结构。
// 非 JSON 文本返回 (原串, false)。
func compactJSON(s string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return s, false
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return s, false
	}
	// 超大数组抽样：保留前 N 项 + 总数，而非整段丢弃
	if arr, ok := v.([]any); ok && len(arr) > precompressMaxArrayElems {
		sampled := arr
		if len(arr) > precompressSampleElems {
			sampled = arr[:precompressSampleElems]
		}
		v = map[string]any{
			"_sampled": sampled,
			"_total":   len(arr),
			"_note":    "原数组过大已抽样，仅保留前 " + strconv.Itoa(len(sampled)) + " / " + strconv.Itoa(len(arr)) + " 项",
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s, false
	}
	return string(b), true
}

// dedupeRepeatedLines 合并连续重复行，用 [重复行×N] 占位。
// 仅处理多行文本（日志、堆栈），单行不变。
func dedupeRepeatedLines(s string) string {
	if !strings.Contains(s, "\n") {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	var prev string
	repeat := 0
	flush := func() {
		if repeat > 0 {
			out = append(out, "[重复行×"+strconv.Itoa(repeat+1)+"]")
			repeat = 0
		}
	}
	for _, l := range lines {
		if l == prev {
			repeat++
			continue
		}
		flush()
		out = append(out, l)
		prev = l
	}
	flush()
	return strings.Join(out, "\n")
}

// extractMustKeep 抽取脆弱 token（UUID/路径/URL/错误码/金额/邮箱等），去重后返回。
// 基于原文抽取（JSON 压缩后这些值仍在，但原文抽取最稳）。
func extractMustKeep(s string) []string {
	var found []string
	for _, re := range mustKeepPatterns {
		for _, m := range re.FindAllString(s, maxKeepPerPattern) {
			found = append(found, m)
		}
	}
	seen := make(map[string]bool, len(found))
	uniq := found[:0]
	for _, f := range found {
		if !seen[f] {
			seen[f] = true
			uniq = append(uniq, f)
		}
	}
	return uniq
}
