package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/secret"
	ulog "github.com/kasuganosora/thinkbot/util/log"
)

// ----------------------------------------------------------------------------
// Minimal SessionHandle for the WebSocket contract tests. It is intentionally
// NOT a mock of sessionCore: the tests exercise the real handler / registry /
// frame protocol, and only the process side is stubbed out.
// ----------------------------------------------------------------------------

type fakePtyHandle struct {
	mu          sync.Mutex
	id          string
	owner       string
	data        []byte
	state       sandbox.SessionState
	input       [][]byte
	cols        uint16
	rows        uint16
	resizeCalls int
	closed      bool
	lastTouched time.Time
}

var _ sandbox.SessionHandle = (*fakePtyHandle)(nil)

func (h *fakePtyHandle) Poll(_ uint64, _ string, _ int, _ time.Duration) (sandbox.SessionView, error) {
	return h.View(), nil
}
func (h *fakePtyHandle) Send(b []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.input = append(h.input, append([]byte(nil), b...))
	return nil
}
func (h *fakePtyHandle) Resize(cols, rows uint16) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cols, h.rows = cols, rows
	h.resizeCalls++
	return nil
}
func (h *fakePtyHandle) Signal(string) error { return nil }
func (h *fakePtyHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	h.state = sandbox.SessionExited
	return nil
}
func (h *fakePtyHandle) View() sandbox.SessionView {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sandbox.SessionView{SessionID: h.id, State: h.state,
		Seq: uint64(len(h.data)), Backend: "test"}
}
func (h *fakePtyHandle) RawSince(seq uint64) sandbox.RawChunk {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur := uint64(len(h.data))
	if seq > cur {
		seq = cur
	}
	return sandbox.RawChunk{Data: append([]byte(nil), h.data[seq:]...),
		Seq: cur, State: h.state, Cols: 80, Rows: 24}
}
func (h *fakePtyHandle) Touch() {
	h.mu.Lock()
	h.lastTouched = time.Now()
	h.mu.Unlock()
}
func (h *fakePtyHandle) LastTouch() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastTouched
}
func (h *fakePtyHandle) BotID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.owner
}
func (h *fakePtyHandle) SetOwner(botID string) {
	h.mu.Lock()
	h.owner = botID
	h.mu.Unlock()
}
func (h *fakePtyHandle) ID() string { return h.id }

func (h *fakePtyHandle) appendOutput(s string) {
	h.mu.Lock()
	h.data = append(h.data, []byte(s)...)
	h.mu.Unlock()
}

type fakePtySpawner struct {
	n    int
	last *fakePtyHandle
}

func (s *fakePtySpawner) Spawn(_ context.Context, _ string, _ sandbox.SpawnRequest) (sandbox.SessionHandle, error) {
	s.n++
	h := &fakePtyHandle{id: fmt.Sprintf("sess-ws-%d", s.n),
		state: sandbox.SessionRunning, lastTouched: time.Now()}
	h.appendOutput("hello\r\n")
	s.last = h
	return h, nil
}

func newPtyTestRegistry(t *testing.T) (*sandbox.SessionRegistry, *fakePtyHandle, string) {
	t.Helper()
	sp := &fakePtySpawner{}
	cfg := sandbox.DefaultSessionConfig()
	reg := sandbox.NewSessionRegistry(cfg, zap.NewNop().Sugar(), sp)
	v, err := reg.Spawn(context.Background(), "bot-a", "default",
		sandbox.SpawnRequest{Command: "sh"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return reg, sp.last, v.SessionID
}

func newPtyTestServer(t *testing.T, reg *sandbox.SessionRegistry) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	srv := &Server{logger: zap.NewNop().Sugar()}
	r.GET("/api/bots/:id/pty-sessions", func(c *gin.Context) {
		OK(c, gin.H{"sessions": reg.List(c.Param("id")), "canManage": true})
	})
	r.POST("/api/bots/:id/pty-sessions", func(c *gin.Context) {
		srv.spawnPtySession(c, reg, c.Param("id"))
	})
	r.GET("/api/bots/:id/pty-sessions/:sid/stream", func(c *gin.Context) {
		srv.servePtySessionStream(c, reg, c.Param("id"), c.Param("sid"))
	})
	r.DELETE("/api/bots/:id/pty-sessions/:sid", func(c *gin.Context) {
		if err := reg.Close(c.Param("id"), c.Param("sid")); err != nil {
			FailMsg(c, http.StatusNotFound, "session not found")
			return
		}
		OK(c, gin.H{"closed": true})
	})
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

func readPtyFrame(t *testing.T, c *websocket.Conn) (int, []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, b, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return mt, b
}

func TestPtySessionUpgrader_CheckOrigin(t *testing.T) {
	cases := []struct {
		name, host, origin string
		want               bool
	}{
		{"same origin", "example.com:8080", "http://example.com:8080", true},
		{"no origin (non-browser)", "example.com:8080", "", true},
		{"foreign origin", "example.com:8080", "https://evil.example", false},
		{"external host with loopback origin", "example.com:8080", "http://localhost:54727", false},
		{"vite dev proxy (changeOrigin rewrites Host)", "localhost:8080", "http://localhost:54727", true},
		{"loopback IP dev proxy", "127.0.0.1:8080", "http://127.0.0.1:54727", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/x", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := ptySessionUpgrader.CheckOrigin(req); got != tc.want {
				t.Errorf("CheckOrigin = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHandleListPtySessions_RequiresRunningBot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name string
		svc  *BotService
		want int
	}{
		{"no bot service", nil, http.StatusServiceUnavailable},
		{"bot not running", &BotService{}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			srv := &Server{logger: zap.NewNop().Sugar(), botSvc: tc.svc}
			r.GET("/api/bots/:id/pty-sessions", srv.handleListPtySessions)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/bots/b1/pty-sessions", nil))
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestPtyCanManage_RoleGating(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		role string
		want bool
	}{
		{auth.RoleAdmin, true},
		{auth.RoleMember, false},
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set(ContextKeyUser, &dao.User{Role: tc.role})
		if got := ptyCanManage(c); got != tc.want {
			t.Errorf("role %q: canManage = %v, want %v", tc.role, got, tc.want)
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if ptyCanManage(c) {
		t.Error("anonymous context must not be able to manage sessions")
	}
}

func TestPtySessionSpawn_ViaREST(t *testing.T) {
	reg, _, _ := newPtyTestRegistry(t)
	ts := newPtyTestServer(t, reg)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/bots/bot-a/pty-sessions",
		strings.NewReader(`{"command":"sh","cols":100,"rows":30}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("spawn status = %d", res.StatusCode)
	}
	var out struct {
		Code int                 `json:"code"`
		Data sandbox.SessionView `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Data.SessionID == "" || out.Data.State != sandbox.SessionRunning {
		t.Fatalf("spawn result = %+v", out.Data)
	}
	if reg.Count("bot-a") != 2 {
		t.Errorf("registry count = %d, want 2", reg.Count("bot-a"))
	}

	// Empty command must be rejected before reaching the registry.
	bad, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/bots/bot-a/pty-sessions",
		strings.NewReader(`{"command":"   "}`))
	bad.Header.Set("Content-Type", "application/json")
	badRes, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatalf("bad spawn: %v", err)
	}
	badRes.Body.Close()
	if badRes.StatusCode != http.StatusBadRequest {
		t.Errorf("empty command status = %d, want 400", badRes.StatusCode)
	}
}

func TestPtySessionStream_ReplayInputResizeAndExplicitClose(t *testing.T) {
	reg, h, sid := newPtyTestRegistry(t)
	ts := newPtyTestServer(t, reg)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/api/bots/bot-a/pty-sessions/" + sid + "/stream"

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer conn.Close()

	// Fresh attach must replay already-produced output as BINARY, then report
	// state. Neither frame may be an ANSI-free re-render of the semantic view.
	var gotBinary, gotState bool
	for i := 0; i < 2 && !(gotBinary && gotState); i++ {
		mt, b := readPtyFrame(t, conn)
		switch mt {
		case websocket.BinaryMessage:
			if !strings.Contains(string(b), "hello") {
				t.Errorf("binary replay = %q", b)
			}
			gotBinary = true
		case websocket.TextMessage:
			var f ptyServerFrame
			if err := json.Unmarshal(b, &f); err != nil {
				t.Fatalf("bad control frame: %v", err)
			}
			if f.Type != ptyFrameState || f.State != string(sandbox.SessionRunning) {
				t.Errorf("first control frame = %+v", f)
			}
			gotState = true
		default:
			t.Fatalf("unexpected frame type %d", mt)
		}
	}
	if !gotBinary || !gotState {
		t.Fatalf("missing frames: binary=%v state=%v", gotBinary, gotState)
	}

	// Agent-side output keeps streaming to the human tab without a new attach.
	h.appendOutput("AFTER\r\n")

	input := base64.StdEncoding.EncodeToString([]byte("ping\n"))
	if err := conn.WriteJSON(ptyClientFrame{Type: ptyFrameInput, Data: input}); err != nil {
		t.Fatalf("send input frame: %v", err)
	}
	if err := conn.WriteJSON(ptyClientFrame{Type: ptyFrameResize, Cols: 100, Rows: 30}); err != nil {
		t.Fatalf("send resize frame: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		okInput := len(h.input) == 1 && string(h.input[0]) == "ping\n"
		okResize := h.cols == 100 && h.rows == 30
		h.mu.Unlock()
		if okInput && okResize {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	if len(h.input) != 1 || string(h.input[0]) != "ping\n" {
		t.Errorf("input not delivered: %q", h.input)
	}
	if h.cols != 100 || h.rows != 30 {
		t.Errorf("resize not delivered: %dx%d", h.cols, h.rows)
	}
	h.mu.Unlock()

	// Explicit close is the ONLY human path that terminates a session.
	if err := conn.WriteJSON(ptyClientFrame{Type: ptyFrameClose}); err != nil {
		t.Fatalf("send close frame: %v", err)
	}
	sawExited := false
	for i := 0; i < 5; i++ {
		mt, b, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt == websocket.BinaryMessage {
			continue // trailing output replay
		}
		var f ptyServerFrame
		if json.Unmarshal(b, &f) == nil && f.Type == ptyFrameState &&
			f.State == string(sandbox.SessionExited) {
			sawExited = true
			break
		}
	}
	if !sawExited {
		t.Error("explicit close did not produce a state=exited frame")
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if !closed {
		t.Error("explicit close did not reach SessionRegistry.Close")
	}
}

func TestPtySessionStream_RejectsCrossBotBeforeUpgrade(t *testing.T) {
	reg, _, sid := newPtyTestRegistry(t)
	ts := newPtyTestServer(t, reg)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/api/bots/bot-b/pty-sessions/" + sid + "/stream"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		conn.Close()
		t.Fatal("cross-bot stream must not upgrade")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403 (err=%v)", resp, err)
	}
	if reg.Count("bot-a") != 1 {
		t.Error("rejected stream mutated the registry")
	}
}

func TestPtySessionListAndDelete_AreScopedToBot(t *testing.T) {
	reg, h, sid := newPtyTestRegistry(t)
	ts := newPtyTestServer(t, reg)

	// List is pure read-only: it must not close or GC anything.
	resp, err := http.Get(ts.URL + "/api/bots/bot-a/pty-sessions")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var body struct {
		Code int `json:"code"`
		Data struct {
			Sessions []sandbox.SessionView `json:"sessions"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(body.Data.Sessions) != 1 || body.Data.Sessions[0].SessionID != sid {
		t.Fatalf("list = %+v, want one session %s", body.Data.Sessions, sid)
	}
	if reg.Count("bot-a") != 1 {
		t.Error("listing GC'd the registry")
	}

	// Cross-bot delete must be rejected by the registry's ownership check.
	req, _ := http.NewRequest(http.MethodDelete,
		ts.URL+"/api/bots/bot-b/pty-sessions/"+sid, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cross-bot delete: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("cross-bot delete status = %d, want 404", res.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodDelete,
		ts.URL+"/api/bots/bot-a/pty-sessions/"+sid, nil)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("delete status = %d, want 200", res.StatusCode)
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if !closed {
		t.Error("REST delete did not close the session")
	}
}

func TestPtySessionStream_DisconnectDoesNotCloseSession(t *testing.T) {
	reg, h, sid := newPtyTestRegistry(t)
	ts := newPtyTestServer(t, reg)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/api/bots/bot-a/pty-sessions/" + sid + "/stream"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	readPtyFrame(t, conn)
	_ = conn.Close() // tab closed WITHOUT a close frame
	time.Sleep(120 * time.Millisecond)
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		t.Error("WS detach killed the session; it must outlive the browser tab")
	}
	if reg.Count("bot-a") != 1 {
		t.Error("WS detach removed the session from the registry")
	}
}

// Regression: a real browser produced 4434 resize frames in ~3 minutes because
// ResizeObserver + xterm fit() fed back into an auto-height container. Even
// with the frontend fixed, the server must collapse no-op frames and clamp
// untrusted winsize before it reaches pty.Setsize.
func TestPtySessionStream_ResizeDedupeAndClamp(t *testing.T) {
	reg, h, sid := newPtyTestRegistry(t)
	ts := newPtyTestServer(t, reg)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/api/bots/bot-a/pty-sessions/" + sid + "/stream"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	frames := []ptyClientFrame{
		{Type: ptyFrameResize, Cols: 100, Rows: 30},
		{Type: ptyFrameResize, Cols: 100, Rows: 30}, // exact duplicate
		{Type: ptyFrameResize, Cols: 100, Rows: 30},
		{Type: ptyFrameResize, Cols: 118, Rows: 3286},  // runaway browser value
		{Type: ptyFrameResize, Cols: 118, Rows: 9000},  // different raw, same clamped
		{Type: ptyFrameResize, Cols: 9000, Rows: 9000}, // still distinct after clamp
	}
	for _, f := range frames {
		if err := conn.WriteJSON(f); err != nil {
			t.Fatalf("send resize frame: %v", err)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := h.resizeCalls
		h.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.resizeCalls != 3 {
		t.Fatalf("handle Resize called %d times, want 3 (100x30, 118x500, 1000x500)", h.resizeCalls)
	}
	if h.cols != sandbox.MaxSessionCols || h.rows != sandbox.MaxSessionRows {
		t.Errorf("final winsize = %dx%d, want %dx%d",
			h.cols, h.rows, sandbox.MaxSessionCols, sandbox.MaxSessionRows)
	}
}

// A credential registered AFTER the bytes were produced is the one case the
// sandbox ingress masker cannot cover. On the Web console those bytes arrive
// in 50ms RawSince ticks, so a value split across two ticks would be emitted
// in plaintext halves unless the WS stream carries the withheld suffix over.
func TestPtySessionStream_MasksLateRegisteredCredentialAcrossFrames(t *testing.T) {
	const value = "WS-LATE-CRED-4242"
	prev := secret.Default()
	v, err := secret.New("") // in-memory vault
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	if err := v.Register("bot-a", "ws.pin", value, "late-registered credential"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	secret.SetDefault(v)
	t.Cleanup(func() {
		secret.SetDefault(prev)
		ulog.UnregisterSecret(value)
	})

	reg, h, sid := newPtyTestRegistry(t)
	ts := newPtyTestServer(t, reg)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/api/bots/bot-a/pty-sessions/" + sid + "/stream"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer conn.Close()

	// Two separate ticks: the first ends mid-credential.
	h.appendOutput("PIN?" + value[:8])
	time.Sleep(180 * time.Millisecond)
	h.appendOutput(value[8:] + "\r\n")
	time.Sleep(180 * time.Millisecond)
	if err := conn.WriteJSON(ptyClientFrame{Type: ptyFrameClose}); err != nil {
		t.Fatalf("close frame: %v", err)
	}

	var binary strings.Builder
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		mt, b, err := conn.ReadMessage()
		if err != nil {
			break // explicit close ends the stream
		}
		if mt == websocket.BinaryMessage {
			binary.Write(b)
		}
	}
	stream := binary.String()
	if strings.Contains(stream, value) {
		t.Fatalf("plaintext credential reached the browser: %q", stream)
	}
	// Neither half may be emitted verbatim: a partial credential is still
	// credential material, and the two halves together reconstruct the value.
	if strings.Contains(stream, value[:8]) || strings.Contains(stream, value[8:]) {
		t.Fatalf("a credential half leaked in a frame: %q", stream)
	}
	if !strings.Contains(stream, secret.Placeholder("ws.pin")) && !strings.Contains(stream, "***") {
		t.Fatalf("no redaction marker in the WS stream: %q", stream)
	}
	if !strings.Contains(stream, "hello") || !strings.Contains(stream, "PIN?") {
		t.Fatalf("normal terminal output was lost: %q", stream)
	}
}
