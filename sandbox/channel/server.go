package channel

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Config is the daemon configuration. Token comes from the environment; the
// main process derives it later and injects it. An empty token rejects every
// Hello. Health.Ping stays available so a local HEALTHCHECK can succeed
// before that injection exists.
type Config struct {
	Root        string
	Token       string
	BotID       string
	SocketPath  string
	MaxPutBytes int64
	OnShutdown  func()
}

// Server is the unix-socket RPC server.
type Server struct {
	cfg     Config
	ln      net.Listener
	reaper  *Reaper
	started time.Time
	ready   string

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

// Listen creates the socket (mode 0600) and the ready file next to it.
func Listen(cfg Config) (*Server, error) {
	if cfg.Root == "" {
		cfg.Root = DefaultRoot
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSock
	}
	if cfg.MaxPutBytes == 0 {
		cfg.MaxPutBytes = DefaultMaxPut
	}
	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		return nil, err
	}
	dir := filepath.Dir(cfg.SocketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(cfg.SocketPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(cfg.SocketPath)
	}
	ln, err := listenUnix(cfg.SocketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	ready := filepath.Join(dir, "ready")
	if err := os.WriteFile(ready, []byte("protocol="+Protocol+"\nversion="+Version+"\n"), 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if err := claimSockPeer(dir, cfg.SocketPath, ready); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &Server{
		cfg:     cfg,
		ln:      ln,
		reaper:  NewReaper(),
		started: time.Now(),
		ready:   ready,
		conns:   map[net.Conn]struct{}{},
	}, nil
}

// Serve accepts until ctx is cancelled or Close is called.
func (s *Server) Serve(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.ln.Close()
		case <-done:
		}
	}()
	defer close(done)
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || s.isClosed() {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.track(nc)
		go func() {
			defer s.untrack(nc)
			s.serveConn(nc)
		}()
	}
}

// Close stops the listener, signals commands this daemon started, and removes
// the ready file. It does not signal unrelated host processes.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	ln := s.ln
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	var err error
	if ln != nil {
		err = ln.Close()
	}
	for nc := range conns {
		_ = nc.Close()
	}
	if s.reaper != nil {
		s.reaper.Forward(syscall.SIGTERM)
		s.reaper.Close()
	}
	if s.ready != "" {
		_ = os.Remove(s.ready)
	}
	if s.cfg.SocketPath != "" {
		if fi, statErr := os.Lstat(s.cfg.SocketPath); statErr == nil && fi.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(s.cfg.SocketPath)
		}
	}
	return err
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) track(nc net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = nc.Close()
		return
	}
	s.conns[nc] = struct{}{}
}

func (s *Server) untrack(nc net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, nc)
}

// Addr returns the socket path.
func (s *Server) Addr() string { return s.cfg.SocketPath }

type conn struct {
	s    *Server
	nc   net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	wmu  sync.Mutex
	mu   sync.Mutex
	ok   bool
	puts map[uint32]*putSession
	exec map[uint32]*execSession
}

type putSession struct {
	ch   chan Frame
	done chan struct{}
	once sync.Once
}

type execSession struct {
	mu         sync.Mutex
	pid        int
	pending    syscall.Signal
	hasPending bool
	done       chan struct{}
}

func (s *Server) serveConn(nc net.Conn) {
	c := &conn{
		s:    s,
		nc:   nc,
		br:   bufio.NewReader(nc),
		bw:   bufio.NewWriter(nc),
		puts: map[uint32]*putSession{},
		exec: map[uint32]*execSession{},
	}
	defer nc.Close()
	defer c.abortExecs()
	defer c.cancelPuts()
	for {
		f, err := ReadFrame(c.br, DefaultMaxFrame)
		if err != nil {
			return
		}
		c.dispatch(f)
	}
}

func (c *conn) dispatch(f Frame) {
	switch f.Type {
	case MsgHealthPing:
		c.sendResult(MsgHealthPong, f.ReqID, HealthPong{OK: true, Version: Version, Protocol: Protocol})
		return
	case MsgHello:
		c.hello(f)
		return
	case MsgFilePutData, MsgFilePutCommit, MsgFilePutAbort:
		c.forwardPut(f)
		return
	case MsgCommandSignal:
		c.signalExec(f)
		return
	}
	if !c.authed() && f.Type != MsgHealthPing {
		c.sendError(f.ReqID, CodeUnauthorized, "hello required")
		return
	}
	switch f.Type {
	case MsgHealthInfo:
		c.sendResult(MsgHealthInfoResult, f.ReqID, c.s.healthInfo())
	case MsgFilePutOpen:
		sess, err := c.beginPut(f.ReqID)
		if err != nil {
			c.sendError(f.ReqID, CodeBusy, err.Error())
			return
		}
		go c.runPut(f, sess)
	case MsgFileGetOpen:
		go c.runGet(f)
	case MsgFileList:
		go c.runList(f)
	case MsgFileDelete:
		go c.runDelete(f)
	case MsgFileStat:
		go c.runStat(f)
	case MsgCommandExec:
		sess, err := c.beginExec(f.ReqID)
		if err != nil {
			c.sendError(f.ReqID, CodeBusy, err.Error())
			return
		}
		go c.runExec(f, sess)
	case MsgProcessList:
		go c.runProcessList(f)
	case MsgProcessSignal:
		go c.runProcessSignal(f, false)
	case MsgProcessKill:
		go c.runProcessSignal(f, true)
	case MsgShutdown:
		c.sendResult(MsgShutdownAck, f.ReqID, OkResult{OK: true})
		if c.s.cfg.OnShutdown != nil {
			go c.s.cfg.OnShutdown()
		}
	default:
		c.sendError(f.ReqID, CodeUnsupported, "unknown message")
	}
}

func (c *conn) authed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ok
}

func (c *conn) hello(f Frame) {
	var req Hello
	if err := decode(f.Payload, &req); err != nil {
		c.sendError(f.ReqID, CodeBadRequest, err.Error())
		_ = c.nc.Close()
		return
	}
	if !tokenMatch(c.s.cfg.Token, req.Token) {
		c.sendError(f.ReqID, CodeUnauthorized, "token rejected")
		_ = c.nc.Close()
		return
	}
	if c.s.cfg.BotID != "" && req.BotID != c.s.cfg.BotID {
		c.sendError(f.ReqID, CodeUnauthorized, "bot id rejected")
		_ = c.nc.Close()
		return
	}
	c.mu.Lock()
	c.ok = true
	c.mu.Unlock()
	c.sendResult(MsgHelloAck, f.ReqID, HelloAck{
		ServerVersion: Version,
		Protocol:      Protocol,
		Capabilities:  Capabilities,
		MaxPutBytes:   c.s.effectiveLimit(0),
	})
}

func tokenMatch(want, got string) bool {
	if want == "" {
		return false
	}
	wb := []byte(want)
	gb := []byte(got)
	if len(wb) != len(gb) {
		// Compare the expected token with itself so the mismatch path still
		// touches the secret bytes for a similar duration.
		subtle.ConstantTimeCompare(wb, wb)
		return false
	}
	return subtle.ConstantTimeCompare(wb, gb) == 1
}

func (s *Server) healthInfo() HealthInfo {
	info := HealthInfo{
		Version:      Version,
		Protocol:     Protocol,
		UptimeSec:    int64(time.Since(s.started).Seconds()),
		Root:         s.cfg.Root,
		Capabilities: Capabilities,
		Disk:         diskUsage(s.cfg.Root),
	}
	return info
}

func (c *conn) beginPut(id uint32) (*putSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.puts[id]; ok {
		return nil, errors.New("put already in progress")
	}
	sess := &putSession{ch: make(chan Frame, 8), done: make(chan struct{})}
	c.puts[id] = sess
	return sess, nil
}

func (c *conn) endPut(id uint32, sess *putSession) {
	sess.once.Do(func() { close(sess.done) })
	c.mu.Lock()
	if c.puts[id] == sess {
		delete(c.puts, id)
	}
	c.mu.Unlock()
}

func (c *conn) forwardPut(f Frame) {
	c.mu.Lock()
	sess := c.puts[f.ReqID]
	c.mu.Unlock()
	if sess == nil {
		c.sendError(f.ReqID, CodeBadRequest, "no put in progress")
		return
	}
	select {
	case <-sess.done:
	case sess.ch <- f:
	}
}

func (c *conn) cancelPuts() {
	c.mu.Lock()
	all := c.puts
	c.puts = map[uint32]*putSession{}
	c.mu.Unlock()
	for _, sess := range all {
		sess.once.Do(func() { close(sess.done) })
	}
}

func (c *conn) beginExec(id uint32) (*execSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.exec[id]; ok {
		return nil, errors.New("exec already in progress")
	}
	sess := &execSession{done: make(chan struct{})}
	c.exec[id] = sess
	return sess, nil
}

func (c *conn) endExec(id uint32, sess *execSession) {
	close(sess.done)
	c.mu.Lock()
	if c.exec[id] == sess {
		delete(c.exec, id)
	}
	c.mu.Unlock()
}

func (s *execSession) setPID(pid int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pid = pid
	if s.hasPending {
		return SignalGroup(pid, s.pending)
	}
	return nil
}

func (s *execSession) signal(sig syscall.Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return errors.New("exec finished")
	default:
	}
	if s.pid <= 0 {
		s.pending = sig
		s.hasPending = true
		return nil
	}
	return SignalGroup(s.pid, sig)
}

func (c *conn) signalExec(f Frame) {
	if !c.authed() {
		c.sendError(f.ReqID, CodeUnauthorized, "hello required")
		return
	}
	var req SignalRequest
	if err := decode(f.Payload, &req); err != nil {
		c.sendResult(MsgCommandSignalResult, f.ReqID, SignalResult{Code: CodeBadRequest, Error: err.Error()})
		return
	}
	sig, err := parseSignal(req.Signal)
	if err != nil {
		c.sendResult(MsgCommandSignalResult, f.ReqID, SignalResult{Code: CodeBadRequest, Error: err.Error()})
		return
	}
	c.mu.Lock()
	sess := c.exec[f.ReqID]
	c.mu.Unlock()
	if sess == nil {
		c.sendResult(MsgCommandSignalResult, f.ReqID, SignalResult{Code: CodeNotFound, Error: "no such exec"})
		return
	}
	if err := sess.signal(sig); err != nil {
		c.sendResult(MsgCommandSignalResult, f.ReqID, SignalResult{Code: CodeIO, Error: err.Error()})
		return
	}
	c.sendResult(MsgCommandSignalResult, f.ReqID, SignalResult{OK: true})
}

func (c *conn) abortExecs() {
	c.mu.Lock()
	all := make([]*execSession, 0, len(c.exec))
	for _, sess := range c.exec {
		all = append(all, sess)
	}
	c.exec = map[uint32]*execSession{}
	c.mu.Unlock()
	for _, sess := range all {
		_ = sess.signal(syscall.SIGKILL)
	}
}

func (c *conn) runProcessList(f Frame) {
	procs, err := listProcesses()
	if err != nil {
		c.sendError(f.ReqID, CodeIO, err.Error())
		return
	}
	c.sendResult(MsgProcessListResult, f.ReqID, ProcessList{Processes: procs})
}

func (c *conn) runProcessSignal(f Frame, kill bool) {
	var req SignalRequest
	if err := decode(f.Payload, &req); err != nil {
		c.sendResult(MsgProcessSignalResult, f.ReqID, SignalResult{Code: CodeBadRequest, Error: err.Error()})
		return
	}
	sigName := req.Signal
	if kill || sigName == "" {
		sigName = "KILL"
	}
	sig, err := parseSignal(sigName)
	if err != nil {
		c.sendResult(MsgProcessSignalResult, f.ReqID, SignalResult{Code: CodeBadRequest, Error: err.Error()})
		return
	}
	if err := signalPID(req.PID, sig); err != nil {
		code := CodeIO
		if errors.Is(err, os.ErrNotExist) {
			code = CodeNotFound
		}
		c.sendResult(MsgProcessSignalResult, f.ReqID, SignalResult{Code: code, Error: err.Error()})
		return
	}
	c.sendResult(MsgProcessSignalResult, f.ReqID, SignalResult{OK: true})
}

func (c *conn) sendRaw(t MsgType, id uint32, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := WriteFrame(c.bw, Frame{Type: t, ReqID: id, Payload: payload}); err != nil {
		return err
	}
	return c.bw.Flush()
}

func (c *conn) sendError(id uint32, code, message string) {
	c.sendResult(MsgError, id, ErrorBody{Code: code, Message: message})
}

// Ping dials sock and expects Health.Ping to succeed without a token.
// Docker HEALTHCHECK and a future main-process probe both use this.
func Ping(ctx context.Context, sock string) error {
	if sock == "" {
		sock = DefaultSock
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return err
	}
	defer nc.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(3 * time.Second)
	}
	_ = nc.SetDeadline(deadline)
	br := bufio.NewReader(nc)
	bw := bufio.NewWriter(nc)
	if err := WriteFrame(bw, Frame{Type: MsgHealthPing, ReqID: 1}); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	f, err := ReadFrame(br, DefaultMaxFrame)
	if err != nil {
		return err
	}
	if f.Type != MsgHealthPong {
		return errors.New("channel: health ping was not answered")
	}
	var pong HealthPong
	if err := decode(f.Payload, &pong); err != nil {
		return err
	}
	if !pong.OK || pong.Protocol != Protocol {
		return errors.New("channel: health pong not ok")
	}
	return nil
}

// ReadyFile is the sidecar file written next to the socket once Listen succeeds.
func ReadyFile(sock string) string {
	if sock == "" {
		sock = DefaultSock
	}
	return filepath.Join(filepath.Dir(sock), "ready")
}
