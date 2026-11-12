//go:build linux

package channel

// Linux PTY session runner. The container owns the process group; the host
// owns the screen model (docs §3.7). We intentionally do NOT call cmd.Wait():
// the global Reaper is the only waiter on this daemon (reaper_unix.go), and
// mixing os/exec.Wait with Wait4 races. Exit status is therefore reported as
// -1 (the host only needs state + reason, not a precise code).

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// sessionGroupAlive uses signal 0 on the whole process group. A zombie whose
// group is already empty reports ESRCH, so the KILL escalation cannot outlive
// the session it belongs to.
func sessionGroupAlive(pgid int) bool {
	if pgid <= 1 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}

func killSessionGroup(pgid int, sig string) error {
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal pid <= 1")
	}
	var s syscall.Signal
	switch strings.ToUpper(sig) {
	case "INT":
		s = syscall.SIGINT
	case "TERM":
		s = syscall.SIGTERM
	case "KILL":
		s = syscall.SIGKILL
	default:
		return fmt.Errorf("unsupported signal %q", sig)
	}
	return syscall.Kill(-pgid, s)
}

func (c *conn) runSession(f Frame, s *sessionRunner) {
	defer c.removeSession(f.ReqID)
	defer s.stop()

	var req SessionSpawnRequest
	if err := decode(f.Payload, &req); err != nil {
		s.sendSpawnResult(SessionSpawnResult{Error: err.Error(), Code: CodeBadRequest})
		return
	}
	cwd := c.s.cfg.Root
	if req.Cwd != "" {
		p, err := JailPath(cwd, req.Cwd)
		if err != nil {
			s.sendSpawnResult(SessionSpawnResult{Error: err.Error(), Code: CodeJail})
			return
		}
		cwd = p
	}
	var cmd *exec.Cmd
	if len(req.Argv) > 0 {
		cmd = exec.Command(req.Argv[0], req.Argv[1:]...)
	} else if strings.TrimSpace(req.Command) != "" {
		cmd = exec.Command("/bin/sh", "-c", req.Command)
	} else {
		s.sendSpawnResult(SessionSpawnResult{Error: "command or argv is required", Code: CodeBadRequest})
		return
	}
	cmd.Dir = cwd
	// The host already applied an env allowlist before putting env on the
	// wire; childEnv scrubs container-side credential-like variables too.
	cmd.Env = childEnv(req.Env)

	cols, rows := req.Cols, req.Rows
	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 40
	}
	master, slave, err := pty.Open()
	if err != nil {
		s.sendSpawnResult(SessionSpawnResult{Error: err.Error(), Code: CodeIO})
		return
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: cols, Rows: rows}); err != nil {
		_ = master.Close()
		_ = slave.Close()
		s.sendSpawnResult(SessionSpawnResult{Error: err.Error(), Code: CodeIO})
		return
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	// stderr stays on the PTY to keep isatty(2) unchanged (docs §5).
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		s.sendSpawnResult(SessionSpawnResult{Error: err.Error(), Code: CodeIO})
		return
	}
	_ = slave.Close()

	pid := cmd.Process.Pid
	s.attach(master, pid,
		func(b []byte) error { _, e := master.Write(b); return e },
		func(sig string) error { return killSessionGroup(pid, sig) },
		func(c, r uint16) error { return pty.Setsize(master, &pty.Winsize{Cols: c, Rows: r}) },
	)
	s.sendSpawnResult(SessionSpawnResult{PID: pid})

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := master.Read(buf)
			if n > 0 {
				s.sendOutput(append([]byte(nil), buf[:n]...))
			}
			if readErr != nil {
				// A stop() initiated by close/idle-TTL closes the master too;
				// do not emit a duplicate exit frame in that case.
				select {
				case <-s.done:
					return
				default:
				}
				if readErr != io.EOF && !isPTYClosed(readErr) {
					s.sendDiag("pty read: " + readErr.Error())
				}
				s.sendExit(SessionExit{ExitCode: -1})
				s.stop()
				return
			}
		}
	}()

	// The container owns reclamation. If the host disappears (crash, abort,
	// network loss) nobody sends touch frames any more and the process group
	// is killed here instead of surviving as an orphan (docs §13 #9).
	ttl := time.Duration(req.IdleTTLMs) * time.Millisecond
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if s.idleFor() > ttl {
				s.sendDiag("idle TTL reached; terminating session process group")
				s.sendExit(SessionExit{ExitCode: -1, Code: "idle_ttl"})
				s.stop()
				return
			}
		case <-s.done:
			return
		}
	}
}

func isPTYClosed(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "input/output error") ||
		strings.Contains(msg, "file already closed") ||
		strings.Contains(msg, "use of closed")
}
