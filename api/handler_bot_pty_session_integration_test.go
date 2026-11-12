//go:build unix

package api

// End-to-end: real PTY-backed shell -> SessionRegistry.RawSince -> WebSocket
// binary frames -> human input back into the shell. The fake-handle tests cover
// the frame contract; this one proves the actual process side is wired up.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/sandbox"
)

func TestPtySessionStream_RealPTYRoundTrip(t *testing.T) {
	cfg := sandbox.DefaultConfig()
	cfg.Backend = "local" // never touch Docker in this test
	cfg.Session.IdleTTL = 30 * time.Second
	cfg.Session.TermGrace = 200 * time.Millisecond
	mgr, err := sandbox.NewBotWorkspaceManager(t.TempDir(), cfg, zap.NewNop().Sugar())
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}
	defer mgr.CloseAll()
	reg := mgr.Sessions()
	if reg == nil {
		t.Fatal("session registry is nil")
	}

	const botID = "bot-pty-e2e"
	// A unique marker proves the bytes reached the CHILD, not just the PTY echo.
	v, err := reg.Spawn(context.Background(), botID, "default", sandbox.SpawnRequest{
		Command: `while IFS= read -r l; do echo "GOT:$l"; done`,
	})
	if err != nil {
		t.Fatalf("spawn real PTY session: %v", err)
	}
	if v.PID <= 0 {
		t.Fatalf("spawn returned no pid: %+v", v)
	}

	ts := newPtyTestServer(t, reg)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") +
		"/api/bots/" + botID + "/pty-sessions/" + v.SessionID + "/stream"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	payload := base64.StdEncoding.EncodeToString([]byte("PING42\n"))
	if err := conn.WriteJSON(ptyClientFrame{Type: ptyFrameInput, Data: payload}); err != nil {
		t.Fatalf("send input: %v", err)
	}

	deadline := time.Now().Add(8 * time.Second)
	seen := ""
	for time.Now().Before(deadline) && !strings.Contains(seen, "GOT:PING42") {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		mt, b, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read while waiting for shell echo: %v (seen=%q)", err, seen)
		}
		if mt == websocket.BinaryMessage {
			seen += string(b)
		}
	}
	if !strings.Contains(seen, "GOT:PING42") {
		t.Fatalf("input never reached the real PTY child; output=%q", seen)
	}

	// Resizing must not break the stream; the daemon-side grid is covered by
	// sandbox/session_buffer_test.go, here we only assert the frame is accepted.
	if err := conn.WriteJSON(ptyClientFrame{Type: ptyFrameResize, Cols: 100, Rows: 30}); err != nil {
		t.Fatalf("send resize: %v", err)
	}
	if err := conn.WriteJSON(ptyClientFrame{Type: ptyFrameInput,
		Data: base64.StdEncoding.EncodeToString([]byte("AFTER-RESIZE\n"))}); err != nil {
		t.Fatalf("send second input: %v", err)
	}
	seen = ""
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(seen, "GOT:AFTER-RESIZE") {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		mt, b, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read after resize: %v (seen=%q)", err, seen)
		}
		if mt == websocket.BinaryMessage {
			seen += string(b)
		}
	}
	if !strings.Contains(seen, "GOT:AFTER-RESIZE") {
		t.Errorf("session stopped responding after resize; output=%q", seen)
	}

	// Explicit close terminates the process group, not just the socket.
	if err := conn.WriteJSON(ptyClientFrame{Type: ptyFrameClose}); err != nil {
		t.Fatalf("send close: %v", err)
	}
	closed := false
	for i := 0; i < 8; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		mt, b, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt != websocket.TextMessage {
			continue
		}
		var f ptyServerFrame
		if json.Unmarshal(b, &f) == nil && f.Type == ptyFrameState &&
			f.State == string(sandbox.SessionExited) {
			closed = true
			break
		}
	}
	if !closed {
		t.Error("real PTY session did not report state=exited after close")
	}
	if reg.Count(botID) != 0 {
		t.Error("closed session is still registered")
	}
}
