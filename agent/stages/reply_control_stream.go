package stages

import (
	"strings"
	"unicode"
)

// ReplyControlStreamFilter 在流式增量里剥离 reply-control 控制块
// （@@REPLY_CONTROL@@{"send": ...}），保证协议标记永远不会出现在推给前端的 delta 里。
//
// 为什么不能逐 delta 调 StripReplyControlBlock：模型按 token 流式输出，控制块几乎
// 总是被切成多段（"@@" / "REPLY" / "_CONTROL" / "@@{\"" / "send" / "\": true}"），
// 单个 delta 里既找不到完整分隔符、也拼不出完整 JSON，逐段剥离形同虚设，
// 前端就会看到 @@REPLY_CONTROL@@{"send": true}（最终 done 事件用全文剥离，所以
// 落定后的消息是干净的，只有流式过程中可见）。
//
// 做法：
//   - 普通状态下，只扣住「可能是分隔符前缀」的尾部（以及它前面的空白，避免控制块前
//     的换行残留在正文末尾），其余文本立即放行，不增加可感知的延迟。
//   - 一旦凑出完整分隔符，进入控制状态：吞掉分隔符之后的代码围栏 / 空白与第一个
//     完整 JSON 对象（按括号深度与字符串转义跟踪），再吞掉紧随的空白与收尾围栏，
//     随后恢复普通状态（极少数情况下控制块后还有正文，照常放行）。
//
// 只影响「推给前端的流」；是否出站仍由完整原文经 parseReplyControl 裁决，控制信号照常生效。
// 非并发安全，每条流一个实例。
type ReplyControlStreamFilter struct {
	state   rcState
	pending string // 普通状态下扣住的尾部（空白 + 可能的分隔符前缀）
	depth   int    // JSON 括号深度（控制状态）
	inStr   bool   // 是否在 JSON 字符串内
	escape  bool   // 字符串内上一个字符是否为反斜杠
}

type rcState int

const (
	rcNormal  rcState = iota
	rcPreJSON         // 已见分隔符，等待 JSON 起始 '{'
	rcInJSON          // 在控制 JSON 对象内
	rcPost            // JSON 已闭合，吞掉紧随的空白 / 收尾围栏
)

// Feed 输入一段增量，返回可以立即安全发出的文本（可能为空）。
func (f *ReplyControlStreamFilter) Feed(delta string) string {
	if delta == "" {
		return ""
	}
	var out strings.Builder
	f.feed(delta, &out)
	return out.String()
}

// Flush 在流结束（或被工具调用等非文本事件打断）时调用，返回仍扣住的文本。
// 扣住的只可能是「看起来像分隔符开头但最终没凑齐」的普通正文，原样放行；
// 未闭合的控制块则直接丢弃。调用后过滤器回到初始状态。
func (f *ReplyControlStreamFilter) Flush() string {
	out := ""
	if f.state == rcNormal {
		out = f.pending
	}
	*f = ReplyControlStreamFilter{}
	return out
}

func (f *ReplyControlStreamFilter) feed(s string, out *strings.Builder) {
	for s != "" {
		switch f.state {
		case rcNormal:
			buf := f.pending + s
			s = ""
			if idx := strings.Index(buf, replyControlDelimiter); idx >= 0 {
				out.WriteString(strings.TrimRightFunc(buf[:idx], unicode.IsSpace))
				f.pending = ""
				f.state = rcPreJSON
				s = buf[idx+len(replyControlDelimiter):]
				continue
			}
			cut := holdBackIndex(buf)
			out.WriteString(buf[:cut])
			f.pending = buf[cut:]

		case rcPreJSON:
			i := strings.IndexByte(s, '{')
			if i < 0 {
				// 围栏 / 语言标记 / 空白：全部吞掉，继续等 '{'
				return
			}
			f.state = rcInJSON
			f.depth, f.inStr, f.escape = 0, false, false
			s = s[i:]

		case rcInJSON:
			end := -1
			for i := 0; i < len(s); i++ {
				c := s[i]
				if f.inStr {
					switch {
					case f.escape:
						f.escape = false
					case c == '\\':
						f.escape = true
					case c == '"':
						f.inStr = false
					}
					continue
				}
				switch c {
				case '"':
					f.inStr = true
				case '{':
					f.depth++
				case '}':
					f.depth--
					if f.depth == 0 {
						end = i
					}
				}
				if end >= 0 {
					break
				}
			}
			if end < 0 {
				return
			}
			f.state = rcPost
			s = s[end+1:]

		case rcPost:
			i := strings.IndexFunc(s, func(r rune) bool { return !unicode.IsSpace(r) && r != '`' })
			if i < 0 {
				return
			}
			f.state = rcNormal
			s = s[i:]
		}
	}
}

// holdBackIndex 返回 buf 中应扣住部分的起点：最长的「是分隔符真前缀」的后缀，
// 连同它前面紧邻的空白一起扣住。其余部分可以安全发出。
func holdBackIndex(buf string) int {
	cut := len(buf)
	maxK := len(replyControlDelimiter) - 1
	if maxK > len(buf) {
		maxK = len(buf)
	}
	for k := maxK; k > 0; k-- {
		if strings.HasPrefix(replyControlDelimiter, buf[len(buf)-k:]) {
			cut = len(buf) - k
			break
		}
	}
	// 连同前面的空白一起扣住：若随后确实是控制块，这些空白不应出现在正文末尾。
	trimmed := strings.TrimRightFunc(buf[:cut], unicode.IsSpace)
	return len(trimmed)
}
