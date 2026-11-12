//go:build unix

package sandbox

// These tests drive the real host-side session state machine against a fake
// channel daemon over net.Pipe. No Linux container is needed, so the channel
// path is actually exercised on darwin too (docs §11 baseline discipline).

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
)

type fakeChannelDaemon struct {
	nc       net.Conn
	mu       sync.Mutex
	reqID    uint32
	stdin    chan string
	spawnReq channel.SessionSpawnRequest
}

func newFakeChannelDaemon(t *testing.T, nc net.Conn, result channel.SessionSpawnResult) *fakeChannelDaemon {
	t.Helper()
	f, err := channel.ReadFrame(nc, 0)
	if err != nil {
		t.Fatalf("fake daemon read spawn: %v", err)
	}
	if f.Type != channel.MsgSessionSpawn {
		t.Fatalf("first frame type = %d, want MsgSessionSpawn", f.Type)
	}
	d := &fakeChannelDaemon{nc: nc, reqID: f.ReqID, stdin: make(chan string, 8)}
	if err := json.Unmarshal(f.Payload, &d.spawnReq); err != nil {
		t.Fatalf("decode spawn request: %v", err)
	}
	d.send(channel.MsgSessionSpawnResult, result)
	if result.Code == "" && result.Error == "" {
		go d.readLoop()
	}
	return d
}

func (d *fakeChannelDaemon) send(typ channel.MsgType, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// net.Pipe has no buffer: a writer whose peer stopped reading must not
	// block the test forever.
	_ = d.nc.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = channel.WriteFrame(d.nc, channel.Frame{Type: typ, ReqID: d.reqID, Payload: payload})
}

func (d *fakeChannelDaemon) readLoop() {
	for {
		f, err := channel.ReadFrame(d.nc, 0)
		if err != nil {
			return
		}
		switch f.Type {
		case channel.MsgSessionStdin:
			var sd channel.SessionData
			if json.Unmarshal(f.Payload, &sd) == nil {
				select {
				case d.stdin <- string(sd.Data):
				default:
				}
			}
		case channel.MsgSessionClose:
			return
		}
	}
}

func (d *fakeChannelDaemon) drop() { _ = d.nc.Close() }

func fakeSessionDialer(t *testing.T, result channel.SessionSpawnResult) (channelSessionDialer, func() *fakeChannelDaemon) {
	t.Helper()
	var (
		mu sync.Mutex
		d  *fakeChannelDaemon
	)
	dial := func(ctx context.Context, botID string, req channel.SessionSpawnRequest,
		cb channel.SessionCallbacks) (*channel.SessionClient, error) {
		srv, cli := net.Pipe()
		type dialResult struct {
			sc  *channel.SessionClient
			err error
		}
		resultCh := make(chan dialResult, 1)
		// DialSession writes the spawn frame; net.Pipe is unbuffered, so the
		// fake daemon must be reading at the same time.
		go func() {
			sc, err := channel.DialSession(ctx, cli, channel.DialOptions{}, req, cb)
			resultCh <- dialResult{sc: sc, err: err}
		}()
		dd := newFakeChannelDaemon(t, srv, result)
		mu.Lock()
		d = dd
		mu.Unlock()
		dr := <-resultCh
		if dr.err != nil {
			_ = srv.Close()
			return nil, dr.err
		}
		return dr.sc, nil
	}
	return dial, func() *fakeChannelDaemon {
		mu.Lock()
		defer mu.Unlock()
		return d
	}
}

func waitSessionState(t *testing.T, h SessionHandle, want SessionState, timeout time.Duration) SessionView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		v := h.View()
		if v.State == want {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state never became %q; last view = %+v", want, h.View())
	return SessionView{}
}

func TestChannelSession_OutputSendAndCleanExit(t *testing.T) {
	cfg := testSessionConfig()
	dial, daemon := fakeSessionDialer(t, channel.SessionSpawnResult{PID: 4242})
	sp := newChannelSessionSpawner(cfg, zap.NewNop().Sugar(), dial)
	h, err := sp.Spawn(context.Background(), "bot-a", SpawnRequest{Command: "sh"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer h.Close()
	d := daemon()
	if d == nil {
		t.Fatal("fake daemon was not started")
	}
	if d.spawnReq.IdleTTLMs != cfg.IdleTTL.Milliseconds() {
		t.Errorf("idleTtlMs = %d, want %d", d.spawnReq.IdleTTLMs, cfg.IdleTTL.Milliseconds())
	}
	if d.spawnReq.Cols != cfg.ScreenCols || d.spawnReq.Rows != cfg.ScreenRows {
		t.Errorf("winsize not forwarded: %+v", d.spawnReq)
	}

	d.send(channel.MsgSessionOutput, channel.SessionData{Data: []byte("\x1b[2J\x1b[Hhello channel> ")})
	d.send(channel.MsgSessionDiag, channel.SessionDiag{Data: "boot ok"})

	v, err := h.Poll(0, "screen", 40, 2*time.Second)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !strings.Contains(v.Screen+v.Delta, "hello channel") {
		t.Fatalf("output missing from view: screen=%q delta=%q", v.Screen, v.Delta)
	}
	if v.Backend != "channel" || v.PID != 4242 {
		t.Errorf("backend/pid = %q/%d, want channel/4242", v.Backend, v.PID)
	}
	if v.PGID != 0 {
		t.Errorf("host must not expose a container pgid, got %d", v.PGID)
	}
	if !strings.Contains(v.BootDiag, "boot ok") {
		t.Errorf("diagnostic side channel lost: %q", v.BootDiag)
	}
	if v.Seq == 0 || v.SettledSeq > v.Seq {
		t.Errorf("double watermark broken: seq=%d settledSeq=%d", v.Seq, v.SettledSeq)
	}

	if err := h.Send([]byte("y\r")); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case got := <-d.stdin:
		if got != "y\r" {
			t.Errorf("stdin frame = %q, want %q", got, "y\r")
		}
	case <-time.After(2 * time.Second):
		t.Error("stdin never reached the daemon")
	}

	d.send(channel.MsgSessionExit, channel.SessionExit{ExitCode: 0})
	final := waitSessionState(t, h, SessionExited, 3*time.Second)
	if final.Reason != SessionReasonExit {
		t.Errorf("reason = %q, want %q", final.Reason, SessionReasonExit)
	}
	if final.ExitCode == nil || *final.ExitCode != 0 {
		t.Errorf("exitCode = %v, want 0", final.ExitCode)
	}
}

func TestChannelSession_ConnectionLossIsLostNotExited(t *testing.T) {
	dial, daemon := fakeSessionDialer(t, channel.SessionSpawnResult{PID: 77})
	h, err := newChannelSessionSpawner(testSessionConfig(), zap.NewNop().Sugar(), dial).
		Spawn(context.Background(), "bot-a", SpawnRequest{Command: "sh"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer h.Close()
	d := daemon()
	d.send(channel.MsgSessionOutput, channel.SessionData{Data: []byte("partial output\n")})

	// Wait until the bytes actually landed, then kill the connection.
	deadline := time.Now().Add(2 * time.Second)
	for h.View().Seq == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	d.drop()

	v := waitSessionState(t, h, SessionLost, 3*time.Second)
	if v.Reason != SessionReasonConnLost {
		t.Errorf("reason = %q, want %q", v.Reason, SessionReasonConnLost)
	}
	if v.LastSeq == 0 {
		t.Error("state=lost must carry lastSeq so the agent can reopen without guessing")
	}
	if !strings.Contains(v.Hint, "reopen") {
		t.Errorf("lost hint missing: %q", v.Hint)
	}
	if err := h.Send([]byte("x")); err != ErrSessionNotRunning {
		t.Errorf("send on lost session = %v, want ErrSessionNotRunning", err)
	}
}

func TestChannelSession_DaemonIdleTTL(t *testing.T) {
	dial, daemon := fakeSessionDialer(t, channel.SessionSpawnResult{PID: 88})
	h, err := newChannelSessionSpawner(testSessionConfig(), zap.NewNop().Sugar(), dial).
		Spawn(context.Background(), "bot-a", SpawnRequest{Command: "sh"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer h.Close()
	daemon().send(channel.MsgSessionExit, channel.SessionExit{ExitCode: -1, Code: "idle_ttl"})

	v := waitSessionState(t, h, SessionExited, 3*time.Second)
	if v.Reason != SessionReasonIdleTTL {
		t.Errorf("reason = %q, want %q", v.Reason, SessionReasonIdleTTL)
	}
}

func TestChannelSession_SpawnFailureDoesNotRegister(t *testing.T) {
	dial, _ := fakeSessionDialer(t, channel.SessionSpawnResult{
		Code: channel.CodeUnsupported, Error: "PTY unavailable",
	})
	sp := newChannelSessionSpawner(testSessionConfig(), zap.NewNop().Sugar(), dial)
	reg := NewSessionRegistry(testSessionConfig(), zap.NewNop().Sugar(), sp)
	if _, err := reg.Spawn(context.Background(), "bot-a", "default",
		SpawnRequest{Command: "sh"}); err == nil {
		t.Fatal("unsupported daemon must fail the spawn, not return an empty running session")
	}
	if n := reg.Count(""); n != 0 {
		t.Errorf("failed spawn registered %d sessions", n)
	}
}

type recordingSpawner struct {
	calls int
	err   error
}

func (s *recordingSpawner) Spawn(ctx context.Context, botID string, req SpawnRequest) (SessionHandle, error) {
	s.calls++
	return nil, s.err
}

func TestRoutingSessionSpawner_ChannelBotNeverFallsBackToHostPTY(t *testing.T) {
	local := &recordingSpawner{err: errors.New("local used")}
	remote := &recordingSpawner{err: errors.New("channel used")}
	r := &routingSessionSpawner{
		local: local, channel: remote,
		channelFor: func(botID string) bool { return botID == "channel-bot" },
	}
	_, _ = r.Spawn(context.Background(), "local-bot", SpawnRequest{Command: "sh"})
	_, _ = r.Spawn(context.Background(), "channel-bot", SpawnRequest{Command: "sh"})
	if local.calls != 1 || remote.calls != 1 {
		t.Fatalf("routing wrong: local=%d channel=%d", local.calls, remote.calls)
	}
}
