package misskey

import (
	"net/url"
	"regexp"
	"strings"
)

// leadingMentionsRe matches the run of @mentions Misskey clients auto-prefix to
// a reply (the reply target's author plus the participants it mentioned).
var leadingMentionsRe = regexp.MustCompile(`^(?:\s*@[A-Za-z0-9_]+(?:@[A-Za-z0-9.\-]+)?)+\s*`)

// ownHost returns the bare hostname of the bot's own instance ("" if unknown).
func (c *MisskeyChannel) ownHost() string {
	h := strings.TrimSpace(c.cfg.Host)
	if h == "" {
		return ""
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// textMentionsSelf reports whether text contains @botUsername or
// @botUsername@<own host> (a mention of a same-named account on another
// instance does not count).
func (c *MisskeyChannel) textMentionsSelf(text string) bool {
	if c.botUsername == "" || text == "" {
		return false
	}
	own := c.ownHost()
	for _, m := range mentionAnyRe.FindAllStringIndex(text, -1) {
		// must not be glued to a preceding word char (e.g. an e-mail address)
		if m[0] > 0 {
			prev := text[m[0]-1]
			if prev == '_' || prev == '@' || prev == '.' || (prev >= '0' && prev <= '9') || (prev|0x20 >= 'a' && prev|0x20 <= 'z') {
				continue
			}
		}
		tok := text[m[0]+1 : m[1]]
		user, host, _ := strings.Cut(tok, "@")
		if !strings.EqualFold(user, c.botUsername) {
			continue
		}
		if host == "" || (own != "" && strings.EqualFold(host, own)) {
			return true
		}
	}
	return false
}

// explicitlyAddressed reports whether the author explicitly directed the note
// at the bot, as opposed to the bot merely being carried along in the
// mentions of a reply chain. Conservative by design:
//   - a reply to the bot's own note counts;
//   - a non-reply note counts if its text mentions the bot;
//   - in a reply to someone else, the leading @-run is the client's
//     auto-prefix; an @bot there counts only when the bot was NOT already a
//     participant of the parent (so the author must have typed it). An @bot
//     anywhere after the auto-prefix always counts. When the parent note is
//     not hydrated we cannot tell, so the leading run is ignored.
func (c *MisskeyChannel) explicitlyAddressed(note Note) bool {
	if c.botUserID != "" && note.Reply != nil && note.Reply.User.ID == c.botUserID {
		return true
	}
	if note.ReplyID == "" && note.Reply == nil {
		return c.textMentionsSelf(note.Text)
	}
	prefix := leadingMentionsRe.FindString(note.Text)
	if c.textMentionsSelf(note.Text[len(prefix):]) {
		return true
	}
	if prefix == "" || !c.textMentionsSelf(prefix) || note.Reply == nil {
		return false
	}
	return !c.inheritedFrom(note.Reply)
}

// inheritedFrom reports whether a reply to parent would auto-carry the bot.
func (c *MisskeyChannel) inheritedFrom(parent *Note) bool {
	if c.botUserID == "" {
		return true // cannot tell: assume inherited (conservative)
	}
	if parent.User.ID == c.botUserID {
		return true
	}
	for _, id := range parent.Mentions {
		if id == c.botUserID {
			return true
		}
	}
	return c.textMentionsSelf(parent.Text)
}
