package mastodon

import (
	"html"
	"regexp"
	"strings"
)

// ============================================================================
// HTML → 纯文本
// ============================================================================
// Mastodon 的 Status.Content 是服务端渲染后的 HTML（Misskey 是纯文本），
// 直接塞进 core.Message.Text 会让 LLM 和用户看到 "<p>hello</p>" 这种垃圾。
// 本文件把 HTML 降级为可读纯文本，规则与 Mastodon 客户端的「纯文本视图」一致。

var (
	// brRe 匹配 <br> / <br/> / <br />，一律换成换行。
	brRe = regexp.MustCompile(`(?i)<br\s*/?>`)
	// blockCloseRe 匹配块级元素的闭合标签，闭合后需要换行分段。
	blockCloseRe = regexp.MustCompile(`(?i)</(p|div|li|blockquote|h[1-6]|ul|ol|pre|tr)>`)
	// tagRe 兜底删除所有剩余标签（含 <a>、<span class="h-card"> 等）。
	// mention 的 @acct 是标签的**文本节点**，删除标签后自然保留。
	tagRe = regexp.MustCompile(`<[^>]*>`)
	// multiNewlineRe 压缩连续空行，避免一堆 </p><p></p> 产生大片空白。
	multiNewlineRe = regexp.MustCompile(`\n{3,}`)
	// trailingWSRe 去掉每行尾部的空白，保持 prompt 紧凑。
	trailingWSRe = regexp.MustCompile(`[ \t]+$`)
)

// htmlToText 把 Mastodon 的 HTML 正文转成纯文本。
//
// 处理顺序很关键：先补换行（块级闭合 / <br>），再删标签，最后反转义实体。
// 先反转义会把 `&lt;p&gt;` 之类的转义文本误变成真标签，造成内容丢失。
//
// 返回已 TrimSpace 的文本；空输入返回空串。
func htmlToText(s string) string {
	if s == "" {
		return ""
	}
	t := brRe.ReplaceAllString(s, "\n")
	t = blockCloseRe.ReplaceAllString(t, "\n")
	t = tagRe.ReplaceAllString(t, "")
	t = html.UnescapeString(t)

	// 逐行去掉尾部空白，并压缩连续空行。
	lines := strings.Split(t, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, trailingWSRe.ReplaceAllString(ln, ""))
	}
	t = strings.Join(out, "\n")
	t = multiNewlineRe.ReplaceAllString(t, "\n\n")
	return strings.TrimSpace(t)
}

// isBlankContent 判断内容在转成纯文本后是否为空（纯 boost / 纯媒体帖）。
func isBlankContent(s string) bool {
	return htmlToText(s) == ""
}
