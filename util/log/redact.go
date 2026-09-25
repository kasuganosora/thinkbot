package log

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap/zapcore"
)

// ============================================================================
// Secret redaction for log output
//
// Last line of defence: every log output (stdout/stderr/file cores built by
// InitWithConfig) is wrapped in a redacting WriteSyncer, so a credential that
// slips into a message, a field or an error string (e.g. Go's *url.Error embeds
// the full request URL, and Telegram puts the bot token in the URL path) is
// masked before it reaches the terminal or thinkbot.log. Call sites should
// still sanitize at the source (util/http.SanitizeURL / SanitizeError); this
// layer only guarantees that a miss does not end up on disk.
//
// Redaction works on the encoded entry (one Write per entry), so it covers the
// message, every field, errorVerbose and stack traces regardless of the field
// type.
// ============================================================================

// RedactedPlaceholder replaces secret material in logs.
const RedactedPlaceholder = "***"

type redactRule struct {
	re   *regexp.Regexp
	repl []byte
	// hint is a cheap substring pre-check; the regexp only runs when the
	// entry contains it (case-insensitive hints are lower-case and checked
	// against a lower-cased copy).
	hint     string
	foldCase bool
}

var redactRules = []redactRule{
	// Telegram bot token inside an API URL path: /bot<id>:<secret>/...
	{re: regexp.MustCompile(`bot[0-9]{5,}:[A-Za-z0-9_-]{20,}`), repl: []byte("bot" + RedactedPlaceholder), hint: "bot"},
	// Bare Telegram bot token (<id>:<secret>).
	{re: regexp.MustCompile(`\b[0-9]{6,12}:[A-Za-z0-9_-]{30,}`), repl: []byte(RedactedPlaceholder), hint: ":"},
	// Authorization: Bearer <token>
	{re: regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{12,}`), repl: []byte("${1}" + RedactedPlaceholder), hint: "bearer", foldCase: true},
	// Credentials in query strings (?i= is the Misskey token, key= Google API keys).
	{re: regexp.MustCompile(`(?i)([?&](?:i|token|access_token|api_key|apikey|key|secret|password)=)[^&\s"'\\]+`), repl: []byte("${1}" + RedactedPlaceholder), hint: "=", foldCase: false},
}

// minRegisteredSecretLen avoids masking short, common strings by accident.
const minRegisteredSecretLen = 12

var (
	secretsMu sync.RWMutex
	secrets   []string // sorted longest first
)

// RegisterSecret adds an exact secret value (e.g. a bot token loaded from
// config) that must never appear in logs. Values shorter than 12 characters
// are ignored to avoid masking ordinary text. Safe for concurrent use.
func RegisterSecret(s string) {
	s = strings.TrimSpace(s)
	if len(s) < minRegisteredSecretLen {
		return
	}
	secretsMu.Lock()
	defer secretsMu.Unlock()
	for _, v := range secrets {
		if v == s {
			return
		}
	}
	secrets = append(secrets, s)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
}

// RedactSecrets masks known secret patterns and registered secret values in s.
func RedactSecrets(s string) string {
	if s == "" {
		return s
	}
	out := redactBytes([]byte(s))
	return string(out)
}

// redactBytes returns p with secrets masked. When nothing matches, p itself
// is returned (no copy).
func redactBytes(p []byte) []byte {
	out := p
	secretsMu.RLock()
	for _, sec := range secrets {
		if bytes.Contains(out, []byte(sec)) {
			out = bytes.ReplaceAll(out, []byte(sec), []byte(RedactedPlaceholder))
		}
	}
	secretsMu.RUnlock()
	var lower []byte
	for _, r := range redactRules {
		if r.foldCase {
			if lower == nil {
				lower = bytes.ToLower(out)
			}
			if !bytes.Contains(lower, []byte(r.hint)) {
				continue
			}
		} else if !bytes.Contains(out, []byte(r.hint)) {
			continue
		}
		if r.re.Match(out) {
			out = r.re.ReplaceAll(out, r.repl)
			lower = nil
		}
	}
	return out
}

// redactingWriteSyncer masks secrets in every encoded log entry.
type redactingWriteSyncer struct {
	zapcore.WriteSyncer
}

// NewRedactingWriteSyncer wraps ws so that secrets are masked before writing.
func NewRedactingWriteSyncer(ws zapcore.WriteSyncer) zapcore.WriteSyncer {
	if ws == nil {
		return nil
	}
	if _, ok := ws.(*redactingWriteSyncer); ok {
		return ws
	}
	return &redactingWriteSyncer{WriteSyncer: ws}
}

func (w *redactingWriteSyncer) Write(p []byte) (int, error) {
	out := redactBytes(p)
	if _, err := w.WriteSyncer.Write(out); err != nil {
		return 0, err
	}
	// Report the caller's length: zap treats short writes as errors.
	return len(p), nil
}
