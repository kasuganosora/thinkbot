package memory

import (
	"regexp"
	"strings"
)

// embedVersion identifies the text normalization + hashing scheme used for
// memory_vec. Bump it whenever hashEmbed's output changes; OpenVecIndex then
// drops the stale vectors and the startup backfill rewrites them.
const embedVersion = "bigram-norm-v1"

// Boilerplate that every note/message shares (mentions, ids, timestamps, URLs,
// reply prefixes) dominated the character-bigram vectors, so unrelated entries
// scored 0.6+ against each other. normalizeForEmbed strips it before hashing.
var (
	embedReURL     = regexp.MustCompile(`(?i)\b(?:https?|wss?)://\S+|\bwww\.\S+`)
	embedReReply   = regexp.MustCompile(`(?i)[\[【(（]\s*(?:reply(?:ing)?\s+to|in\s+reply\s+to|re:|回复|返信|ответ)[^\]】)）]*[\]】)）]`)
	embedReMention = regexp.MustCompile(`@[\p{L}\p{N}_.\-]+(?:@[\p{L}\p{N}_\-]+(?:\.[\p{L}\p{N}_\-]+)+)?`)
	embedReDate    = regexp.MustCompile(`\b\d{4}[-/.]\d{1,2}[-/.]\d{1,2}(?:[T ]\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?)?\b|\d{4}年\d{1,2}月\d{1,2}日`)
	embedReTime    = regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:\s*(?:Z|[+-]\d{2}:?\d{2}|UTC))?\b`)
	embedReLongNum = regexp.MustCompile(`\b\d{8,}\b`)
	// Misskey aid/aidx (10/16 chars: base36 time + random) and similar ids:
	// lowercase alnum tokens that mix letters and digits.
	embedReAid    = regexp.MustCompile(`\b[0-9a-z]{10}(?:[0-9a-z]{6})?\b`)
	embedReHex    = regexp.MustCompile(`(?i)\b[0-9a-f]{24,}\b|\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	embedReDigit  = regexp.MustCompile(`[0-9]`)
	embedReLetter = regexp.MustCompile(`[a-z]`)
)

// normalizeForEmbed removes boilerplate before hashing. It is applied to
// writes, queries and EmbedSimilarity alike. Empty result falls back to text.
func normalizeForEmbed(text string) string {
	s := embedReURL.ReplaceAllString(text, " ")
	s = embedReReply.ReplaceAllString(s, " ")
	s = embedReMention.ReplaceAllString(s, " ")
	s = embedReHex.ReplaceAllString(s, " ")
	s = embedReDate.ReplaceAllString(s, " ")
	s = embedReTime.ReplaceAllString(s, " ")
	s = embedReLongNum.ReplaceAllString(s, " ")
	s = embedReAid.ReplaceAllStringFunc(s, func(tok string) string {
		if embedReDigit.MatchString(tok) && embedReLetter.MatchString(tok) {
			return " "
		}
		return tok
	})
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return strings.Join(strings.Fields(text), " ")
	}
	return s
}
