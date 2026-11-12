package channel

// Server-side plumbing for interactive PTY sessions. The actual process start
// is platform-specific (session_linux.go / session_other.go); everything here
// is transport and lifecycle only.

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

type sessionRunner struct {
	id uint32
	c  *conn

	mu           sync.Mutex
	ptmx         *os.File
	pgid         int
	started      bool
	lastActivity time.Time // refreshed by stdin, output and host touch frames

	stopOnce sync.Once
	done     chan struct{}

	writeStdin func([]byte) error
	sendSignal func(string) error
	setSize    func(cols, rows uint16) error
}

func (c *conn) beginSession(id uint32) (*sessionRunner, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.sess[id]; exists {
		return nil, errors.New("session id already in use on this connection")
	}
	s := &sessionRunner{id: id, c: c, done: make(chan struct{}), lastActivity: time.Now()}
	c.sess[id] = s
	return s, nil
}

func (s *sessionRunner) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *sessionRunner) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastActivity)
}

func (c *conn) removeSession(id uint32) {
	c.mu.Lock()
	delete(c.sess, id)
	c.mu.Unlock()
}

func (c *conn) abortSessions() {
	c.mu.Lock()
	all := make([]*sessionRunner, 0, len(c.sess))
	for id, s := range c.sess {
		all = append(all, s)
		delete(c.sess, id)
	}
	c.mu.Unlock()
	for _, s := range all {
		s.stop()
	}
}

func (s *sessionRunner) stop() {
	s.stopOnce.Do(func() {
		close(s.done)
		s.mu.Lock()
		f := s.ptmx
		pgid := s.pgid
		s.mu.Unlock()
		if pgid > 1 {
			_ = killSessionGroup(pgid, "TERM")
			// Interactive shells and TUIs routinely ignore SIGTERM (measured
			// on Linux: sh stays Running after close). Escalate to KILL while
			// the pid is still ours — a delayed AfterFunc would risk killing a
			// reused pgid. The local PTY backend has the same TERM->KILL rule.
			for i := 0; i < 20 && sessionGroupAlive(pgid); i++ {
				time.Sleep(25 * time.Millisecond)
			}
			if sessionGroupAlive(pgid) {
				_ = killSessionGroup(pgid, "KILL")
			}
		}
		if f != nil {
			_ = f.Close()
		}
	})
}

func (c *conn) routeSession(f Frame) {
	c.mu.Lock()
	s := c.sess[f.ReqID]
	c.mu.Unlock()
	if s == nil {
		c.sendError(f.ReqID, CodeNotFound, "session not found")
		return
	}
	switch f.Type {
	case MsgSessionTouch:
		s.touch()
	case MsgSessionStdin:
		s.touch()
		var d SessionData
		if err := decode(f.Payload, &d); err != nil {
			c.sendError(f.ReqID, CodeBadRequest, err.Error())
			return
		}
		if s.writeStdin == nil {
			c.sendError(f.ReqID, CodeBusy, "session is still starting")
			return
		}
		if err := s.writeStdin(d.Data); err != nil {
			c.sendFrameErr(f.ReqID, err)
		}
	case MsgSessionSignal:
		var q SessionSignal
		if err := decode(f.Payload, &q); err != nil {
			c.sendError(f.ReqID, CodeBadRequest, err.Error())
			return
		}
		if s.sendSignal == nil {
			c.sendError(f.ReqID, CodeBusy, "session is still starting")
			return
		}
		if err := s.sendSignal(q.Sig); err != nil {
			c.sendFrameErr(f.ReqID, err)
		}
	case MsgSessionResize:
		var q SessionResize
		if err := decode(f.Payload, &q); err != nil {
			c.sendError(f.ReqID, CodeBadRequest, err.Error())
			return
		}
		if s.setSize != nil {
			_ = s.setSize(q.Cols, q.Rows)
		}
	case MsgSessionClose:
		s.stop()
		c.removeSession(f.ReqID)
	}
}

func (c *conn) sendFrameErr(id uint32, err error) {
	if err == nil {
		return
	}
	c.sendError(id, CodeIO, err.Error())
}

func (s *sessionRunner) sendOutput(data []byte) {
	s.touch()
	s.c.sendResult(MsgSessionOutput, s.id, SessionData{Data: data})
}

func (s *sessionRunner) sendDiag(text string) {
	if text == "" {
		return
	}
	s.c.sendResult(MsgSessionDiag, s.id, SessionDiag{Data: text})
}

func (s *sessionRunner) sendExit(exit SessionExit) {
	s.c.sendResult(MsgSessionExit, s.id, exit)
}

func (s *sessionRunner) sendSpawnResult(res SessionSpawnResult) {
	s.c.sendResult(MsgSessionSpawnResult, s.id, res)
}

func (s *sessionRunner) attach(master *os.File, pgid int,
	write func([]byte) error, signal func(string) error, resize func(uint16, uint16) error) {
	s.mu.Lock()
	s.ptmx = master
	s.pgid = pgid
	s.started = true
	s.writeStdin = write
	s.sendSignal = signal
	s.setSize = resize
	s.mu.Unlock()
}

func sessionUnsupportedResult(reason string) SessionSpawnResult {
	return SessionSpawnResult{
		Error: fmt.Sprintf("interactive PTY session unavailable: %s", reason),
		Code:  CodeUnsupported,
	}
}
