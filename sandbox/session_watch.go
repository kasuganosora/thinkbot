package sandbox

// ============================================================================
// sandbox_session_watch long poll — event-driven attention wake-up
//
// First release was pure pull: the agent had to call watch repeatedly with a
// self-chosen sleep. This adds an optional wait so a single tool call blocks
// until something actually needs a decision, then returns the SAME read-only
// attention queue. docs §3.7 / §11 P2.
//
// Hard boundaries (these are the reasons this is a tool-level long poll and
// NOT an out-of-turn wake-up):
//
//   - NO orchestrator change. llm/orchestrate.go's runTool stays synchronous;
//     this is just one tool call that happens to block for a while.
//   - NO toolperm bypass. The call itself already went through the permission
//     evaluator's call-time re-check. We never invoke another tool, never
//     synthesize a tool result, and never start a new agent turn. The model
//     receives attention items and decides what to do through the ordinary,
//     permission-checked poll/send/close tools.
//   - SSE is used only as a heartbeat channel (SendProgress), mirroring how
//     sandbox_exec already streams progress. No message injection.
//   - Strictly read-only: watchSnapshot does not Touch, does not consume
//     deltas, does not advance any per-agent cursor.
// ============================================================================

import (
	"context"
	"strings"
	"time"
)

const (
	watchHeartbeatInterval = 2 * time.Second
	watchMinWait           = 50 * time.Millisecond
)

// attentionSignature is a cheap change detector. It intentionally ignores raw
// seq: a spinner repainting forever must not wake the agent every frame. Only
// things that can change a DECISION are included (priority, verdict, state,
// orphaned, and whether a settled delta is waiting to be read).
func attentionSignature(w WatchResult) string {
	var b strings.Builder
	for _, it := range w.Attention {
		b.WriteString(it.SessionID)
		b.WriteByte(':')
		b.WriteString(it.State)
		b.WriteByte(':')
		b.WriteString(it.Verdict)
		b.WriteByte(':')
		b.WriteByte(byte('0' + it.Priority))
		if it.Orphaned {
			b.WriteByte('o')
		}
		b.WriteByte('|')
	}
	b.WriteByte('#')
	b.WriteString(itoa(w.Total))
	b.WriteByte(':')
	b.WriteString(itoa(w.Quiet))
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// hasDecisionWake reports whether the queue contains something the agent
// should act on now: P0 (actionable / stuck / daemon fault) or P1 (exited /
// idle prompt / orphaned). Quiet P2 progress alone is not a wake-up.
func hasDecisionWake(w WatchResult) bool {
	for _, it := range w.Attention {
		if it.Priority <= 1 {
			return true
		}
	}
	return false
}

// WaitForAttention blocks until the attention queue changes (or contains a
// decision item), wait elapses, or ctx is cancelled. onHeartbeat, when set,
// is called on a slow ticker so the caller can push an SSE heartbeat through
// the existing SendProgress channel.
func (r *SessionRegistry) WaitForAttention(ctx context.Context, botID, agent string,
	limit int, includeQuiet bool, wait time.Duration,
	onHeartbeat func(elapsedMs int64)) WatchResult {

	start := time.Now()
	base := r.watchSnapshot(botID, agent, limit, includeQuiet, 0)
	if wait < watchMinWait || r.cfg.WatchWaitMax < 0 {
		base.WakeReason = "nowait"
		base.Note = joinNote(base.Note, "long-poll disabled; instant snapshot")
		return base
	}
	maxWait := r.cfg.WatchWaitMax
	if maxWait == 0 {
		// Zero-valued SessionConfig (tests / direct construction) must not
		// silently clamp every wait to zero.
		maxWait = DefaultSessionConfig().WatchWaitMax
	}
	if wait > maxWait {
		wait = maxWait
	}
	if hasDecisionWake(base) {
		base.WakeReason = "attention"
		base.WaitedMs = time.Since(start).Milliseconds()
		return base
	}

	baseSig := attentionSignature(base)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	heartbeat := time.NewTicker(watchHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		// Subscribe BEFORE re-reading the snapshot: a change between the last
		// read and this subscribe must not be lost.
		ch := r.subscribe()
		cur := r.watchSnapshot(botID, agent, limit, includeQuiet, 0)
		if hasDecisionWake(cur) {
			cur.WakeReason = "attention"
			cur.WaitedMs = time.Since(start).Milliseconds()
			return cur
		}
		if attentionSignature(cur) != baseSig {
			cur.WakeReason = "change"
			cur.WaitedMs = time.Since(start).Milliseconds()
			return cur
		}

		select {
		case <-ch:
			// A session produced output / changed state / was closed. Wait out
			// one stable window before re-reading: a TUI repaints many times
			// per second, and without this throttle a spinner would spin this
			// loop hot even though the attention signature never changes.
			// It also gives the semantic delta time to settle.
			settle := r.cfg.StableWindow
			if settle < 50*time.Millisecond {
				settle = 50 * time.Millisecond
			}
			t := time.NewTimer(settle)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				out := r.watchSnapshot(botID, agent, limit, includeQuiet, 0)
				out.WakeReason = "cancelled"
				out.WaitedMs = time.Since(start).Milliseconds()
				return out
			case <-deadline.C:
				t.Stop()
				out := r.watchSnapshot(botID, agent, limit, includeQuiet, 0)
				out.WakeReason = "timeout"
				out.WaitedMs = time.Since(start).Milliseconds()
				return out
			}
		case <-heartbeat.C:
			if onHeartbeat != nil {
				onHeartbeat(time.Since(start).Milliseconds())
			}
		case <-deadline.C:
			out := r.watchSnapshot(botID, agent, limit, includeQuiet, 0)
			out.WakeReason = "timeout"
			out.WaitedMs = time.Since(start).Milliseconds()
			out.Note = joinNote(out.Note, "no decision event within the wait window; act only on attention items")
			return out
		case <-ctx.Done():
			out := r.watchSnapshot(botID, agent, limit, includeQuiet, 0)
			out.WakeReason = "cancelled"
			out.WaitedMs = time.Since(start).Milliseconds()
			return out
		}
	}
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}
