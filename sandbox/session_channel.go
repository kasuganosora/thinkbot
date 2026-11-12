package sandbox

// ============================================================================
// Channel PTY backend (host side)
//
// The container owns the process group; the host owns the screen model. One
// session == one dedicated connection (Client.rpc is a single mutex, docs §3.6).
// On a lost connection the host NEVER tries to reattach: it reports
// state=lost + lastSeq so the agent reopens (docs §13 #9). The container kills
// its own orphaned process group when touch frames stop arriving.
// ============================================================================

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

type channelSessionDialer func(ctx context.Context, botID string,
	req channel.SessionSpawnRequest, cb channel.SessionCallbacks) (*channel.SessionClient, error)

type channelSessionSpawner struct {
	cfg    SessionConfig
	logger *zap.SugaredLogger
	dial   channelSessionDialer
}

func newChannelSessionSpawner(cfg SessionConfig, logger *zap.SugaredLogger,
	dial channelSessionDialer) *channelSessionSpawner {
	return &channelSessionSpawner{cfg: cfg, logger: logger, dial: dial}
}

var _ SessionSpawner = (*channelSessionSpawner)(nil)

type channelSession struct {
	*sessionCore

	client *channel.SessionClient

	ctlMu          sync.Mutex
	closeRequested bool
	lastTouchSent  time.Time

	stopOnce sync.Once
	exitOnce sync.Once
	done     chan struct{}
}

var _ SessionHandle = (*channelSession)(nil)

func (s *channelSessionSpawner) Spawn(ctx context.Context, botID string, req SpawnRequest) (SessionHandle, error) {
	if s.dial == nil {
		return nil, ErrSessionUnsupported
	}
	if strings.TrimSpace(req.Command) == "" {
		return nil, ErrSessionBadInput
	}
	cols, rows, term := req.Cols, req.Rows, req.Term
	if cols == 0 {
		cols = s.cfg.ScreenCols
	}
	if rows == 0 {
		rows = s.cfg.ScreenRows
	}
	if term == "" {
		term = s.cfg.Term
	}
	// Container env comes from the container, not from the host (see
	// buildContainerSessionEnv). Only explicit extra vars + terminal geometry
	// cross the wire.
	env, dropped := buildContainerSessionEnv(req.Env, term, cols, rows)

	sess := &channelSession{
		sessionCore: newSessionCore(s.cfg, s.logger, idgen.New("sess"),
			req.Command, "channel", 0, 0, cols, rows),
		done: make(chan struct{}),
	}
	sess.alive = sess.connected

	spawnReq := channel.SessionSpawnRequest{
		Command: req.Command, Cwd: req.Cwd, Env: env,
		Cols: cols, Rows: rows, Term: term,
		IdleTTLMs: s.cfg.IdleTTL.Milliseconds(),
	}
	cb := channel.SessionCallbacks{
		OnOutput: sess.onOutput,
		OnDiag: func(text string) {
			_, _ = sess.diag.Write([]byte(text + "\n"))
		},
		OnExit: sess.onExit,
	}
	cli, err := s.dial(ctx, botID, spawnReq, cb)
	if err != nil {
		// Startup failures (unsupported daemon, bad request, jail) are real
		// errors, not a running session with an empty screen.
		return nil, fmt.Errorf("channel session spawn: %w", err)
	}
	sess.client = cli
	sess.pid = cli.PID() // container-namespace pid: never signal it from the host
	if dropped > 0 {
		sess.noteDiag(fmt.Sprintf("startup: dropped %d host env variables (names withheld)", dropped))
	}
	go sess.runTicker()
	sess.logEvent("session_spawn", map[string]any{
		"pid": sess.pid, "cols": cols, "rows": rows,
		"idleTtlMs": spawnReq.IdleTTLMs, "envDropped": dropped,
		"command": redactCommand(req.Command),
	})
	return sess, nil
}

func (s *channelSession) onOutput(b []byte) { s.consume(b) }

func (s *channelSession) onExit(e channel.SessionExit) {
	s.ctlMu.Lock()
	closeReq := s.closeRequested
	s.ctlMu.Unlock()

	code := e.ExitCode
	if code == 0 && e.Error != "" {
		code = -1
	}
	switch {
	case closeReq:
		s.markFinished(code, SessionReasonClose)
	case e.Code == "idle_ttl":
		s.markFinished(code, SessionReasonIdleTTL)
	case e.Code == channel.CodeIO:
		// readLoop lost the connection. No handshake, no reattach: the
		// container is responsible for killing its own process group.
		s.noteDiag("connection lost: " + e.Error)
		s.markLost(SessionReasonConnLost)
	default:
		reason := SessionReasonExit
		if e.Signal != "" {
			reason = SessionReasonSignal
		}
		if e.Error != "" {
			s.noteDiag("daemon exit: " + e.Error)
		}
		s.markFinished(code, reason)
	}
	s.exitOnce.Do(func() { close(s.done) })
}

func (s *channelSession) connected() bool {
	if s.client == nil {
		return false
	}
	select {
	case <-s.client.Done():
		return false
	default:
	}
	s.mu.RLock()
	st := s.state
	s.mu.RUnlock()
	return st == SessionRunning
}

// runTicker keeps the stability window and health heartbeat alive even when
// no bytes arrive. The local backend does the same from its read loop.
func (s *channelSession) runTicker() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.settle(time.Now())
		case <-s.done:
			return
		}
	}
}

// sendTouch is rate limited: poll can be called many times per second and a
// touch frame must never compete with real stdin traffic.
func (s *channelSession) sendTouch() {
	s.ctlMu.Lock()
	if time.Since(s.lastTouchSent) < time.Second {
		s.ctlMu.Unlock()
		return
	}
	s.lastTouchSent = time.Now()
	s.ctlMu.Unlock()
	if s.client != nil {
		_ = s.client.Touch()
	}
}

func (s *channelSession) Poll(sinceSeq uint64, mode string, lines int, wait time.Duration) (SessionView, error) {
	s.sendTouch()
	return s.sessionCore.Poll(sinceSeq, mode, lines, wait)
}

func (s *channelSession) Send(data []byte) error {
	if !s.connected() {
		return ErrSessionNotRunning
	}
	s.Touch()
	err := s.client.Send(data)
	s.logEvent("session_send", map[string]any{"sendBytes": len(data)})
	return err
}

func (s *channelSession) Resize(cols, rows uint16) error {
	if !s.connected() {
		return ErrSessionNotRunning
	}
	if err := s.client.Resize(cols, rows); err != nil {
		return err
	}
	s.resizeScreen(cols, rows)
	return nil
}

func (s *channelSession) Signal(sig string) error {
	if !s.connected() {
		return ErrSessionNotRunning
	}
	sig = strings.ToUpper(sig)
	switch sig {
	case "INT", "TERM", "KILL":
	default:
		return fmt.Errorf("unsupported signal %q", sig)
	}
	s.Touch()
	// Real TUI sessions exit on INT rather than resuming (docs §3.4).
	if sig == "INT" || sig == "KILL" {
		s.mu.Lock()
		s.reason = SessionReasonSignal
		s.mu.Unlock()
	}
	err := s.client.Signal(sig)
	s.logEvent("session_signal", map[string]any{"sig": sig})
	return err
}

func (s *channelSession) Close() error {
	s.stopOnce.Do(func() {
		s.ctlMu.Lock()
		s.closeRequested = true
		s.ctlMu.Unlock()
		if s.client != nil {
			_ = s.client.Close()
		}
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			// The daemon may be gone already; the local view must still settle.
		}
		s.mu.RLock()
		st := s.state
		s.mu.RUnlock()
		if st == SessionRunning {
			s.markFinished(-1, SessionReasonClose)
		}
		s.exitOnce.Do(func() { close(s.done) })
	})
	return nil
}

// routingSessionSpawner picks the backend per bot. A channel bot MUST go to
// the container; silently falling back to a host PTY would break isolation.
type routingSessionSpawner struct {
	local      SessionSpawner
	channel    SessionSpawner
	channelFor func(botID string) bool
}

var _ SessionSpawner = (*routingSessionSpawner)(nil)

func (r *routingSessionSpawner) Spawn(ctx context.Context, botID string, req SpawnRequest) (SessionHandle, error) {
	if r.channelFor != nil && r.channelFor(botID) {
		if r.channel == nil {
			return nil, ErrSessionUnsupported
		}
		return r.channel.Spawn(ctx, botID, req)
	}
	if r.local == nil {
		return nil, ErrSessionUnsupported
	}
	return r.local.Spawn(ctx, botID, req)
}
