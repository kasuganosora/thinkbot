package sandbox

// ============================================================================
// sandbox_exec(background="auto") — optional one-shot-to-background upgrade
//
// Design: docs/design-agent-interactive-pty.md §3 / §11 P2.
//
// The whole point of auto is to decide the RETURN TIMING, not to classify the
// program. We always start a real PTY session first (never re-exec the command:
// a second execution could have side effects), then watch it for a short
// window and pick one of three outcomes:
//
//   1. exited / lost inside the window -> return a one-shot ExecResult-shaped
//      result containing every byte already captured (docs §11 "不丢已收输出")
//      and close the session so it cannot become an orphan;
//   2. alive with visible output at the window edge -> upgrade: return the
//      sessionId and let the agent drive it with sandbox_session_poll/send;
//   3. alive with ZERO output -> NEVER upgrade. That is exactly the shape of a
//      hidden startup failure (real CLI EADDRINUSE: alive, 45s, no bytes,
//      §3.2). Close it and return a startup diagnostic instead.
//
// The positive "TUI-ness" signals from §3 (CSI density, alt-screen, structured
// options) are reported as observable evidence but deliberately NOT used as a
// hard gate: §3 says a misclassification only costs an early return, and the
// real CLI produced csi=87 with alt=0, so any hard gate would be brittle.
// ============================================================================

import (
	"context"
	"strings"
	"time"
)

// AutoUpgradeOutcome is returned to the model. All three branches share the
// same shape keys where possible so the agent can switch on "upgraded".
type AutoUpgradeOutcome struct {
	Upgraded      bool   `json:"upgraded"`
	Reason        string `json:"reason"`
	ObservedMs    int64  `json:"observedMs"`
	UpgradeReason string `json:"upgradeReason,omitempty"`
}

const (
	autoReasonExited    = "exited_in_window"
	autoReasonLost      = "session_lost"
	autoReasonUpgraded  = "alive_with_output"
	autoReasonNoOutput  = "zero_output_startup_diagnostic"
	autoReasonSpawnFail = "spawn_failed"
)

// maxAutoUpgradeOutput caps the one-shot stdout we synthesise from the PTY
// bytes. The execution layer already truncates at MaxOutput; this is a second
// guard so an auto-upgraded chatty program cannot blow up the tool result.
const maxAutoUpgradeOutput = 1 << 20

// autoUpgradeSession is the minimal surface needed from SessionRegistry.
// Keeping it as a small interface makes the decision logic unit-testable
// without a real PTY (the real caller passes *SessionRegistry).
type autoUpgradeSession interface {
	Spawn(ctx context.Context, botID, agent string, req SpawnRequest) (SessionView, error)
	View(botID, sessionID string) (SessionView, error)
	RawSince(botID, sessionID string, seq uint64) (RawChunk, error)
	Close(botID, sessionID string) error
}

// runAutoUpgrade implements the three-way decision. It NEVER re-executes the
// command and NEVER leaves a live session behind on a non-upgrade branch.
func runAutoUpgrade(ctx context.Context, reg autoUpgradeSession, botID string,
	req SpawnRequest, cfg SessionConfig) (map[string]any, error) {

	start := time.Now()
	// Spawn is the only place that must escape the caller's cancellation: a
	// background session has to outlive the message context (docs §3.7/v3.7).
	// The observation loop below deliberately keeps using ctx, so an abort
	// interrupts the wait and closes the not-yet-handed-over session.
	view, err := reg.Spawn(context.WithoutCancel(ctx), botID, sessionAgent, req)
	if err != nil {
		// Spawn failed (unsupported backend / limit / bad path): fall back to a
		// normal sync exec. The caller decides that; here we just surface it.
		return nil, err
	}
	sid := view.SessionID

	window := cfg.AutoUpgradeWindow
	maxWait := cfg.AutoUpgradeMaxWait
	if window <= 0 {
		window = 2500 * time.Millisecond
	}
	if maxWait < window {
		maxWait = window
	}

	// Two phases, both driven by a single ticker and absolute deadlines:
	//
	//   phase 1 [0, window): a process that exits here is an ordinary short
	//            command -> return a one-shot result. Visible output is only
	//            remembered, not acted on, so a chatty-but-short command is
	//            not upgraded before it gets the chance to finish.
	//   phase 2 [window, maxWait]: alive + visible output -> upgrade at once;
	//            zero output -> keep waiting for a slow starter, and at
	//            maxWait return a startup diagnostic instead of a handle.
	windowAt := start.Add(window)
	maxAt := start.Add(maxWait)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		v, verr := reg.View(botID, sid)
		if verr != nil {
			// Session vanished from the registry (e.g. concurrent close_all).
			// Report it as a lost one-shot result instead of silently succeeding.
			return autoOneShotResult(reg, botID, sid, SessionView{
				SessionID: sid, State: SessionLost, Reason: SessionReasonDaemonGone,
			}, autoReasonLost, start, req.Cwd), nil
		}

		if v.State != SessionRunning {
			reason := autoReasonExited
			if v.State == SessionLost {
				reason = autoReasonLost
			}
			return autoOneShotResult(reg, botID, sid, v, reason, start, req.Cwd), nil
		}

		now := time.Now()
		if now.Before(windowAt) {
			if err := autoWaitTick(ctx, ticker.C); err != nil {
				return autoOneShotResult(reg, botID, sid, v, autoReasonNoOutput, start, req.Cwd), nil
			}
			continue
		}

		if hasVisibleOutput(v) {
			// Outcome 2: alive past the window and demonstrably producing a
			// screen. Upgrade. The session stays owned by the registry; the
			// agent takes over with sandbox_session_poll using settledSeq.
			out := sessionViewToMap(v)
			out["upgraded"] = true
			out["background"] = true
			out["reason"] = autoReasonUpgraded
			out["observedMs"] = time.Since(start).Milliseconds()
			out["upgradeReason"] = autoUpgradeEvidence(v)
			out["hint"] = "Command is still running as a background session. " +
				"Use session_poll with sinceSeq=settledSeq to read new output, " +
				"session_send to interact, and session_close when done. " +
				"stdout/stderr are merged in the PTY screen."
			return out, nil
		}

		if !now.Before(maxAt) {
			// Outcome 3: zero output + alive for the whole diagnostic window.
			// Do NOT hand a possibly-broken process to the agent.
			return autoOneShotResult(reg, botID, sid, v, autoReasonNoOutput, start, req.Cwd), nil
		}
		if err := autoWaitTick(ctx, ticker.C); err != nil {
			// Abort must not leave a live PTY process behind: this branch owns
			// a session the agent never received a handle for. autoOneShotResult
			// performs the single Close for every non-upgrade branch.
			return autoOneShotResult(reg, botID, sid, v, autoReasonNoOutput, start, req.Cwd), nil
		}
	}
}

// autoWaitTick waits for one observation tick; a non-nil error means the
// caller's context was cancelled (chat abort / tool timeout).
func autoWaitTick(ctx context.Context, tick <-chan time.Time) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tick:
		return nil
	}
}

// hasVisibleOutput is the HARD negative condition from §3.2. A raw Seq > 0 is
// not enough: a program can emit only cursor moves / colour resets / NULs and
// still have an empty screen, which is indistinguishable from a startup
// failure. We require actual printable content on the rendered screen.
func hasVisibleOutput(v SessionView) bool {
	if v.State != SessionRunning {
		return true // exit/lost is handled by its own branch
	}
	return strings.TrimSpace(v.Screen) != ""
}

// autoUpgradeEvidence reports the §3 positive signals as diagnostics. It is
// intentionally not a gate (see file header).
func autoUpgradeEvidence(v SessionView) string {
	var parts []string
	if v.Health.Actionable {
		parts = append(parts, "actionable_prompt")
	}
	if v.Health.InputReady {
		parts = append(parts, "input_ready")
	}
	if v.Health.Waiting {
		parts = append(parts, "waiting_indicator")
	}
	if v.Health.SeqRate > 0 {
		parts = append(parts, "csi_redraw_activity")
	}
	if len(v.URLs) > 0 {
		parts = append(parts, "out_of_band_url")
	}
	if len(parts) == 0 {
		return "alive_with_visible_output"
	}
	return strings.Join(parts, ",")
}

// autoOneShotResult converts a finished (or rejected) session back into the
// normal sandbox_exec result shape, so an auto call that turned out to be a
// short command is indistinguishable from a sync call for the agent.
func autoOneShotResult(reg autoUpgradeSession, botID, sid string,
	v SessionView, reason string, start time.Time, cwd string) map[string]any {

	raw, _ := reg.RawSince(botID, sid, 0)
	stdout := stripTerminalBytes(raw.Data)
	stderr := strings.TrimSpace(v.StderrTail)
	boot := strings.TrimSpace(v.BootDiag)

	warnings := []string{}
	if reason == autoReasonNoOutput {
		warnings = append(warnings,
			"background=auto did NOT upgrade: the process was alive but produced no visible output. "+
				"This is the shape of a hidden startup failure; the session was closed. "+
				"Check bootDiag/stderrTail, or use session_spawn explicitly if the program is simply slow to start.")
	}
	if reason == autoReasonLost {
		warnings = append(warnings,
			"background=auto session was lost before a handle could be returned; partial output only.")
	}
	if reason == autoReasonExited || reason == autoReasonLost || reason == autoReasonNoOutput {
		warnings = append(warnings,
			"Output came from a PTY session: stdout/stderr were merged and terminal control bytes were stripped. "+
				"Exit-code and reliability signals are best-effort compared with a plain exec.")
	}

	code := 0
	if v.ExitCode != nil {
		code = *v.ExitCode
	}
	reliable := reason == autoReasonExited && v.State == SessionExited
	if len(stdout) > maxAutoUpgradeOutput {
		stdout = stdout[:maxAutoUpgradeOutput]
		warnings = append(warnings, "auto-upgrade output truncated at 1 MiB")
	}

	res := &ExecResult{
		ExitCode:  code,
		Stdout:    stdout,
		Stderr:    stderr,
		Truncated: len(stdout) >= maxAutoUpgradeOutput,
		Reliable:  reliable,
		// Aborted means "killed by a signal / timed out". A zero-output
		// diagnostic is neither: we deliberately declined to upgrade and
		// closed the session. Marking it Aborted would emit a misleading
		// "failed midway (killed or timed out)" warning.
		Aborted:  reason == autoReasonLost,
		Warnings: warnings,
	}
	out := execResultToToolOutput(res, cwd)
	out["upgraded"] = false
	out["background"] = false
	out["reason"] = reason
	out["observedMs"] = time.Since(start).Milliseconds()
	// Kept for trace correlation ONLY. Every non-upgrade branch closes the
	// session below, so presenting this as a live handle made the model call
	// session_close on it in a later turn and get "session not found".
	out["sessionId"] = sid
	out["sessionClosed"] = true
	out["hint"] = "background=auto did not upgrade; this sessionId is already closed. " +
		"Do not call session_poll / session_close on it; call session_watch for the ids of live sessions."
	if boot != "" {
		out["bootDiag"] = boot
	}
	if v.State != SessionExited {
		out["state"] = string(v.State)
	}
	// Every non-upgrade branch must release the registry entry, otherwise a
	// rejected auto attempt leaves a live process with no handle (docs §3.7).
	_ = reg.Close(botID, sid)
	return out
}

// stripTerminalBytes turns raw PTY bytes into readable plain text without
// losing user-visible content. It removes CSI/OSC/other ESC sequences,
// normalises CRLF/CR to LF (PTY line discipline), and drops C0 control bytes
// except tab/newline. This is deliberately a byte-level strip, NOT a VT
// re-render: for one-shot commands we must keep scrolled-off history rather
// than only the final screen grid.
func stripTerminalBytes(b []byte) string {
	if len(b) > maxAutoUpgradeOutput*2 {
		b = b[:maxAutoUpgradeOutput*2]
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == 0x1b { // ESC
			i++
			if i >= len(b) {
				break
			}
			switch b[i] {
			case '[': // CSI: consume params/intermediates then one final byte
				for i+1 < len(b) {
					i++
					if b[i] >= 0x40 && b[i] <= 0x7e {
						break
					}
				}
			case ']': // OSC: terminated by BEL or ST (ESC \)
				for i+1 < len(b) {
					i++
					if b[i] == 0x07 {
						break
					}
					if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
						i++
						break
					}
				}
			case 'P', 'X', '^', '_': // DCS/SOS/PM/APC: terminated by ST
				for i+1 < len(b) {
					i++
					if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
						i++
						break
					}
				}
			default: // 2-char ESC sequence
			}
			continue
		}
		switch c {
		case '\r':
			if i+1 < len(b) && b[i+1] == '\n' {
				continue // CRLF: the \n below emits one newline
			}
			sb.WriteByte('\n')
		case '\n', '\t':
			sb.WriteByte(c)
		default:
			if c < 0x20 || c == 0x7f {
				continue
			}
			sb.WriteByte(c)
		}
	}
	return sb.String()
}
