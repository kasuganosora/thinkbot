package memory

import (
	"regexp"
	"strings"
)

// embedVersion identifies the text normalization + hashing scheme used for
// memory_vec. Bump it whenever hashEmbed's output changes; OpenVecIndex then
// drops the stale vectors and the startup backfill rewrites them.
const embedVersion = "bigram-norm-v3"

// Boilerplate that every note/message shares (mentions, ids, timestamps, URLs,
// reply prefixes, bracket tags) dominated the character-bigram vectors, so
// unrelated entries scored 0.6+ against each other. Memory-extraction
// summaries carry it mid-text, so everything except the quote segment and the
// author prefix is stripped anywhere in the text.
const (
	embedHandle = `@[A-Za-z0-9_][A-Za-z0-9_.\-]*(?:@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)+)?`
)

var (
	embedReURL = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s\]】)）]+|\bwww\.[^\s\]】)）]+`)
	// Whole quote segments: "[Reply to Name: @handle quoted text…]" (may span
	// lines, holds parentheses), plus 【回复 …】 / (in reply to …) variants.
	embedReReply = regexp.MustCompile(`(?is)^\s*\[\s*(?:in\s+)?reply(?:ing)?\s+to\b.*?\]\s*\n|\[\s*(?:in\s+)?reply(?:ing)?\s+to\b[^\]]*\]|【\s*(?:回复|返信|ответ)[^】]*】|[(（]\s*(?:in\s+reply\s+to|reply\s+to|回复|返信)[^)）]*[)）]`)
	// Author prefix at the start of an entry or line: "@handle", "@handle@host",
	// optionally "（Display Name）" / "(Display Name)" and ":" / "：".
	embedReAuthor = regexp.MustCompile(`(?m)^\s*` + embedHandle + `\s*(?:[(（][^)）\n]{0,40}[)）])?\s*[:：]?`)
	// "回复 @handle：" / "reply to @handle:" inline.
	embedReReplyAt = regexp.MustCompile(`(?i)(?:回复|返信|reply\s+to)\s*` + embedHandle + `\s*[:：]?`)
	// Handles anywhere. ASCII only (Misskey/Mastodon usernames), so CJK text
	// right after "@" is kept.
	embedReMention = regexp.MustCompile(embedHandle)
	// Short bracket tags/labels: 【…记档】, 【日常/杂谈】, [tag]. Up to 20 runes.
	embedReTag = regexp.MustCompile(`【[^】\n]{0,20}】|\[[^\]\n]{1,20}\]`)
	// Id references: "note 9h6zahxdlu", "note(9h6zahxdlu)", "note：…", "notes #…",
	// "status 1122…", 帖子/笔记/ノート + id.
	embedReIDRef   = regexp.MustCompile(`(?i)(?:\b(?:notes?|status(?:es)?|post|toot|id)|帖子|贴文|笔记|ノート)\s*[:：#(（]?\s*([0-9a-z]{6,32})\b\s*[)）]?`)
	embedReDate    = regexp.MustCompile(`\b\d{4}[-/.]\d{1,2}[-/.]\d{1,2}(?:[T ]\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?)?\b|(?:\d{4}\s*年\s*)?\d{1,2}\s*月\s*\d{1,2}\s*[日号]|\d{4}\s*年\s*\d{1,2}\s*月|\b\d{1,2}/\d{1,2}\b`)
	embedReTime    = regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:\s*(?:Z|[+-]\d{2}:?\d{2}|UTC))?\b|\d{1,2}\s*[点時时]\s*\d{1,2}\s*分`)
	embedReLongNum = regexp.MustCompile(`\b\d{8,}\b`)
	// Misskey aid/aidx (10/16 chars) and similar ids: lowercase alnum tokens of
	// 8-32 chars that mix letters and digits.
	embedReAid        = regexp.MustCompile(`\b[0-9a-z]{8,32}\b`)
	embedReHex        = regexp.MustCompile(`(?i)\b[0-9a-f]{24,}\b|\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	embedReDigit      = regexp.MustCompile(`[0-9]`)
	embedReLetter     = regexp.MustCompile(`[a-z]`)
	embedReEmptyParen = regexp.MustCompile(`[(（]\s*[,，、;；]?\s*[)）]`)
)

// normalizeForEmbed removes boilerplate before hashing. It is applied to
// writes, queries and EmbedSimilarity alike. Empty result falls back to text.
func normalizeForEmbed(text string) string {
	s := embedReReply.ReplaceAllString(text, " ")
	s = embedReURL.ReplaceAllString(s, " ")
	s = embedReAuthor.ReplaceAllString(s, " ")
	s = embedReReplyAt.ReplaceAllString(s, " ")
	s = embedReTag.ReplaceAllString(s, " ")
	s = embedReMention.ReplaceAllString(s, " ")
	s = embedReHex.ReplaceAllString(s, " ")
	s = embedReIDRef.ReplaceAllStringFunc(s, func(m string) string {
		// only real ids (contain a digit): "post office" stays.
		if sub := embedReIDRef.FindStringSubmatch(m); len(sub) > 1 && embedReDigit.MatchString(sub[1]) {
			return " "
		}
		return m
	})
	s = embedReDate.ReplaceAllString(s, " ")
	s = embedReTime.ReplaceAllString(s, " ")
	s = embedReLongNum.ReplaceAllString(s, " ")
	s = embedReAid.ReplaceAllStringFunc(s, func(tok string) string {
		if embedReDigit.MatchString(tok) && embedReLetter.MatchString(tok) {
			return " "
		}
		return tok
	})
	s = embedReEmptyParen.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return strings.Join(strings.Fields(text), " ")
	}
	return s
}
