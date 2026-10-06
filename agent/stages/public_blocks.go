package stages

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

// 多段 <public> 的合并与近重复去重。
//
// 一轮回复可以跨多个工具步骤：processStream 把所有步骤的文本拼进 result.Text，所以模型在
// 第 1 步写的过场话（"容栞娜翻翻资料…"）和最后一步的真正答复会是两个不同的 <public> 区块。
// 5fb36fa 为了避免「同一段话发两遍」改成只取第一个区块，结果把真正的答复丢了
// （2026-10-06 21:49 TG 私聊只发出了过场话）。
//
// 现在的规则（extractPublicReply 与 OutputCleanStreamFilter 共用，保证渠道发送 / 落库 /
// Web 流式三处一致）：
//   - 按出现顺序保留每个清洗后非空的区块，用空行（"\n\n"）连接；
//   - 与任一已保留区块「近乎相同」的区块丢弃（保留先出现的那份——流式已经推出去的文字收不回来）。
//
// 「近乎相同」= 归一化（全角折半角、转小写、去掉标点/空白/符号/emoji）后：
//   - 前 publicDupPrefixRunes 个字的相似度 ≥ publicDupPrefixSim（短区块 ≥ publicDupShortSim），且
//   - 全文相似度 ≥ publicDupFullSim（相似度 = 1 - 编辑距离/较长者长度）。
//
// 阈值依据线上历史多区块样本：真重复（全半角/错别字差异 0.98+、改写 前缀0.83/全文0.58）
// 都能命中；不同内容（过场话+答复、错误结论+更正）前缀相似度都在 0.15 以下，不会被误判。
const (
	publicDupPrefixRunes = 48
	publicDupPrefixSim   = 0.8
	publicDupFullSim     = 0.5
	// publicDupShortSim 用于不足 publicDupPrefixRunes 字的短区块（此时前缀即全文）。
	publicDupShortSim = 0.9
	// publicDupMaxRunes 限制参与编辑距离计算的长度（O(n²)），超长区块只比较前这么多字。
	publicDupMaxRunes = 2000
)

// blockOpenRe / internalCloseRe 用于定位区块起点、跳过顶层 <internal> 区块（大小写不敏感）。
var (
	blockOpenRe     = regexp.MustCompile(`(?i)<public>|<internal>`)
	internalCloseRe = regexp.MustCompile(`(?i)</internal>`)
)

// publicBlockContents 按顺序返回 clean 中所有成对 <public> 区块的原始内文，跳过位于顶层
// <internal> 区块内的 <public>（那是心里话的一部分，与流式过滤器的处理一致）。
// 未闭合的 <public> / <internal> 之后的内容一律不取。
func publicBlockContents(clean string) []string {
	var out []string
	pos := 0
	for pos < len(clean) {
		loc := blockOpenRe.FindStringIndex(clean[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		if strings.EqualFold(clean[start:pos+loc[1]], "<internal>") {
			end := internalCloseRe.FindStringIndex(clean[pos+loc[1]:])
			if end == nil {
				break // 未闭合 internal：其后全是心里话
			}
			pos = pos + loc[1] + end[1]
			continue
		}
		m := publicTagRe.FindStringSubmatchIndex(clean[start:])
		if m == nil || m[0] != 0 {
			break // 未闭合的 <public>（流式截断的半句话）不取
		}
		out = append(out, clean[start+m[2]:start+m[3]])
		pos = start + m[1]
	}
	return out
}

// cleanPublicBlock 对单个 <public> 区块内文做出站清洗：剥离夹带的 <internal> 与残留标签。
func cleanPublicBlock(content string) string {
	out := memory.StripInternalTags(strings.TrimSpace(content))
	return strings.TrimSpace(strayTagRe.ReplaceAllString(out, ""))
}

// publicBlockSet 记录已决定发送的区块（及其归一化形式）。
type publicBlockSet struct {
	raw  []string
	norm [][]rune
}

func (s *publicBlockSet) add(block string) {
	s.raw = append(s.raw, block)
	s.norm = append(s.norm, normalizeForDedup(block))
}

func (s *publicBlockSet) len() int { return len(s.raw) }

func (s *publicBlockSet) join() string { return strings.Join(s.raw, "\n\n") }

// isDuplicate 报告 block 是否与任一已保留区块近乎相同。
func (s *publicBlockSet) isDuplicate(block string) bool {
	nb := normalizeForDedup(block)
	for i, nk := range s.norm {
		if len(nb) == 0 || len(nk) == 0 {
			// 纯符号/emoji 区块：只有完全相同才算重复。
			if strings.TrimSpace(block) == strings.TrimSpace(s.raw[i]) {
				return true
			}
			continue
		}
		if prefixSimilar(nk, nb) && runeSimilarity(capRunes(nk, publicDupMaxRunes), capRunes(nb, publicDupMaxRunes)) >= publicDupFullSim {
			return true
		}
	}
	return false
}

// prefixVerdict 供流式过滤器在区块闭合前提前裁决。block 的归一化前缀不足
// publicDupPrefixRunes 个字时 decided=false（还不能判断）；满了之后前缀就不会再变：
//   - distinct=true：与所有已保留区块的前缀都不相似 —— isDuplicate 必然为 false
//     （它要求前缀相似），可以不等闭合就开始推送；
//   - distinct=false：前缀与某个已保留区块相似，是重复候选，须等闭合后用 isDuplicate 裁决。
func (s *publicBlockSet) prefixVerdict(block string) (decided, distinct bool) {
	nb := normalizeForDedup(block)
	if len(nb) < publicDupPrefixRunes {
		return false, false
	}
	for _, nk := range s.norm {
		if len(nk) > 0 && prefixSimilar(nk, nb) {
			return true, false
		}
	}
	return true, true
}

// prefixSimilar 比较两者归一化后的前 publicDupPrefixRunes 个字。短文本的编辑距离比例太粗
// （"第一段" vs "第二段" 只差一字就有 0.67），所以比较长度不足时要求更高的
// publicDupShortSim。阈值只取决于截断后的长度：流式提前裁决时 block 前缀已满，
// 与闭合后 isDuplicate 的判断完全一致。
func prefixSimilar(a, b []rune) bool {
	a, b = capRunes(a, publicDupPrefixRunes), capRunes(b, publicDupPrefixRunes)
	th := publicDupPrefixSim
	if max(len(a), len(b)) < publicDupPrefixRunes {
		th = publicDupShortSim
	}
	return runeSimilarity(a, b) >= th
}

func capRunes(r []rune, n int) []rune {
	if len(r) > n {
		return r[:n]
	}
	return r
}

// normalizeForDedup 全角 ASCII 折成半角、转小写，只保留字母/数字/组合附加符号。
func normalizeForDedup(s string) []rune {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
			out = append(out, unicode.ToLower(r))
		}
	}
	return out
}

// runeSimilarity = 1 - Levenshtein(a,b)/max(len)；两者皆空视为相同。
func runeSimilarity(a, b []rune) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	return 1 - float64(levenshtein(a, b))/float64(maxLen)
}

func levenshtein(a, b []rune) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
