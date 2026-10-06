package stages

import (
	"strings"
	"unicode"
)

// OutputCleanStreamFilter 在流式增量里做与出站门控（LLMStage.Process 中
// StripThinking → extractPublicReply 那条共享清洗链）同口径的标签清洗，让 Web 渠道
// 实时推送的 delta 与其他渠道最终发出的内容一致：
//
//   - <think>/<thinking> 块：始终整块丢弃（与 memory.StripThinking 一致，未闭合则丢到流尾）；
//   - replyTags=true（回复控制门控生效时，与 Process 中 RequireReplyControl && !outreach 同条件）：
//   - <internal>…</internal>：整块丢弃，闭合标签可以在任意后续 chunk 到达；未闭合则丢到流尾；
//     位于 <public> 内时遇到 </public> 也视为结束（与 publicTagRe 非贪婪匹配后再
//     StripInternalTags 的效果一致）；
//   - <public>/</public>：去掉标签本身、保留内文；多个区块按 extractPublicReply 的规则
//     合并（见 public_blocks.go）：第一个非空区块直接推送，之后的区块先扣住，一旦确定不是
//     已推送区块的近重复（归一化前缀满 publicDupPrefixRunes 字且前缀不相似）就补上 "\n\n"
//     分隔并继续实时推送；前缀相似的区块等闭合后按完整规则裁决，重复则整块丢弃；
//     流结束时仍未闭合、尚未放行的区块丢弃（门控也不取未闭合区块）；
//   - 一旦出现过 <internal> 或 <public>，区块之外的裸文本不再放行（最终出站只会取
//     public 内文，或因「有 internal 无 public」整段 fail-closed）；
//   - 其余 <tag …> / </tag> 与畸形的 /public> /internal>：去掉标签文本（对应 strayTagRe）。
//   - 首部空白与尾部空白不放行（出站内容均 TrimSpace），中间空白在后续有正文时补发。
//
// 只扣住「可能是标签开头」的尾部（最长 maxStreamTagLen 字节）与尾部空白，其余立即放行。
// 与 ReplyControlStreamFilter 一样只影响推给前端的流，result.Text 保留原文，
// 发送与否仍由门控基于完整原文裁决；最终落库/回放内容以门控产出的 payload 为准。
// 非并发安全，每条流一个实例。
type OutputCleanStreamFilter struct {
	replyTags bool

	mode       ocMode
	parent     ocMode // think / internal 块结束后回到的模式
	pending    string // 扣住的原文尾部（可能的半截标签 / 闭合标签前缀）
	ws         string // 已处理、等待后续正文的尾部空白
	emitted    bool   // 是否已放行过非空白正文（控制首部空白）
	trimLead   bool   // 刚进入 <public>：跳过区块内首部空白
	sawPrivate bool   // 出现过 <internal> / <public>：区块外裸文本不再放行
	pubContent bool   // 当前 <public> 区块是否已有非空白内容

	kept         publicBlockSet // 已推送（决定发送）的 <public> 区块
	cur          string         // 当前 <public> 区块已清洗的内文（不含尾部空白）
	hold         bool           // 当前区块尚未确定不是重复：内文只进 cur，暂不推送
	dupCandidate bool           // 扣住的区块前缀已与某个已推送区块相似：等闭合后裁决
}

type ocMode int

const (
	ocOutside  ocMode = iota // 区块之外
	ocPublic                 // <public> 区块内
	ocInternal               // <internal> 区块内（丢弃）
	ocThink                  // <think> 区块内（丢弃）
)

// maxStreamTagLen 是为判定「是否为标签」最多扣住的字节数；超过仍未见 '>' 则按普通文本放行
// （最终内容仍由门控 payload 兜底，流式只做有界延迟）。
const maxStreamTagLen = 64

// NewOutputCleanStreamFilter 创建过滤器。replyTags 与出站门控是否剥离
// <public>/<internal>/残留标签保持一致（RequireReplyControl && !outreach）。
func NewOutputCleanStreamFilter(replyTags bool) *OutputCleanStreamFilter {
	return &OutputCleanStreamFilter{replyTags: replyTags}
}

// Feed 输入一段增量，返回可以立即安全发出的文本（可能为空）。
func (f *OutputCleanStreamFilter) Feed(delta string) string {
	if delta == "" {
		return ""
	}
	var out strings.Builder
	buf := f.pending + delta
	f.pending = ""
	f.process(buf, &out, false)
	return out.String()
}

// Flush 在流结束时调用：放行仍扣住的普通文本（最终没凑成标签的半截 "<b" 等），
// 丢弃未闭合的 think/internal 块与尾部空白。调用后过滤器回到初始状态。
func (f *OutputCleanStreamFilter) Flush() string {
	var out strings.Builder
	if f.pending != "" {
		buf := f.pending
		f.pending = ""
		f.process(buf, &out, true)
	}
	*f = OutputCleanStreamFilter{replyTags: f.replyTags}
	return out.String()
}

// process 处理 buf；final=true 表示流已结束，半截标签不再等待。
func (f *OutputCleanStreamFilter) process(buf string, out *strings.Builder, final bool) {
	for buf != "" {
		switch f.mode {
		case ocThink:
			idx, n := indexFoldAny(buf, "</think>", "</thinking>")
			if idx < 0 {
				if !final {
					f.pending = buf[len(buf)-closingPrefixLen(buf, "</think>", "</thinking>"):]
				}
				return
			}
			buf = buf[idx+n:]
			f.mode = f.parent

		case ocInternal:
			var idx, n int
			if f.parent == ocPublic {
				idx, n = indexFoldAny(buf, "</internal>", "</public>")
			} else {
				idx, n = indexFoldAny(buf, "</internal>")
			}
			if idx < 0 {
				if !final {
					if f.parent == ocPublic {
						f.pending = buf[len(buf)-closingPrefixLen(buf, "</internal>", "</public>"):]
					} else {
						f.pending = buf[len(buf)-closingPrefixLen(buf, "</internal>"):]
					}
				}
				return
			}
			closing := asciiLower(buf[idx : idx+n])
			buf = buf[idx+n:]
			if closing == "</public>" {
				f.closePublic(out)
			} else {
				f.mode = f.parent
			}

		default: // ocOutside / ocPublic
			i := f.nextTagStart(buf)
			if i < 0 {
				f.emit(buf, out)
				return
			}
			f.emit(buf[:i], out)
			buf = buf[i:]
			kind, n := f.classify(buf, final)
			switch kind {
			case tagIncomplete:
				f.pending = buf
				return
			case tagNone:
				f.emit(buf[:1], out)
				buf = buf[1:]
				continue
			}
			buf = buf[n:]
			f.applyTag(kind, out)
		}
	}
}

type tagKind int

const (
	tagNone       tagKind = iota // 不是标签：首字节按普通文本放行
	tagIncomplete                // 可能是标签但还没凑齐：扣住
	tagThinkOpen
	tagInternalOpen
	tagPublicOpen
	tagPublicClose
	tagStray // 其余标签 / 畸形 /public> /internal>：去掉标签文本
)

// nextTagStart 返回下一个可能的标签起点（'<'，门控剥离标签时还包括畸形标签的 '/'）。
func (f *OutputCleanStreamFilter) nextTagStart(s string) int {
	if f.replyTags {
		return strings.IndexAny(s, "</")
	}
	return strings.IndexByte(s, '<')
}

// classify 判定 s（以 '<' 或 '/' 开头）的标签类型与长度。
func (f *OutputCleanStreamFilter) classify(s string, final bool) (tagKind, int) {
	if s[0] == '/' {
		// 畸形标签（缺 '<'）：只认 /public> 与 /internal>，避免 URL 等正文里的 '/' 被长时间扣住。
		for _, t := range []string{"/public>", "/internal>"} {
			if hasPrefixFold(s, t) {
				return tagStray, len(t)
			}
			if !final && len(s) < len(t) && hasPrefixFold(t, s) {
				return tagIncomplete, 0
			}
		}
		return tagNone, 0
	}

	if !f.replyTags {
		// 只处理 think 标签（StripThinking 无条件生效），其余标签原样放行。
		for _, t := range []string{"<think>", "<thinking>"} {
			if hasPrefixFold(s, t) {
				return tagThinkOpen, len(t)
			}
		}
		if !final && (hasPrefixFold("<thinking>", s) || hasPrefixFold("<think>", s)) {
			return tagIncomplete, 0
		}
		return tagNone, 0
	}

	// strayTagRe 形态：</? + 字母 + [a-zA-Z0-9_-]* + [^>]* + '>'
	p := 1
	if p < len(s) && s[p] == '/' {
		p++
	}
	if p >= len(s) {
		if final {
			return tagNone, 0
		}
		return tagIncomplete, 0
	}
	if !isASCIILetter(s[p]) {
		return tagNone, 0
	}
	limit := len(s)
	if limit > maxStreamTagLen {
		limit = maxStreamTagLen
	}
	end := strings.IndexByte(s[:limit], '>')
	if end < 0 {
		if !final && len(s) < maxStreamTagLen {
			return tagIncomplete, 0
		}
		return tagNone, 0
	}
	switch asciiLower(s[:end+1]) {
	case "<think>", "<thinking>":
		return tagThinkOpen, end + 1
	case "<internal>":
		return tagInternalOpen, end + 1
	case "<public>":
		return tagPublicOpen, end + 1
	case "</public>":
		return tagPublicClose, end + 1
	}
	return tagStray, end + 1
}

func (f *OutputCleanStreamFilter) applyTag(kind tagKind, out *strings.Builder) {
	switch kind {
	case tagThinkOpen:
		f.parent = f.mode
		f.mode = ocThink
	case tagInternalOpen:
		if f.mode == ocOutside {
			f.sawPrivate = true
			f.ws = ""
		}
		f.parent = f.mode
		f.mode = ocInternal
	case tagPublicOpen:
		if f.mode == ocOutside {
			f.sawPrivate = true
			f.mode = ocPublic
			f.ws = ""
			f.trimLead = true
			f.pubContent = false
			f.cur = ""
			f.dupCandidate = false
			// 已有推送过的区块：新区块可能是重复，先扣住等判定。
			f.hold = f.kept.len() > 0
		}
		// 已在 <public> 内：嵌套的字面开标签按残留标签去掉。
	case tagPublicClose:
		if f.mode == ocPublic {
			f.closePublic(out)
		}
		// 区块外的孤立 </public>：按残留标签去掉。
	}
}

// closePublic 结束当前 <public> 区块并回到区块外（继续等下一个区块）。
// 非空区块记入 kept；仍在扣住的区块此时按完整规则裁决：不重复则补发，重复则丢弃。
func (f *OutputCleanStreamFilter) closePublic(out *strings.Builder) {
	f.ws = ""
	f.mode = ocOutside
	if f.pubContent {
		if !f.hold {
			f.kept.add(f.cur)
		} else if !f.kept.isDuplicate(f.cur) {
			out.WriteString("\n\n")
			out.WriteString(f.cur)
			f.kept.add(f.cur)
		}
	}
	f.cur, f.hold, f.dupCandidate, f.pubContent = "", false, false, false
}

// emit 放行一段已确认不含标签的正文：处理首部/尾部空白，并按模式决定是否可见。
func (f *OutputCleanStreamFilter) emit(s string, out *strings.Builder) {
	if s == "" {
		return
	}
	if f.mode == ocOutside && f.replyTags && f.sawPrivate {
		return
	}
	if f.mode == ocPublic && f.trimLead {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if s == "" {
			return
		}
		f.trimLead = false
	}
	if !f.emitted {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if s == "" {
			return
		}
	}
	body := strings.TrimRightFunc(s, unicode.IsSpace)
	if body == "" {
		f.ws += s
		return
	}
	visible := f.ws + body
	f.ws = s[len(body):]
	f.emitted = true
	if f.mode == ocPublic {
		f.pubContent = true
		f.cur += visible
		if f.hold {
			if f.dupCandidate {
				return // 重复候选：扣到闭合再裁决
			}
			decided, distinct := f.kept.prefixVerdict(f.cur)
			if !decided || !distinct {
				f.dupCandidate = decided
				return // 还不能确定 / 可能是重复：继续扣住
			}
			f.hold = false
			out.WriteString("\n\n")
			out.WriteString(f.cur)
			return
		}
	}
	out.WriteString(visible)
}

// indexFoldAny 在 s 中按 ASCII 大小写不敏感查找最早出现的任一 needle，返回位置与长度。
func indexFoldAny(s string, needles ...string) (int, int) {
	ls := asciiLower(s)
	best, n := -1, 0
	for _, nd := range needles {
		if i := strings.Index(ls, nd); i >= 0 && (best < 0 || i < best) {
			best, n = i, len(nd)
		}
	}
	return best, n
}

// closingPrefixLen 返回 s 的最长后缀长度，该后缀是某个 needle 的真前缀（需扣住等待后续 chunk）。
func closingPrefixLen(s string, needles ...string) int {
	best := 0
	for _, nd := range needles {
		maxK := len(nd) - 1
		if maxK > len(s) {
			maxK = len(s)
		}
		for k := maxK; k > best; k-- {
			if hasPrefixFold(nd, s[len(s)-k:]) {
				best = k
				break
			}
		}
	}
	return best
}

// hasPrefixFold 报告 s 是否以 prefix 开头（ASCII 大小写不敏感；prefix 为小写）。
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return asciiLower(s[:len(prefix)]) == asciiLower(prefix)
}

func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
