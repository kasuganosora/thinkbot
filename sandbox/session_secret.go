package sandbox

// ============================================================================
// Credential masking for interactive PTY sessions
//
// SSH exposed a leak the one-shot exec path never had: a credential typed into
// an echo-ON prompt is echoed by the terminal line discipline into the master
// stream, so the literal value lands in the raw ring, the VT screen, the
// semantic delta, the extracted URLs, the Web-console WS replay and (through
// those) the LLM tool-call history. ssh's own password prompt sets ECHO=0 and
// is safe; `read -p`, sudo-with-echo, PIN dialogs and web forms are not.
//
// The project already owns the credential machinery:
//   * secret_input registers a value and hands the model {{secret:name}}.
//   * llm/orchestrate.go resolves that placeholder at the single tool-input
//     choke point and redacts tool output on the way out.
//
// So there is NO new "secret=true" parameter. The model writes the placeholder
// in sandbox_session_send.text; orchestrate resolves it just before Execute.
// This file adds the two defences that choke point cannot cover:
//   1. ingress masking on the raw PTY byte stream, so no downstream consumer
//      (including the binary WS replay) can ever see a registered value;
//   2. egress redaction on every text field of SessionView / RawChunk, which
//      also covers credentials registered AFTER the bytes were captured.
//
// Masking at ingress rather than at each consumer is deliberate: the VT screen
// and the WS binary replay are fed from the SAME bytes, so masking separately
// would let the two diverge (cursor positions, replay length, seq watermark).
// ============================================================================

import (
	"github.com/kasuganosora/thinkbot/secret"
)

// SecretStream is a stateful, cross-chunk masker for a byte stream. A PTY
// read may split a credential in the middle; a per-chunk Redact would miss both
// halves. Vault.StreamHoldLen tells us exactly how many trailing bytes could
// still be the beginning of a registered value, so only those are withheld.
// Normal text is never delayed (hold = 0).
type SecretStream struct {
	pending []byte
}

// Feed returns the bytes that are safe to emit now. The returned slice is a
// fresh allocation and does not alias chunk.
func (m *SecretStream) Feed(chunk []byte) []byte {
	vault := secret.Default()
	if !vault.HasValues() {
		// Fast path: no registered credentials, keep the stream bit-exact.
		if len(m.pending) == 0 {
			return chunk
		}
		p := m.pending
		m.pending = nil
		return append(p, chunk...)
	}
	buf := make([]byte, 0, len(m.pending)+len(chunk))
	buf = append(buf, m.pending...)
	buf = append(buf, chunk...)
	m.pending = nil

	safe := []byte(vault.Redact(string(buf)))
	if hold := vault.StreamHoldLen(string(safe)); hold > 0 {
		if hold > len(safe) {
			hold = len(safe)
		}
		m.pending = append([]byte(nil), safe[len(safe)-hold:]...)
		safe = safe[:len(safe)-hold]
	}
	return safe
}

// Flush is called when the byte source is done. Anything still withheld is by
// construction a proper prefix of a registered credential, so emitting it
// verbatim would leak partial credential material: replace it with a marker.
func (m *SecretStream) Flush() []byte {
	if len(m.pending) == 0 {
		return nil
	}
	m.pending = nil
	return []byte("***")
}

// redactSessionText masks values registered after the bytes were captured.
func redactSessionText(s string) string {
	if s == "" {
		return s
	}
	vault := secret.Default()
	if !vault.HasValues() {
		return s
	}
	return vault.Redact(s)
}

// redactSessionView applies egress redaction to every field derived from the
// terminal stream. Server-generated strings (Hint / Reason / Backend / health
// Evidence) are deliberately left alone: they contain no terminal bytes, and
// Hint itself quotes the {{secret:name}} syntax which a short registered value
// could otherwise corrupt via bounded substring matching.
func redactSessionView(v *SessionView) {
	if v == nil {
		return
	}
	vault := secret.Default()
	if !vault.HasValues() {
		return
	}
	v.Delta = vault.Redact(v.Delta)
	v.Screen = vault.Redact(v.Screen)
	v.StderrTail = vault.Redact(v.StderrTail)
	v.BootDiag = vault.Redact(v.BootDiag)
	for i := range v.URLs {
		v.URLs[i] = vault.Redact(v.URLs[i])
	}
}

// redactRawChunk is the egress counterpart for the Web-console replay. Ingress
// masking already removed values known at capture time; this catches values
// registered later. A value split across two RawSince ticks is NOT recoverable
// here -- that case is covered by ingress masking at capture time.
func redactRawChunk(c RawChunk) RawChunk {
	vault := secret.Default()
	if len(c.Data) > 0 && vault.HasValues() {
		c.Data = []byte(vault.Redact(string(c.Data)))
	}
	return c
}
