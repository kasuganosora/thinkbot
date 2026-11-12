package channel

// Client side of an interactive PTY session.
//
// A session ALWAYS gets its own connection. Client.rpc serialises every RPC on
// a shared connection, and Exec holds that lock until MsgCommandExit; trying
// to multiplex a long-lived session over it would block Info / Exec for the
// whole session lifetime (docs §3.6 Go probe: 2.599s vs 283us).

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
)

type SessionCallbacks struct {
	OnOutput func([]byte)
	OnDiag   func(string)
	OnExit   func(SessionExit)
}

type SessionClient struct {
	c  *Client
	id uint32

	mu     sync.Mutex
	pid    int
	exit   SessionExit
	exited bool
	done   chan struct{}
	once   sync.Once
}

// DialSession authenticates a dedicated connection and starts a PTY session.
// The first frame is consumed synchronously so startup errors are returned to
// the caller; all later frames are delivered through cb from a reader loop.
func DialSession(ctx context.Context, nc net.Conn, opt DialOptions,
	req SessionSpawnRequest, cb SessionCallbacks) (*SessionClient, error) {
	cli, err := DialConn(ctx, nc, opt)
	if err != nil {
		return nil, err
	}
	sc := &SessionClient{c: cli, id: cli.id(), done: make(chan struct{})}
	fail := func(err error) (*SessionClient, error) {
		_ = cli.Close()
		return nil, err
	}
	if err := cli.sendJSON(MsgSessionSpawn, sc.id, req); err != nil {
		return fail(err)
	}
	f, err := cli.readFrame(ctx)
	if err != nil {
		return fail(err)
	}
	if f.Type == MsgError {
		return fail(rpcFrom(f))
	}
	if f.Type != MsgSessionSpawnResult {
		return fail(fmt.Errorf("channel: expected session spawn result, got %d", f.Type))
	}
	var res SessionSpawnResult
	if err := decode(f.Payload, &res); err != nil {
		return fail(err)
	}
	if res.Code != "" || res.Error != "" {
		return fail(fmt.Errorf("channel: session spawn failed (%s): %s", res.Code, res.Error))
	}
	sc.mu.Lock()
	sc.pid = res.PID
	sc.mu.Unlock()
	go sc.readLoop(cb)
	return sc, nil
}

func (s *SessionClient) readLoop(cb SessionCallbacks) {
	// A dropped connection is an exit too. Without publishing it the host
	// would keep reporting state=running forever (health says exited, agent
	// polls an empty screen). markExit returns true only for the first exit,
	// so an explicit MsgSessionExit is never delivered twice.
	lost := SessionExit{ExitCode: -1, Code: CodeIO, Error: "session connection closed"}
	defer func() {
		if s.markExit(lost) && cb.OnExit != nil {
			cb.OnExit(lost)
		}
	}()
	for {
		f, err := s.c.readFrame(context.Background())
		if err != nil {
			if cb.OnDiag != nil && !isClosedConn(err) {
				cb.OnDiag("connection read: " + err.Error())
			}
			return
		}
		switch f.Type {
		case MsgSessionOutput:
			var d SessionData
			if decode(f.Payload, &d) == nil && cb.OnOutput != nil {
				cb.OnOutput(d.Data)
			}
		case MsgSessionDiag:
			var d SessionDiag
			if decode(f.Payload, &d) == nil && cb.OnDiag != nil {
				cb.OnDiag(d.Data)
			}
		case MsgSessionExit:
			var e SessionExit
			_ = decode(f.Payload, &e)
			if s.markExit(e) && cb.OnExit != nil {
				cb.OnExit(e)
			}
			return
		case MsgError:
			e := SessionExit{ExitCode: -1, Code: CodeIO, Error: rpcFrom(f).Error()}
			if s.markExit(e) && cb.OnExit != nil {
				cb.OnExit(e)
			}
			return
		}
	}
}

func isClosedConn(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection reset")
}

// markExit reports whether this was the first (authoritative) exit.
func (s *SessionClient) markExit(e SessionExit) bool {
	first := false
	s.once.Do(func() {
		first = true
		s.mu.Lock()
		s.exit = e
		s.exited = true
		s.mu.Unlock()
		close(s.done)
	})
	return first
}

func (s *SessionClient) send(t MsgType, body any) error {
	// Do not hold s.mu across the network write: readLoop needs the same lock
	// to publish the exit frame. Client.sendRaw already serialises writers.
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if exited {
		return fmt.Errorf("session already exited")
	}
	return s.c.sendJSON(t, s.id, body)
}

func (s *SessionClient) Send(data []byte) error {
	return s.send(MsgSessionStdin, SessionData{Data: data})
}

func (s *SessionClient) Signal(sig string) error {
	return s.send(MsgSessionSignal, SessionSignal{Sig: sig})
}

func (s *SessionClient) Resize(cols, rows uint16) error {
	return s.send(MsgSessionResize, SessionResize{Cols: cols, Rows: rows})
}

// Touch refreshes the container-side idle TTL. It is best effort: a dead
// connection is reported through OnExit, not through this call.
func (s *SessionClient) Touch() error {
	return s.send(MsgSessionTouch, SessionTouch{})
}

func (s *SessionClient) Close() error {
	_ = s.send(MsgSessionClose, SessionClose{})
	return s.c.Close()
}

func (s *SessionClient) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

func (s *SessionClient) Done() <-chan struct{} { return s.done }

func (s *SessionClient) Exit() SessionExit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit
}
