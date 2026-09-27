package telegram

import (
	"context"
	"html"
	"regexp"
	"strings"

	"github.com/kasuganosora/thinkbot/util/traceid"
)

// Telegram's HTML parse mode rejects the WHOLE message (400 "can't parse
// entities") when the text contains a "<", ">" or "&" that is not part of a
// supported tag or entity. Model replies routinely contain such characters
// ("a<b", "<3", "->", "R&D", or a stray "<long>" wrapper), and before this
// the reply was simply lost (seen 2026-09-27 18:45:37: `Unsupported start tag
// "" at byte offset 194`). Two layers of defence:
//
//  1. sanitizeTelegramHTML escapes every "<", ">" and "&" that is not part of
//     a tag/entity Telegram supports, before the text is sent.
//  2. If Telegram still answers with a parse error (e.g. an unbalanced <b>),
//     the message is resent once as plain text (tags stripped, entities
//     decoded), so the user always gets the content.

// telegramHTMLTag matches one tag Telegram's HTML mode supports, anchored at
// the start of the input. Attributes are allowed but may not contain "<" or ">".
var telegramHTMLTag = regexp.MustCompile(`^</?(?i:b|strong|i|em|u|ins|s|strike|del|code|pre|blockquote|tg-spoiler|tg-emoji|a|span)(?:\s[^<>]*)?>`)

// telegramHTMLEntity matches one entity Telegram's HTML mode supports,
// anchored at the start of the input.
var telegramHTMLEntity = regexp.MustCompile(`^&(?:lt|gt|amp|quot|#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6});`)

// sanitizeTelegramHTML escapes characters that would make Telegram reject an
// HTML-mode message while keeping supported tags and entities intact.
func sanitizeTelegramHTML(s string) string {
	if !strings.ContainsAny(s, "<>&") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		switch s[i] {
		case '<':
			if m := telegramHTMLTag.FindString(s[i:]); m != "" {
				b.WriteString(m)
				i += len(m)
				continue
			}
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			if m := telegramHTMLEntity.FindString(s[i:]); m != "" {
				b.WriteString(m)
				i += len(m)
				continue
			}
			b.WriteString("&amp;")
		default:
			b.WriteByte(s[i])
		}
		i++
	}
	return b.String()
}

// anyTelegramHTMLTag matches supported tags anywhere (for stripping).
var anyTelegramHTMLTag = regexp.MustCompile(`</?(?i:b|strong|i|em|u|ins|s|strike|del|code|pre|blockquote|tg-spoiler|tg-emoji|a|span)(?:\s[^<>]*)?>`)

// telegramHTMLToPlain turns (sanitized) HTML-mode text into plain text for
// the fallback resend: supported tags are removed and entities decoded.
func telegramHTMLToPlain(s string) string {
	return html.UnescapeString(anyTelegramHTMLTag.ReplaceAllString(s, ""))
}

// prepareOutboundText applies parse-mode specific preparation.
func prepareOutboundText(text, parseMode string) string {
	if strings.EqualFold(parseMode, "HTML") {
		return sanitizeTelegramHTML(text)
	}
	return text
}

// plainFallbackText returns the text to send when Telegram rejected the
// formatted version.
func plainFallbackText(text, parseMode string) string {
	if strings.EqualFold(parseMode, "HTML") {
		return telegramHTMLToPlain(text)
	}
	return text
}

// isParseEntitiesError reports whether err is Telegram rejecting the message
// formatting ("can't parse entities" / "can't parse message text" /
// "can't find end of ..."), as opposed to any other failure.
func isParseEntitiesError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "400") && !strings.Contains(msg, "bad request") {
		return false
	}
	return strings.Contains(msg, "can't parse") ||
		strings.Contains(msg, "can't find end") ||
		strings.Contains(msg, "unsupported start tag") ||
		strings.Contains(msg, "unexpected end tag")
}

// logPlainFallback records that a formatted message was resent as plain text.
func logPlainFallback(ctx context.Context, method, parseMode string, err error) {
	if lg := traceid.L(ctx); lg != nil {
		lg.Warnw("telegram rejected formatted text, resending as plain text",
			"method", method, "parse_mode", parseMode, "err", err)
	}
}
