//go:build unix

package sandbox

// Local PTY backend. The process is fully detached from any message context:
// agent turns end after every reply and cancel their context (see docs §3.7),
// so binding a background session to it would kill the CLI after one turn.
// All shared byte/screen/view logic lives in sessionCore (session_core.go).

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/util/idgen"
)

type localSession struct {
	*sessionCore

	cmd       *exec.Cmd
	ptmx      *os.File
	ttlReason string

	stopOnce    sync.Once
	waitOnce    sync.Once
	stopCh      chan struct{}
	readerDone  chan struct{}
	dataChannel chan []byte
}

var _ SessionHandle = (*localSession)(nil)

type localSessionSpawner struct {
	cfg    SessionConfig
	logger *zap.SugaredLogger
	root   string
}

func newLocalSessionSpawner(cfg SessionConfig, logger *zap.SugaredLogger, root string) *localSessionSpawner {
	return &localSessionSpawner{cfg: cfg, logger: logger, root: root}
}

func (s *localSessionSpawner) Spawn(ctx context.Context, botID string, req SpawnRequest) (SessionHandle, error) {
	if strings.TrimSpace(req.Command) == "" {
		return nil, ErrSessionBadInput
	}
	cols := req.Cols
	if cols == 0 {
		cols = s.cfg.ScreenCols
	}
	rows := req.Rows
	if rows == 0 {
		rows = s.cfg.ScreenRows
	}
	term := req.Term
	if term == "" {
		term = s.cfg.Term
	}

	env, dropped := buildSessionEnv(req.Env, term, cols, rows)
	cwd := req.Cwd
	if cwd == "" {
		cwd = s.root
	}

	cmd := exec.Command("sh", "-c", req.Command) //nolint:gosec // command is the agent's explicit request, already sandboxed
	cmd.Dir = cwd
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	master, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	// stderr stays on the PTY: making it a pipe changes isatty(2) and can flip
	// program branches (docs §5). Diagnostics arrive through the side channel.
	cmd.Stderr = slave

	if err := pty.Setsize(master, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, fmt.Errorf("set pty size: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, fmt.Errorf("start session: %w", err)
	}
	_ = slave.Close() // the child holds its own copy

	pid := cmd.Process.Pid
	sess := &localSession{
		sessionCore: newSessionCore(s.cfg, s.logger, idgen.New("sess"),
			req.Command, "local", pid, pid, cols, rows),
		cmd:         cmd,
		ptmx:        master,
		stopCh:      make(chan struct{}),
		readerDone:  make(chan struct{}),
		dataChannel: make(chan []byte, 64),
	}
	sess.alive = sess.procAlive
	if dropped > 0 {
		sess.noteDiag(fmt.Sprintf("startup: dropped %d host env variables (names withheld)", dropped))
	}
	go sess.runReader()
	go sess.runEventLoop()

	sess.logEvent("session_spawn", map[string]any{
		"pid": pid, "pgid": pid, "cols": cols, "rows": rows,
		"envDropped": dropped, "command": redactCommand(req.Command),
	})
	return sess, nil
}

// runReader is the only reader of the PTY master. It exists so the event loop
// can also wait on a timer without blocking the process' output pipe.
func (s *localSession) runReader() {
	defer close(s.readerDone)
	defer close(s.dataChannel)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			select {
			case s.dataChannel <- chunk:
			case <-s.stopCh:
				return
			}
		}
		if err != nil {
			if err != io.EOF && !isClosedErr(err) {
				s.noteDiag("pty read: " + err.Error())
			}
			return
		}
	}
}

// reap is the single waiter for the child. os/exec must be waited on exactly
// once, otherwise the process becomes a zombie.
func (s *localSession) reap() int {
	s.waitOnce.Do(func() {
		if s.cmd != nil {
			_ = s.cmd.Wait()
		}
	})
	if s.cmd != nil && s.cmd.ProcessState != nil {
		return s.cmd.ProcessState.ExitCode()
	}
	return -1
}

func isClosedErr(err error) bool {
	return strings.Contains(err.Error(), "file already closed") ||
		strings.Contains(err.Error(), "input/output error") ||
		strings.Contains(err.Error(), "use of closed")
}

func (s *localSession) runEventLoop() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case chunk, ok := <-s.dataChannel:
			if !ok {
				s.finish()
				return
			}
			s.consume(chunk)
		case <-ticker.C:
			now := time.Now()
			s.mu.RLock()
			st := s.state
			touch := s.lastTouch
			s.mu.RUnlock()
			s.settle(now)
			// Process-level idle TTL: intentionally inside the session's own
			// event loop, not a lazy GC driven by watch/spawn (docs §3.7).
			if st == SessionRunning && now.Sub(touch) > s.cfg.IdleTTL {
				s.noteDiag("idle TTL reached; terminating process group")
				s.terminate(SessionReasonIdleTTL, "idle_ttl")
			}
		case <-s.stopCh:
			return
		}
	}
}

func (s *localSession) finish() {
	code := s.reap()
	s.markFinished(code, SessionReasonExit)
}

func (s *localSession) procAlive() bool {
	if s.cmd == nil || s.cmd.Process == nil {
		return false
	}
	return processAlive(s.cmd.Process)
}

func (s *localSession) Send(data []byte) error {
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()
	if state != SessionRunning {
		return ErrSessionNotRunning
	}
	s.Touch()
	n, err := s.ptmx.Write(data)
	s.logEvent("session_send", map[string]any{"sendBytes": n})
	return err
}

func (s *localSession) Resize(cols, rows uint16) error {
	if err := pty.Setsize(s.ptmx, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
		return err
	}
	s.resizeScreen(cols, rows)
	return nil
}

func (s *localSession) Signal(sig string) error {
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()
	if state != SessionRunning {
		return ErrSessionNotRunning
	}
	s.Touch()
	var signal syscall.Signal
	switch strings.ToUpper(sig) {
	case "INT":
		signal = syscall.SIGINT
	case "TERM":
		signal = syscall.SIGTERM
	case "KILL":
		signal = syscall.SIGKILL
	default:
		return fmt.Errorf("unsupported signal %q", sig)
	}
	// Signalling the whole group is the only safe unit (docs §3.7).
	if err := syscall.Kill(-s.pgid, signal); err != nil {
		return err
	}
	// Real TUI sessions exit on INT rather than resuming (docs §3.4): record
	// the reason up front so the agent sees a coherent final view.
	if signal == syscall.SIGINT || signal == syscall.SIGKILL {
		s.mu.Lock()
		s.reason = SessionReasonSignal
		s.mu.Unlock()
	}
	s.logEvent("session_signal", map[string]any{"sig": sig, "pgid": s.pgid})
	return nil
}

func (s *localSession) terminate(reason, ttlReason string) {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.reason = reason
		s.ttlReason = ttlReason
		s.state = SessionExited
		s.lastSeq = s.ring.Seq()
		s.exitAt = time.Now()
		s.mu.Unlock()
		_ = syscall.Kill(-s.pgid, syscall.SIGTERM)
		time.AfterFunc(s.cfg.TermGrace, func() {
			_ = syscall.Kill(-s.pgid, syscall.SIGKILL)
		})
		close(s.stopCh)
		_ = s.ptmx.Close()
		s.logEvent("session_terminate", map[string]any{"reason": reason, "ttlReason": ttlReason, "pgid": s.pgid})
	})
}

func (s *localSession) Close() error {
	s.terminate(SessionReasonClose, "")
	<-s.readerDone
	_ = s.reap()
	return nil
}
