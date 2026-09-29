package strutil

import (
	"regexp"
	"strings"
)

// 本文件集中处理「渠道层为 LLM prompt 注入的装饰标记」。
//
// 背景（2026-09-29 排查实证）：Misskey 渠道把回复/转发上下文、来源标注、note_id
// 等一律拼进 core.Message.Text（channel/misskey/channel.go 的 noteContext 与
// handleNote），而不是放进 InjectContext。这些内容是给模型看的上下文，不是用户原话。
//
// 若直接把带装饰的 Text 当作「用户说了什么」写入长期记忆或会话标题，会造成两类污染：
//  1. 说话人归属错误：被回复帖的正文（Bot 自己的话居多）被记成用户的话。
//     历史 bug 即源于此——「Bot 对《零之使魔》的安利」被记成「用户熟悉该作」。
//     ⚠️ 旧注释曾声称 "[Reply to 栞娜: ...]" 是帖子正文的一部分、必须保留，
//     这是**错误认知**：该格式由 noteContext 生成，不是 Misskey 的渲染产物。
//     防护写在了正确位置却因这条注释而长期未被启用，故此处显式澄清。
//  2. 标题被吃满：titleFromFirstMessage 只截前 30 rune，而 "[Reply to 栞娜: " 就
//     占 14 字，导致用户的话一个字都进不了会话标题。
//
// 因此凡是「要当作用户原话使用」的场景（记忆捕获、事件流、会话标题）都必须先过一遍
// StripChannelContextMarkers；而 LLM prompt 组装**不能**调用它（模型需要这些上下文）。

// 注入顺序（外 → 内，见 channel/misskey/channel.go:876 之后的处理）：
//
//	[对方是 Bot 账号 u] [Timeline] @u: [Renote from Y: r]\n[Reply to X: q]\n<用户正文>\n[note_id: n]
//
// 剥离必须按此顺序由外向内进行。
var (
	reBotAccountPrefix = regexp.MustCompile(`^\[对方是 Bot 账号 [^\]]+\]\s*`)
	reTimelinePrefix   = regexp.MustCompile(`^\[Timeline\]\s*@\S+:\s*`)
	reDMPrefix         = regexp.MustCompile(`^\[DM\]\s*@\S+:\s*`)
	// Renote / Reply 的 quoted 正文是截断到 200 rune 的被回复帖原文，可能含换行与 ']'，
	// 故用 (?s) 非贪婪匹配到「第一个以 ']' 收尾且其后紧跟换行」的位置——这正是注入格式
	// 的边界。若 quoted 内出现 "]\n" 会提前截断（保守残留），但不会出现更糟的
	// 「把用户正文连同前缀一起吞掉」或「前缀残留污染记忆」。
	reRenoteContext = regexp.MustCompile(`(?s)^\[Renote from [^\]]+: .*?\]\s*?(\r?\n|$)`)
	reReplyContext  = regexp.MustCompile(`(?s)^\[Reply to [^\]]+: .*?\]\s*?(\r?\n|$)`)
	reNoteIDSuffix  = regexp.MustCompile(`(?m)\s*\[note_id: [^\]]+\]\s*$`)
)

// maxStripPasses 限制剥离轮数，避免病态输入下无谓循环。
// 正常一轮即可收敛（Reply 与 Renote 各一层），留余量应对嵌套。
const maxStripPasses = 4

// stripOnce 按注入顺序由外向内剥离一遍装饰标记。
func stripOnce(s string) string {
	s = reBotAccountPrefix.ReplaceAllString(s, "")
	s = reTimelinePrefix.ReplaceAllString(s, "")
	s = reDMPrefix.ReplaceAllString(s, "")
	s = reRenoteContext.ReplaceAllString(s, "")
	s = reReplyContext.ReplaceAllString(s, "")
	s = reNoteIDSuffix.ReplaceAllString(s, "")
	return s
}

// StripChannelContextMarkers 去除文本里渠道注入的装饰前缀/后缀，返回用户原文。
//
// 幂等：对已清洗过的文本再次调用结果不变。空输入返回空串。
// 只剥离「已知的固定装饰格式」，匹配不到时原样返回（保守，宁可残留也不误删用户内容）。
func StripChannelContextMarkers(raw string) string {
	s := raw
	if s == "" {
		return ""
	}
	for i := 0; i < maxStripPasses; i++ {
		before := s
		s = stripOnce(s)
		if s == before {
			break
		}
	}
	return strings.TrimSpace(s)
}
