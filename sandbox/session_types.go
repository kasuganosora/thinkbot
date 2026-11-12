package sandbox

// ============================================================================
// Interactive PTY session contracts
//
// Design: docs/design-agent-interactive-pty.md (v3.7).
// A session is a background process owned by SessionRegistry. The agent gets an
// opaque sessionId and decides when to poll or send; there is no
// "awaiting_input" gate. Everything here is backend-independent; the local PTY
// implementation lives in session_local.go, the channel transport in
// sandbox/channel/session.go.
// ============================================================================

import (
	"context"
	"errors"
	"time"
)

// SessionState is deliberately small. "idle" is a health verdict, not a state.
type SessionState string

const (
	SessionRunning SessionState = "running"
	SessionExited  SessionState = "exited"
	SessionLost    SessionState = "lost"
)

// Reason codes for state=exited / state=lost. Contract: lost MUST carry
// reason + lastSeq so the agent can tell "reopen" from "keep polling".
const (
	SessionReasonDaemonGone = "daemon_gone"
	SessionReasonConnLost   = "conn_lost"
	SessionReasonIdleTTL    = "idle_ttl"
	SessionReasonSignal     = "signal"
	SessionReasonClose      = "close"
	SessionReasonExit       = "exit"
)

// Health verdicts. Purely diagnostic: health never drives a state transition
// and never triggers an automatic send/signal/close.
const (
	HealthProgressing              = "progressing"
	HealthSpinning                 = "spinning"
	HealthAwaitingAction           = "awaiting_action"
	HealthSuspectedStuck           = "suspected_stuck"
	HealthSuspectedStuckNoProgress = "suspected_stuck:no_progress"
	// HealthAwaitingSecret: the tail of the screen asks for a credential
	// (ssh password / key passphrase / OTP). It is a distinct verdict from
	// actionable because the correct next move is a SECRET write, not a normal
	// send, and the model must not put the literal value in its tool arguments.
	HealthAwaitingSecret = "awaiting_secret"
	// HealthConnectFailed: authentication / network failure visible on screen.
	// Retrying blindly is wrong; the agent should surface it instead.
	HealthConnectFailed = "connect_failed"
	HealthIdleAwaitingAgent        = "idle:awaiting_agent"
	HealthIdle                     = "idle"
	HealthUnhealthyDaemon          = "unhealthy:daemon"
	HealthExited                   = "exited"
)

// Winsize hard limits. A browser resize frame is untrusted input: an unbounded
// rows value makes pty.Setsize advertise a giant window to the child, and the
// daemon-side VT grid would be rebuilt for a size the PTY never really had.
// These are the SAME caps sessionCore.resizeScreen applies, so PTY winsize and
// the agent-facing screen can no longer diverge (docs §10.1).
const (
	MaxSessionCols uint16 = 1000
	MaxSessionRows uint16 = 500
)

// ClampWinsize normalizes a requested terminal size. Zero means "use the
// backend default" and is left alone; oversized values are clamped rather than
// rejected so a misbehaving browser still gets a usable session instead of an
// error frame.
func ClampWinsize(cols, rows uint16) (uint16, uint16) {
	if cols > MaxSessionCols {
		cols = MaxSessionCols
	}
	if rows > MaxSessionRows {
		rows = MaxSessionRows
	}
	return cols, rows
}

var (
	ErrSessionNotFound    = errors.New("session not found")
	ErrSessionWrongBot    = errors.New("session belongs to another bot")
	ErrSessionNotRunning  = errors.New("session is not running")
	ErrSessionLimit       = errors.New("session limit reached")
	ErrSessionUnsupported = errors.New("interactive session is unsupported on this backend")
	ErrSessionBadInput    = errors.New("invalid session input")
)

// SessionConfig holds every tunable for background sessions. It is part of
// sandbox.Config so it flows through the normal config chain; nothing here is
// hardcoded at the call site (see docs §8 "配置化落点").
type SessionConfig struct {
	IdleTTL           time.Duration // no poll/send for this long -> readLoop kills the process group
	TermGrace         time.Duration // TERM -> KILL grace
	MaxPerBot         int
	MaxTotal          int
	HealthWindow      time.Duration // sliding window for seqRate / changedPerChunk
	StableWindow      time.Duration // quiet period before a semantic delta is settled
	RawBufferBytes    int           // ring buffer capacity (per session)
	ScreenCols        uint16
	ScreenRows        uint16
	Term              string
	DiagTailBytes     int
	CollapseThreshold int // same row rewritten >= K times inside a window -> collapsed:N
	DeltaMaxLines     int
	ReadLoopHeartbeat time.Duration
	RetainAfterExit   time.Duration
	PollWaitMax       time.Duration
	SeqRateThreshold  float64
	ChangeRatioMin    float64

	// AutoUpgradeWindow is how long sandbox_exec(background="auto") watches a
	// freshly spawned PTY session before deciding whether to return a one-shot
	// result or a session handle. A process that exits inside this window is
	// returned as a normal ExecResult (docs §3).
	AutoUpgradeWindow time.Duration
	// AutoUpgradeMaxWait bounds the total observation time when a process is
	// alive but produced zero output: MUST NOT be upgraded, it is a suspected
	// hidden startup failure (docs §3.2). 0 keeps the default.
	AutoUpgradeMaxWait time.Duration
	// WatchWaitMax caps a single sandbox_session_watch long poll. A negative
	// value (set after defaults are filled) disables waiting entirely.
	WatchWaitMax time.Duration
}

// DefaultSessionConfig mirrors the values finalized in docs §13.
func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		IdleTTL:            15 * time.Minute,
		TermGrace:          2 * time.Second,
		MaxPerBot:          4,
		MaxTotal:           32,
		HealthWindow:       5 * time.Second,
		StableWindow:       120 * time.Millisecond,
		RawBufferBytes:     1 << 20, // 1 MB
		ScreenCols:         120,
		ScreenRows:         40,
		Term:               "xterm-256color",
		DiagTailBytes:      4096,
		CollapseThreshold:  3,
		DeltaMaxLines:      200,
		ReadLoopHeartbeat:  2 * time.Second,
		RetainAfterExit:    60 * time.Second,
		PollWaitMax:        8 * time.Second,
		SeqRateThreshold:   20,
		ChangeRatioMin:     1.5,
		AutoUpgradeWindow:  2500 * time.Millisecond,
		AutoUpgradeMaxWait: 8 * time.Second,
		WatchWaitMax:       30 * time.Second,
	}
}

// SessionView is what poll/watch return. Keep field names and JSON tags in sync
// with the tool contract in docs §4 — every field has a consumer listed there.
type SessionView struct {
	SessionID  string        `json:"sessionId"`
	PID        int           `json:"pid"`
	PGID       int           `json:"pgid,omitempty"`
	State      SessionState  `json:"state"`
	Backend    string        `json:"backend,omitempty"`
	Seq        uint64        `json:"seq"`        // absolute raw byte count; raw mode / diagnostics only
	SettledSeq uint64        `json:"settledSeq"` // semantic deltas settled up to here
	Delta      string        `json:"delta"`
	Screen     string        `json:"screen"`
	URLs       []string      `json:"urls"`
	ExitCode   *int          `json:"exitCode,omitempty"`
	Hint       string        `json:"hint,omitempty"`
	Truncated  bool          `json:"truncated"`
	Merged     bool          `json:"merged"`
	StderrTail string        `json:"stderrTail,omitempty"`
	BootDiag   string        `json:"bootDiag,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	LastSeq    uint64        `json:"lastSeq,omitempty"`
	Orphaned   bool          `json:"orphaned,omitempty"`
	Health     SessionHealth `json:"health"`
}

// RawChunk is the web-console streaming contract. Data is raw terminal bytes
// (sent as a binary WebSocket frame); Seq is the absolute raw-byte watermark,
// NOT SettledSeq. Resync means the requested position fell out of the ring
// buffer and Data is a whole-window snapshot: the client must reset xterm and
// replay, otherwise it would show a stream with a hole in the middle.
type RawChunk struct {
	Data   []byte       `json:"-"`
	Seq    uint64       `json:"seq"`
	State  SessionState `json:"state"`
	Cols   int          `json:"cols"`
	Rows   int          `json:"rows"`
	Resync bool         `json:"resync"`
}

// SessionHealth is computed from daemon-internal signals only (zero OS probes).
type SessionHealth struct {
	Verdict         string  `json:"verdict"`
	SeqRate         float64 `json:"seqRate"`
	ChangedPerChunk float64 `json:"changedPerChunk"`
	ScreenStableMs  int64   `json:"screenStableMs"`
	Actionable      bool    `json:"actionable"`
	InputReady      bool    `json:"inputReady"`
	Waiting         bool    `json:"waiting"`
	ReadLoopAlive   bool    `json:"readLoopAlive"`
	ProcAlive       bool    `json:"procAlive"`
	Evidence        string  `json:"evidence"`
}

// SpawnRequest is the backend-independent start request.
type SpawnRequest struct {
	Command    string   `json:"command"`
	Cwd        string   `json:"cwd,omitempty"`
	Env        []string `json:"env,omitempty"`
	Cols       uint16   `json:"cols,omitempty"`
	Rows       uint16   `json:"rows,omitempty"`
	Term       string   `json:"term,omitempty"`
	Background bool     `json:"background"`
}

// SessionHandle is the owner-side handle. Tools never hold this directly;
// SessionRegistry is the only owner (docs §3.7 / §7).
type SessionHandle interface {
	Poll(sinceSeq uint64, mode string, lines int, wait time.Duration) (SessionView, error)
	Send(data []byte) error
	Resize(cols, rows uint16) error
	Signal(sig string) error
	Close() error
	View() SessionView
	RawSince(seq uint64) RawChunk
	Touch()
	LastTouch() time.Time
	BotID() string
	SetOwner(botID string)
	ID() string
}

// SessionSpawner is implemented by each backend. channel uses an independent
// connection per session (docs §3.6); local uses a PTY. botID is supplied by
// the registry (never by the model): the channel backend needs it to pick the
// right per-bot container, unix socket and HKDF token.
type SessionSpawner interface {
	Spawn(ctx context.Context, botID string, req SpawnRequest) (SessionHandle, error)
}

// WatchItem is one attention row. watch is read-only: it never consumes delta
// and never advances a cursor (docs §3.7).
type WatchItem struct {
	SessionID string   `json:"sessionId"`
	PID       int      `json:"pid"`
	PGID      int      `json:"pgid,omitempty"`
	State     string   `json:"state"`
	Verdict   string   `json:"verdict"`
	Priority  int      `json:"priority"` // P0=0, P1=1, P2=2
	Reason    string   `json:"reason"`
	Suggested string   `json:"suggested"`
	Seq       uint64   `json:"seq"`
	SinceSeq  uint64   `json:"sinceSeq"`
	Orphaned  bool     `json:"orphaned,omitempty"`
	URLs      []string `json:"urls,omitempty"`
	IdleMs    int64    `json:"idleMs"`
}

type WatchResult struct {
	BotID     string      `json:"botId"`
	Total     int         `json:"total"`
	Attention []WatchItem `json:"attention"`
	Quiet     int         `json:"quiet"`
	Reclaimed int         `json:"reclaimed"`
	Note      string      `json:"note,omitempty"`
	// WaitedMs / WakeReason are set only by long-poll watch. WakeReason is one
	// of "attention" | "change" | "timeout" | "cancelled" | "nowait".
	WaitedMs   int64  `json:"waitedMs,omitempty"`
	WakeReason string `json:"wakeReason,omitempty"`
}
