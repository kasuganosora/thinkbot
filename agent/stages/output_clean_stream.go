package stages

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/secret"
)

// OutputCleanStreamFilter 在流式增量里做与出站门控（LLMStage.Process 中
// StripThinking → extractPublicReply 那条共享清洗链）同口径的标签清洗，让 Web 渠道
// 实时推送的 delta 与其他渠道最终发出的内容一致：
//
//   - <think>/<thinking> 块：始终整块丢弃（与 memory.StripThinking 一致，未闭合则丢到流尾）；
//   - replyTags=true（回复控制门控生效时，与 Process 中 RequireReplyControl && !outreach 同条件）：
//   - `<internal`…：整块丢弃。开标签的 `>` 可缺，看到完整的 `<internal` 且后面不是更长的
//     标签名就进入丢弃，不再等 `>`，也不会把半截 `<internal` 当普通文本放行；
//     闭合认 `</internal>`、`/internal>`、`/internal`（`>` 可缺，后面若还是标签名则不算），
//     闭合标记可以在任意后续 chunk 到达；未闭合则丢到流尾；
//     位于 <public> 内时遇到 </public> 也视为结束（与 publicTagRe 非贪婪匹配后再
//     StripInternalTags 的效果一致）；
//   - <public>/</public>：去掉标签本身、保留内文；多个区块按 extractPublicReply 的规则
//     合并（见 public_blocks.go）：第一个非空区块直接推送，之后的区块先扣住，一旦确定不是
//     已推送区块的近重复（归一化前缀满 publicDupPrefixRunes 字且前缀不相似）就补上 "\n\n"
//     分隔并继续实时推送；前缀相似的区块等闭合后按完整规则裁决，重复则整块丢弃；
//     流结束时仍未闭合、尚未放行的区块丢弃（门控也不取未闭合区块）；
//   - 一旦出现过 <internal> 或 <public>，区块之间的裸文本先扣住；流结束时若
//     最后一个 </public> 之后仍有「有实质内容」的裸文本，则按 extractPublicReply
//     同样规则补发（用空行分隔），避免模型把答复写在标签外时丢掉；
//   - 其余 <tag …> / </tag> 与畸形的 /public、/internal（`>` 可缺）：去掉标签文本（对应 strayTagRe）。
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
	released     bool           // 当前区块已提前放行（内容已推给用户，收不回来）
	// released 的区块要到 closePublic 才记入 kept，所以「新区块要不要先扣住判重」不能只
	// 看 kept.len()——否则已推送但尚未闭合的区块不算数。当前代码路径里这段窗口不可达
	//（未闭合时新的 <public> 开标签按残留标签忽略，闭合时 closePublic 已 add），
	// 这是防御性加固：将来若允许 public 嵌套/重入，判重不会凭空失效。
	trail string // </public> 之后、下一标签之前的裸文本（Flush 时若实质则补发）
	// looseInternalOpen 为真表示当前 internal 区是缺 '>' 的开标签。
	// 流结束时若仍未闭合，trail 里残片前面的半句也不能补发（与 extract 的 trailing 闸门一致）。
	looseInternalOpen bool

	// secHold 是跨 chunk 密钥遮蔽的尾部缓冲：保留「可能是某条已登记密钥真前缀」的
	// 尾部字节到下一次 Feed，确保跨界值在被推送前完整落进某次拼接被 Redact。
	// 空库时恒为空，不改变既有流式行为。
	secHold string
}

type ocMode int

const (
	ocOutside  ocMode = iota // 区块之外
	ocPublic                 // <public> 区块内
	ocInternal               // <internal> 区块内（丢弃）
	ocThink                  // <think> 区块内（丢弃）
	ocRecall                 // 召回记忆块内（丢弃，见 tagRecallOpen）
)

// maxStreamTagLen 是为判定「是否为标签」最多扣住的字节数；超过仍未见 '>' 则按普通文本放行
// （最终内容仍由门控 payload 兜底，流式只做有界延迟）。
// `<internal` 不走这条放行：名字一凑齐就进入丢弃，缺 '>' 也不能把后面的心里话吐出去。
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
	// 推送前做跨 chunk 安全的密钥遮蔽：尾部疑似未闭合值的前缀扣到下一次增量。
	return f.redactStream(out.String(), false)
}

// Flush 在流结束时调用：放行仍扣住的普通文本（最终没凑成标签的半截 "<b" 等），
// 丢弃未闭合的 think/internal 块与尾部空白；若 </public> 后扣住的裸文本有实质内容，
// 按 extractPublicReply 同样规则补发。调用后过滤器回到初始状态。
func (f *OutputCleanStreamFilter) Flush() string {
	var out strings.Builder
	if f.pending != "" {
		buf := f.pending
		f.pending = ""
		f.process(buf, &out, true)
	}
	if f.mode == ocInternal && f.looseInternalOpen {
		f.trail = ""
	}
	f.flushTrailing(&out)
	// 流结束：把跨 chunk 缓冲拼回，一并 Redact 后全部放行。
	text := f.redactStream(out.String(), true)
	*f = OutputCleanStreamFilter{replyTags: f.replyTags}
	return text
}

// flushTrailing 在流结束时把最后一个 </public> 之后扣住的实质裸文本并入公开回复。
// 仅在已推送过至少一个 <public> 区块时生效（与 extractPublicReply 路径 1 一致；
// 有 internal 无 public 时仍 fail-closed，不把裸文本漏出去）。
func (f *OutputCleanStreamFilter) flushTrailing(out *strings.Builder) {
	c := cleanPublicBlock(f.trail)
	f.trail = ""
	if f.kept.len() == 0 {
		return
	}
	if !significantTrailingPublic(c) || f.kept.isDuplicate(c) {
		return
	}
	out.WriteString("\n\n")
	out.WriteString(c)
	f.kept.add(c)
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
			idx, n, hold := memory.NextLooseInternalClose(buf, final)
			if f.parent == ocPublic {
				if p, pn := indexFoldAny(buf, "</public>"); p >= 0 && (idx < 0 || p < idx) {
					idx, n, hold = p, pn, 0
				} else if idx < 0 && !final {
					if k := closingPrefixLen(buf, "</public>"); k > hold {
						hold = k
					}
				}
			}
			if idx < 0 {
				if !final && hold > 0 {
					f.pending = buf[len(buf)-hold:]
				}
				return
			}
			closing := asciiLower(buf[idx : idx+n])
			buf = buf[idx+n:]
			f.looseInternalOpen = false
			if closing == "</public>" {
				f.closePublic(out)
			} else {
				f.mode = f.parent
			}

		case ocRecall:
			// 闭合标签可在任意后续 chunk 到达；位于 <public> 内时遇 </public> 也视为结束
			// （与 ocInternal 同口径）。未闭合则丢到流尾。
			var idx, n int
			if f.parent == ocPublic {
				idx, n = indexFoldAny(buf, "</recalled_memory>", "</user_profile>", "</public>")
			} else {
				idx, n = indexFoldAny(buf, "</recalled_memory>", "</user_profile>")
			}
			if idx < 0 {
				if !final {
					if f.parent == ocPublic {
						f.pending = buf[len(buf)-closingPrefixLen(buf, "</recalled_memory>", "</user_profile>", "</public>"):]
					} else {
						f.pending = buf[len(buf)-closingPrefixLen(buf, "</recalled_memory>", "</user_profile>"):]
					}
				}
				return
			}
			closing := asciiLower(buf[idx : idx+n])
			buf = buf[idx+n:]
			if closing == "</public>" {
				// closePublic 自己会把 mode 置回 ocOutside，不要再设 ocPublic
				// （与 ocInternal 同口径）。
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
			opened := buf[:n]
			buf = buf[n:]
			if kind == tagInternalOpen && (len(opened) == 0 || opened[len(opened)-1] != '>') {
				f.looseInternalOpen = true
			}
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
	tagRecallOpen // <recalled_memory> / <user_profile>：连内容整块丢弃
	tagStray      // 其余标签 / 畸形 /public /internal（'>' 可缺）：去掉标签文本
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
		// 畸形闭标签（缺 '<'）：/public 与 /internal，`>` 可有可无。
		// 只认这两个名字，避免 URL 等正文里的 '/' 被长时间扣住。
		return classifyLooseSlash(s, final)
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

	// `<internal` 的 `>` 可缺。必须在下面的「等 '>' / 超过 maxStreamTagLen 就放行」之前判定，
	// 否则缺 '>' 的心里话会在 64 字节后被当成普通文本吐出。
	if n, match, incomplete := memory.LooseInternalOpenAt(s); incomplete && !final {
		return tagIncomplete, 0
	} else if match {
		return tagInternalOpen, n
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
	// 召回记忆块被整段复述：连内容一起丢（与 cleanPublicBlock 同口径）。
	// 跨会话召回把私聊记忆带进群聊后，这里拦住「流式先吐出去、门控再删」
	// 的时间差——Web 流是实时可见的，晚删等于已经泄露。
	case "<recalled_memory>", "<user_profile>":
		return tagRecallOpen, end + 1
	}
	return tagStray, end + 1
}

// classifyLooseSlash 识别缺 '<' 的 /public 与 /internal。`>` 可有可无；
// 后面若还是标签名字符（/internally、/publication）则不是残缺闭标签。
func classifyLooseSlash(s string, final bool) (tagKind, int) {
	for _, name := range []string{"public", "internal"} {
		token := "/" + name
		if len(s) < len(token) && hasPrefixFold(token, s) {
			if !final {
				return tagIncomplete, 0
			}
			return tagNone, 0
		}
	}
	for _, name := range []string{"public", "internal"} {
		token := "/" + name
		if !hasPrefixFold(s, token) {
			continue
		}
		if len(s) == len(token) {
			if !final {
				return tagIncomplete, 0
			}
			return tagStray, len(token)
		}
		c := s[len(token)]
		if c == '>' {
			return tagStray, len(token) + 1
		}
		if isTagNameChar(c) {
			return tagNone, 0
		}
		return tagStray, len(token)
	}
	return tagNone, 0
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
			// 不清空 trail：</public> 与后续 <internal> 之间的裸文本仍属 trailing。
		}
		f.parent = f.mode
		f.mode = ocInternal
	case tagPublicOpen:
		if f.mode == ocOutside {
			f.sawPrivate = true
			f.mode = ocPublic
			f.ws = ""
			f.trail = "" // 区块之间的裸文本丢弃（只要后面还有 <public>）
			f.trimLead = true
			f.pubContent = false
			f.cur = ""
			f.dupCandidate = false
			// 已有推送过的区块（含「已提前放行但还没闭合」的）→ 新区块可能是重复，先扣住等判定。
			f.hold = f.kept.len() > 0 || f.released
			f.released = false
		}
		// 已在 <public> 内：嵌套的字面开标签按残留标签去掉。
	case tagPublicClose:
		if f.mode == ocPublic {
			f.closePublic(out)
		}
		// 区块外的孤立 </public>：按残留标签去掉。
	case tagRecallOpen:
		f.parent = f.mode
		f.mode = ocRecall
	}
}

// closePublic 结束当前 <public> 区块并回到区块外（继续等下一个区块）。
// 非空区块记入 kept；仍在扣住的区块此时按完整规则裁决：不重复则补发，重复则丢弃。
// 已提前放行（released，hold=false）的区块一律记入 kept——内容已经推给用户，
// 此时再判重复只会让「流式看到的」和「落库/渠道发出的」对不上。
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
	f.cur, f.hold, f.dupCandidate, f.pubContent, f.released = "", false, false, false, false
}

// emit 放行一段已确认不含标签的正文：处理首部/尾部空白，并按模式决定是否可见。
func (f *OutputCleanStreamFilter) emit(s string, out *strings.Builder) {
	if s == "" {
		return
	}
	if f.mode == ocOutside && f.replyTags && f.sawPrivate {
		// 扣住：可能是末个 </public> 后的答复正文，Flush 时再裁决。
		f.trail += s
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
			f.released = true // 已推出去：即便闭合时判为重复也收不回来，必须照发
			out.WriteString("\n\n")
			out.WriteString(f.cur)
			return
		}
	}
	out.WriteString(visible)
}

// redactStream 对一段「即将推送」的清洗后文本做跨 chunk 安全的密钥遮蔽。
//
// 顺序很关键：先 Redact 把当前 buf 里完整出现的真实值全部替换成占位符，再按
// StreamHoldLen 把尾部「疑似未闭合值前缀」扣住到下一次增量。若只扣不 Redact，
// 完整的短值会被留到下一 chunk 才遮蔽（多延迟一步）；若只 Redact 不扣，跨界值
// 的前半段会被当作普通文本提前推送出去（泄露）。final=true 表示流已结束，把
// secHold 拼回一并 Redact 后全部放行。
func (f *OutputCleanStreamFilter) redactStream(text string, final bool) string {
	vault := secret.Default()
	buf := f.secHold + text
	if final {
		f.secHold = ""
		return vault.Redact(buf)
	}
	redacted := vault.Redact(buf)
	hold := vault.StreamHoldLen(redacted)
	if hold <= 0 {
		f.secHold = ""
		return redacted
	}
	if len(redacted) <= hold {
		f.secHold = redacted
		return ""
	}
	cut := backToRuneStart(redacted, len(redacted)-hold)
	if cut <= 0 {
		f.secHold = redacted
		return ""
	}
	f.secHold = redacted[cut:]
	return redacted[:cut]
}

// backToRuneStart 把字节下标 i 往回退到最近的 UTF-8 rune 起始处，避免把一个多字节
// 字符从中间切开推送给前端（往回退只会多扣一点，对遮蔽只会更安全）。
func backToRuneStart(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
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

func isTagNameChar(c byte) bool {
	return isASCIILetter(c) || (c >= '0' && c <= '9') || c == '_' || c == '-'
}
