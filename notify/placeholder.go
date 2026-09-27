package notify

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// bot 模式占位符：模型不再自己重打标识符 / 原文。
//
// 旧做法让模型「逐字抄写」设备名、序列号等，再用 MissingIdentifiers 做事后校验；
// 模型仍会抄错（/dev/md/md-test → /dev/md-md-test），校验本身也会误报
// （65536KB 这类「数字+单位」被当成序列号）。现在：
//   - BuildPlaceholders 从通知里取出标识符与原文，编成占位符表（{{device}}、{{serial}}、
//     {{host}}、{{ip}}、{{raw}}、{{title}}、{{source}}、{{time}}，以及逐个编号的 {{id1}}…），
//     随通知数据一起交给模型；
//   - 模型在文中只写占位符，FillPlaceholders 用原文里的准确内容替换（单遍替换，
//     替换进去的外部文本不会被再次解析）；
//   - 占位符写错（未知名字、括号不成对）或标识符既没用占位符也没逐字写出时，
//     ComposeBot 仍附原文块兜底。
// ============================================================================

// Placeholders 是一条通知的占位符表。Keys 保持稳定顺序（用于 prompt），Values 以小写键索引。
type Placeholders struct {
	Keys   []string
	Values map[string]string
}

var (
	// 显式标注的序列号：「S/N: xxx」「Serial Number: xxx」「serial=xxx」。
	serialLabelRE = regexp.MustCompile(`(?i)(?:s/n|serial(?:[ _-]?(?:number|no\.?))?)\s*[:=]\s*([A-Za-z0-9][A-Za-z0-9_.-]{2,})`)
	// {{name}}（允许括号内两侧空白，名字大小写不敏感）。
	placeholderRE = regexp.MustCompile(`\{\{\s*([A-Za-z][A-Za-z0-9_]*)\s*\}\}`)
	// 单花括号的已知占位符（模型常见笔误）：{device}、{id3}。
	singleBracePlaceholderRE = regexp.MustCompile(`\{\s*(device|serial|host|ip|raw|title|source|time|id[0-9]+)\s*\}`)
)

// placeholderUnresolved 替换未知占位符的文本：原文块会随消息附上，提示主人去看原文。
const placeholderUnresolved = "（见下方原文）"

// BuildPlaceholders 为通知构造占位符表。
func BuildPlaceholders(n Notification, loc *time.Location) Placeholders {
	p := Placeholders{Values: map[string]string{}}
	set := func(k, v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if _, dup := p.Values[k]; dup {
			return
		}
		p.Keys = append(p.Keys, k)
		p.Values[k] = v
	}
	src := n.Title + "\n" + n.Body

	// 语义别名：只在能明确判定时给出，模型能在表里看到对应的值。
	for _, tok := range identPathRE.FindAllString(src, -1) {
		if strings.HasPrefix(tok, "/dev/") {
			set("device", tok)
			break
		}
	}
	if _, ok := p.Values["device"]; !ok {
		if m := identMDRE.FindString(src); m != "" {
			set("device", m)
		}
	}
	// 取第一个含数字的标注值：「model/serial: WDC WD40…, S/N:WD-WCC7…」里的「WDC」是厂商名，不是序列号。
	for _, m := range serialLabelRE.FindAllStringSubmatch(src, -1) {
		if v := strings.TrimRight(m[1], ".,;"); strings.ContainsAny(v, "0123456789") {
			set("serial", v)
			break
		}
	}
	if m := identHostLineRE.FindStringSubmatch(src); m != nil {
		set("host", m[1])
	} else if m := identFQDNRE.FindString(src); m != "" {
		set("host", m)
	}
	if m := identIPv4RE.FindString(src); m != "" {
		set("ip", m)
	} else if m := identIPv6RE.FindString(src); m != "" {
		set("ip", m)
	}

	for i, id := range ExtractIdentifiers(n) {
		set("id"+strconv.Itoa(i+1), id)
	}
	set("source", n.Source)
	set("title", n.Title)
	raw := n.Body
	if strings.TrimSpace(raw) == "" {
		raw = n.Title
	}
	set("raw", raw)
	if !n.At.IsZero() {
		set("time", n.At.In(locOr(loc)).Format("2006-01-02 15:04:05 MST"))
	}
	return p
}

// PromptMap 返回给模型看的占位符表：{"{{device}}": "/dev/md/md-test", ...}。
// {{raw}} 只给出提示而不重复整段正文（正文已在 notification_data.body 中）。
func (p Placeholders) PromptMap() map[string]string {
	out := make(map[string]string, len(p.Keys))
	for _, k := range p.Keys {
		v := p.Values[k]
		if k == "raw" {
			v = "(the full original body, verbatim)"
		}
		out["{{"+k+"}}"] = v
	}
	return out
}

// FillResult 描述一次占位符替换。
type FillResult struct {
	Text      string
	Used      []string // 成功替换的占位符名（去重，按首次出现）
	Unknown   []string // 表里没有的占位符名
	Malformed bool     // 括号不成对、单花括号等写法
	RawUsed   bool     // 使用了 {{raw}}（正文已逐字进入消息）
}

// FillPlaceholders 把 text 中的占位符替换为原文里的准确内容。单遍替换：替换进去的
// 外部文本（可能本身含有 {{…}}）不会被再次解析。
func FillPlaceholders(text string, p Placeholders) FillResult {
	var r FillResult
	seen := map[string]bool{}
	lookup := func(name string) (string, bool) {
		key := strings.ToLower(name)
		v, ok := p.Values[key]
		if ok && !seen[key] {
			seen[key] = true
			r.Used = append(r.Used, key)
			if key == "raw" {
				r.RawUsed = true
			}
		}
		return v, ok
	}

	// 先检查括号是否成对：去掉合法占位符后若仍残留 {{ 或 }}，视为写坏了。
	rest := placeholderRE.ReplaceAllString(text, "")
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		r.Malformed = true
	}

	// 单花括号的已知占位符：能替换就替换，但记为 malformed（附原文块兜底）。
	// 先于双花括号处理会误伤 {{x}} 的内层，因此只在 rest（已剔除合法占位符）里有命中时才做。
	singles := singleBracePlaceholderRE.MatchString(rest)

	var b strings.Builder
	last := 0
	for _, m := range placeholderRE.FindAllStringSubmatchIndex(text, -1) {
		seg := text[last:m[0]]
		if singles {
			seg = fillSingleBrace(seg, lookup, &r)
		}
		b.WriteString(seg)
		name := text[m[2]:m[3]]
		if v, ok := lookup(name); ok {
			b.WriteString(v)
		} else {
			r.Unknown = append(r.Unknown, name)
			b.WriteString(placeholderUnresolved)
		}
		last = m[1]
	}
	seg := text[last:]
	if singles {
		seg = fillSingleBrace(seg, lookup, &r)
	}
	b.WriteString(seg)
	r.Text = b.String()
	return r
}

func fillSingleBrace(seg string, lookup func(string) (string, bool), r *FillResult) string {
	return singleBracePlaceholderRE.ReplaceAllStringFunc(seg, func(m string) string {
		name := singleBracePlaceholderRE.FindStringSubmatch(m)[1]
		v, ok := lookup(name)
		if !ok {
			return m
		}
		r.Malformed = true
		return v
	})
}
