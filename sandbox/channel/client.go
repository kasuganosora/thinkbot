package channel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// DialOptions authenticates the connection. An empty Token skips Hello; the
// connection can still Health.Ping, and every other call fails until Hello.
type DialOptions struct {
	Token         string
	BotID         string
	ClientVersion string
}

// Client is one unix connection. RPCs on a single client run one at a time.
// Command.Exec may send Command.Signal on the same connection when its context
// is cancelled. A second connection is required to list or signal processes
// while an exec is streaming.
type Client struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	wmu  sync.Mutex
	rpc  sync.Mutex
	next atomic.Uint32
}

// Dial connects to sock and, when Token is set, completes Hello.
func Dial(ctx context.Context, sock string, opt DialOptions) (*Client, error) {
	if sock == "" {
		sock = DefaultSock
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: nc, br: bufio.NewReader(nc), bw: bufio.NewWriter(nc)}
	if opt.Token == "" && opt.BotID == "" {
		return c, nil
	}
	if err := c.hello(ctx, opt); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return c, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) hello(ctx context.Context, opt DialOptions) error {
	ver := opt.ClientVersion
	if ver == "" {
		ver = Version
	}
	id := c.id()
	if err := c.sendJSON(MsgHello, id, Hello{Token: opt.Token, ClientVersion: ver, BotID: opt.BotID}); err != nil {
		return err
	}
	f, err := c.readFrame(ctx)
	if err != nil {
		return err
	}
	if f.ReqID != id && f.Type != MsgError {
		return errors.New("channel: hello ack for a different request")
	}
	if f.Type == MsgError {
		return rpcFrom(f)
	}
	if f.Type != MsgHelloAck {
		return errors.New("channel: expected hello ack")
	}
	return nil
}

// Info returns Health.Info. The connection must already be authenticated.
func (c *Client) Info(ctx context.Context) (HealthInfo, error) {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	var info HealthInfo
	err := c.roundTrip(ctx, MsgHealthInfo, c.id(), struct{}{}, MsgHealthInfoResult, &info)
	return info, err
}

// Put streams r to path. A limit failure removes the partial file in the sandbox.
func (c *Client) Put(ctx context.Context, path string, r io.Reader, opt PutOpen) (int64, error) {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	opt.Path = path
	id := c.id()
	if err := c.sendJSON(MsgFilePutOpen, id, opt); err != nil {
		return 0, err
	}
	buf := make([]byte, chunkSize)
	for {
		if err := ctx.Err(); err != nil {
			_ = c.sendJSON(MsgFilePutAbort, id, struct{}{})
			_, _ = c.readType(context.Background(), id, MsgFilePutResult)
			return 0, err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if werr := c.sendRaw(MsgFilePutData, id, buf[:n]); werr != nil {
				return 0, werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = c.sendJSON(MsgFilePutAbort, id, struct{}{})
			return 0, err
		}
	}
	if err := c.sendJSON(MsgFilePutCommit, id, struct{}{}); err != nil {
		return 0, err
	}
	var res PutResult
	f, err := c.readType(ctx, id, MsgFilePutResult)
	if err != nil {
		return 0, err
	}
	if err := decode(f.Payload, &res); err != nil {
		return 0, err
	}
	if res.Code != "" {
		return res.Bytes, &RPCError{Code: res.Code, Message: res.Error}
	}
	return res.Bytes, nil
}

// Get streams path into w.
func (c *Client) Get(ctx context.Context, path string, w io.Writer, opt GetOpen) (int64, error) {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	opt.Path = path
	id := c.id()
	if err := c.sendJSON(MsgFileGetOpen, id, opt); err != nil {
		return 0, err
	}
	var got int64
	for {
		f, err := c.readFrame(ctx)
		if err != nil {
			return got, err
		}
		if f.ReqID != id && f.Type != MsgError {
			continue
		}
		switch f.Type {
		case MsgFileGetData:
			n, err := w.Write(f.Payload)
			got += int64(n)
			if err != nil {
				return got, err
			}
		case MsgFileGetEnd:
			var end GetEnd
			if err := decode(f.Payload, &end); err != nil {
				return got, err
			}
			if end.Code != "" {
				return got, &RPCError{Code: end.Code, Message: end.Error}
			}
			return got, nil
		case MsgError:
			return got, rpcFrom(f)
		default:
			return got, errors.New("channel: unexpected frame during get")
		}
	}
}

// List returns directory entries sorted by name.
func (c *Client) List(ctx context.Context, path string) ([]FileEntry, error) {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	var res ListResult
	err := c.roundTrip(ctx, MsgFileList, c.id(), ListRequest{Path: path}, MsgFileListResult, &res)
	return res.Entries, err
}

// Delete removes a file, or a directory when recursive is set.
func (c *Client) Delete(ctx context.Context, path string, recursive bool) error {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	var res OkResult
	return c.roundTrip(ctx, MsgFileDelete, c.id(), DeleteRequest{Path: path, Recursive: recursive}, MsgFileDeleteResult, &res)
}

// Stat returns metadata for path.
func (c *Client) Stat(ctx context.Context, path string) (StatResult, error) {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	var res StatResult
	err := c.roundTrip(ctx, MsgFileStat, c.id(), StatRequest{Path: path}, MsgFileStatResult, &res)
	return res, err
}

// Exec runs a non-interactive command. onChunk may be nil. Cancelling ctx
// signals the process group with SIGKILL and still waits for the exit frame.
func (c *Client) Exec(ctx context.Context, req ExecRequest, onChunk func(stream string, data []byte)) (ExecResult, error) {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	id := c.id()
	if err := c.sendJSON(MsgCommandExec, id, req); err != nil {
		return ExecResult{}, err
	}
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			_ = c.sendJSON(MsgCommandSignal, id, SignalRequest{Signal: "KILL"})
		case <-stop:
		}
	}()
	defer func() {
		close(stop)
		<-finished
	}()
	// Read with a detached context. Cancelling ctx must kill the command and
	// still deliver the exit frame; it must not abort the read itself.
	var res ExecResult
	for {
		f, err := c.readFrame(context.Background())
		if err != nil {
			return res, err
		}
		if f.ReqID != id && f.Type != MsgError {
			continue
		}
		switch f.Type {
		case MsgCommandStdout:
			if onChunk != nil && len(f.Payload) > 0 {
				onChunk("stdout", f.Payload)
			}
		case MsgCommandStderr:
			if onChunk != nil && len(f.Payload) > 0 {
				onChunk("stderr", f.Payload)
			}
		case MsgCommandSignalResult:
			// The cancel goroutine's ack. Keep reading until exit.
		case MsgCommandExit:
			if err := decode(f.Payload, &res); err != nil {
				return res, err
			}
			if res.Code != "" && res.ExitCode < 0 && res.Error != "" {
				return res, &RPCError{Code: res.Code, Message: res.Error}
			}
			return res, nil
		case MsgError:
			return res, rpcFrom(f)
		default:
			return res, errors.New("channel: unexpected frame during exec")
		}
	}
}

// Processes lists processes in the container.
func (c *Client) Processes(ctx context.Context) ([]Process, error) {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	var res ProcessList
	err := c.roundTrip(ctx, MsgProcessList, c.id(), struct{}{}, MsgProcessListResult, &res)
	return res.Processes, err
}

// SignalProcess sends sig to pid. pid 1 and the daemon itself are refused.
func (c *Client) SignalProcess(ctx context.Context, pid int, sig string) error {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	var res SignalResult
	if err := c.roundTrip(ctx, MsgProcessSignal, c.id(), SignalRequest{PID: pid, Signal: sig}, MsgProcessSignalResult, &res); err != nil {
		return err
	}
	if !res.OK {
		code := res.Code
		if code == "" {
			code = CodeIO
		}
		return &RPCError{Code: code, Message: res.Error}
	}
	return nil
}

// KillProcess sends SIGKILL.
func (c *Client) KillProcess(ctx context.Context, pid int) error {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	var res SignalResult
	if err := c.roundTrip(ctx, MsgProcessKill, c.id(), SignalRequest{PID: pid}, MsgProcessSignalResult, &res); err != nil {
		return err
	}
	if !res.OK {
		code := res.Code
		if code == "" {
			code = CodeIO
		}
		return &RPCError{Code: code, Message: res.Error}
	}
	return nil
}

// Shutdown asks the daemon to stop. The daemon process exits only if its
// main is wired to OnShutdown; the RPC itself just acknowledges.
func (c *Client) Shutdown(ctx context.Context) error {
	c.rpc.Lock()
	defer c.rpc.Unlock()
	var res OkResult
	return c.roundTrip(ctx, MsgShutdown, c.id(), struct{}{}, MsgShutdownAck, &res)
}

func (c *Client) roundTrip(ctx context.Context, reqType MsgType, id uint32, body any, resp MsgType, out any) error {
	if err := c.sendJSON(reqType, id, body); err != nil {
		return err
	}
	f, err := c.readType(ctx, id, resp)
	if err != nil {
		return err
	}
	return decode(f.Payload, out)
}

func (c *Client) readType(ctx context.Context, id uint32, want MsgType) (Frame, error) {
	for {
		f, err := c.readFrame(ctx)
		if err != nil {
			return Frame{}, err
		}
		if f.Type == MsgError && (f.ReqID == id || f.ReqID == 0) {
			return Frame{}, rpcFrom(f)
		}
		if f.ReqID != id {
			continue
		}
		if f.Type != want {
			return Frame{}, errors.New("channel: unexpected frame")
		}
		return f, nil
	}
}

func (c *Client) id() uint32 {
	n := c.next.Add(1)
	if n == 0 {
		n = c.next.Add(1)
	}
	return n
}

func (c *Client) sendJSON(t MsgType, id uint32, body any) error {
	payload, err := encode(body)
	if err != nil {
		return err
	}
	return c.sendRaw(t, id, payload)
}

func (c *Client) sendRaw(t MsgType, id uint32, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := WriteFrame(c.bw, Frame{Type: t, ReqID: id, Payload: payload}); err != nil {
		return err
	}
	return c.bw.Flush()
}

func (c *Client) readFrame(ctx context.Context) (Frame, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetReadDeadline(deadline)
	} else {
		_ = c.conn.SetReadDeadline(time.Time{})
	}
	return ReadFrame(c.br, DefaultMaxFrame)
}

func rpcFrom(f Frame) error {
	var body ErrorBody
	if err := decode(f.Payload, &body); err != nil {
		return err
	}
	if body.Code == "" {
		body.Code = CodeIO
	}
	return &RPCError{Code: body.Code, Message: body.Message}
}
