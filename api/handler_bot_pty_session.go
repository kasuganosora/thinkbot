package api

// ============================================================================
// Human takeover of agent-owned interactive PTY sessions (docs §10.1 P2).
//
//   GET /api/bots/:id/pty-sessions              -> list (read-only)
//   GET /api/bots/:id/pty-sessions/:sid/stream  -> WebSocket, raw PTY bytes
//
// ⚠️ The path is deliberately NOT /api/bots/:id/sessions: router.go already
// registers that for CHAT session CRUD (handleListSessions/CreateSession).
//
// Ownership rules (docs §3.7):
//   * SessionRegistry is the only process owner. This handler never spawns,
//     never signals by pid, and never holds a SessionHandle.
//   * A WebSocket attach/detach MUST NOT close the session. Closing the tab
//     only closes the socket; the session outlives it for the agent.
//   * Human input bypasses LLM toolperm (which adjudicates tool calls only),
//     so both routes sit in botsAdmin behind PermBotManage and every input is
//     audit-logged.
// ============================================================================

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/kasuganosora/thinkbot/auth"
	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/util/errs"
)

const (
	ptyFrameReset  = "reset"
	ptyFrameState  = "state"
	ptyFrameError  = "error"
	ptyFrameInput  = "input"
	ptyFrameResize = "resize"
	ptyFrameClose  = "close"

	ptyStreamTick       = 50 * time.Millisecond
	ptyStreamPingEvery  = 15 * time.Second
	ptyStreamWriteLimit = 5 * time.Second
	ptyStreamReadLimit  = 1 << 20 // 1 MiB per client frame
	ptyStreamReadTTL    = 60 * time.Second
)

// ptyClientFrame is what the browser sends (JSON text frame).
type ptyClientFrame struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"` // base64 raw terminal bytes
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// ptyServerFrame is what the browser receives (JSON control frame). Terminal
// bytes themselves go out as BINARY frames to avoid any encoding round-trip.
type ptyServerFrame struct {
	Type    string `json:"type"`
	Seq     uint64 `json:"seq,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	State   string `json:"state,omitempty"`
	Message string `json:"message,omitempty"`
}

// ptySessionUpgrader keeps the same-origin rule of desktopUpgrader but also
// allows the local dev server: the vite /api proxy sets changeOrigin:true, so
// Host becomes the backend port while Origin keeps the dev port. That narrow
// allowance is gated on BOTH endpoints being loopback, and the route itself is
// already behind cookie auth + PermBotManage.
var ptySessionUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 32 * 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // non-browser clients (curl / tests)
		}
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return false
		}
		if u.Host == r.Host {
			return true
		}
		return isLoopbackHost(hostOf(r.Host)) && isLoopbackHost(u.Hostname())
	},
}

func hostOf(authority string) string {
	h, _, err := net.SplitHostPort(authority)
	if err == nil {
		return h
	}
	return authority
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ptyStreamWriter serializes all socket writes. gorilla allows WriteControl to
// be called concurrently, but data/JSON writers must not interleave.
type ptyStreamWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *ptyStreamWriter) json(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(ptyStreamWriteLimit))
	return w.conn.WriteJSON(v)
}

func (w *ptyStreamWriter) binary(b []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(ptyStreamWriteLimit))
	return w.conn.WriteMessage(websocket.BinaryMessage, b)
}

// runningPtySessions is the single access path to the process owner. It must
// NOT use WorkspaceManagerForBot: for a stopped bot that helper builds a
// throwaway manager with a DIFFERENT SessionRegistry, which would silently
// return an empty list instead of failing loudly.
func (s *Server) runningPtySessions(c *gin.Context) (*sandbox.SessionRegistry, bool) {
	botID := c.Param("id")
	if botID == "" {
		FailMsg(c, http.StatusBadRequest, "bot_id required")
		return nil, false
	}
	if s.botSvc == nil {
		FailMsg(c, http.StatusServiceUnavailable, "bot service unavailable")
		return nil, false
	}
	mgr, ok := s.botSvc.RunningBotWorkspaceMgr(botID)
	if !ok || mgr == nil || mgr.Sessions() == nil {
		FailMsg(c, http.StatusConflict, "bot is not running; interactive sessions are unavailable")
		return nil, false
	}
	return mgr.Sessions(), true
}

// GET /api/bots/:id/pty-sessions
// Read-only and visible to every logged-in user: a member should still see
// that background sessions exist. canManage tells the UI whether the takeover
// / close controls are available, so it never renders buttons that would 403.
func (s *Server) handleListPtySessions(c *gin.Context) {
	reg, ok := s.runningPtySessions(c)
	if !ok {
		return
	}
	OK(c, gin.H{"sessions": reg.List(c.Param("id")), "canManage": ptyCanManage(c)})
}

func ptyCanManage(c *gin.Context) bool {
	user := currentUser(c)
	return user != nil && auth.HasPermission(user.Role, auth.PermBotManage)
}

// POST /api/bots/:id/pty-sessions
// Human-initiated spawn. Same trust level as the existing terminal/exec route:
// admin-only, arbitrary shell inside the bot's sandbox. The agent-side tool is
// unchanged; this exists so a person can start a session and take it over from
// the UI (and so the UI can be verified without driving an LLM turn).
// Cwd is deliberately NOT accepted: the backend default is the bot workspace
// root, which keeps path validation in one place.
func (s *Server) handleSpawnPtySession(c *gin.Context) {
	reg, ok := s.runningPtySessions(c)
	if !ok {
		return
	}
	s.spawnPtySession(c, reg, c.Param("id"))
}

func (s *Server) spawnPtySession(c *gin.Context, reg *sandbox.SessionRegistry, botID string) {
	var req struct {
		Command string `json:"command"`
		Cols    uint16 `json:"cols"`
		Rows    uint16 `json:"rows"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		FailMsg(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		FailMsg(c, http.StatusBadRequest, "command is required")
		return
	}
	v, err := reg.Spawn(c.Request.Context(), botID, "web-console", sandbox.SpawnRequest{
		Command: req.Command, Cols: req.Cols, Rows: req.Rows, Background: true,
	})
	if err != nil {
		Fail(c, errs.BadRequest("spawn failed: "+err.Error()))
		return
	}
	if s.logger != nil {
		s.logger.Infow("pty_session_spawn_request", "event", "pty_session_spawn_request",
			"botID", botID, "sessionId", v.SessionID, "pid", v.PID, "via", "rest")
	}
	OK(c, v)
}

// DELETE /api/bots/:id/pty-sessions/:sid
// Explicit human action from the session list. This is the only REST path that
// terminates a session; reading/list/attaching never do.
func (s *Server) handleClosePtySession(c *gin.Context) {
	reg, ok := s.runningPtySessions(c)
	if !ok {
		return
	}
	botID, sid := c.Param("id"), c.Param("sid")
	if sid == "" {
		FailMsg(c, http.StatusBadRequest, "session id required")
		return
	}
	if err := reg.Close(botID, sid); err != nil {
		status := http.StatusNotFound
		if err == sandbox.ErrSessionWrongBot {
			status = http.StatusForbidden
		}
		FailMsg(c, status, "session not found for this bot")
		return
	}
	if s.logger != nil {
		s.logger.Infow("pty_session_close_request", "event", "pty_session_close_request",
			"botID", botID, "sessionId", sid, "via", "rest")
	}
	OK(c, gin.H{"closed": true})
}

// GET /api/bots/:id/pty-sessions/:sid/stream
func (s *Server) handleBotPtySessionStream(c *gin.Context) {
	reg, ok := s.runningPtySessions(c)
	if !ok {
		return
	}
	s.servePtySessionStream(c, reg, c.Param("id"), c.Param("sid"))
}

// servePtySessionStream takes the registry explicitly so the stream contract
// can be exercised in tests without standing up a running bot instance.
func (s *Server) servePtySessionStream(c *gin.Context, reg *sandbox.SessionRegistry, botID, sid string) {
	if sid == "" {
		FailMsg(c, http.StatusBadRequest, "session id required")
		return
	}
	// Validate existence AND bot ownership before upgrading: a failed upgrade
	// can still return a useful HTTP error instead of a bare socket close.
	if _, err := reg.RawSince(botID, sid, 0); err != nil {
		status := http.StatusNotFound
		if err == sandbox.ErrSessionWrongBot {
			status = http.StatusForbidden
		}
		FailMsg(c, status, "session not found for this bot")
		return
	}

	conn, err := ptySessionUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	conn.SetReadLimit(ptyStreamReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(ptyStreamReadTTL))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(ptyStreamReadTTL))
	})

	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }
	defer stop()

	w := &ptyStreamWriter{conn: conn}
	var closeRequested atomic.Bool
	// A browser ResizeObserver can fire many times per second. Track the last
	// size actually applied so identical frames are dropped before they reach
	// pty.Setsize / the channel daemon (and before they flood the audit log).
	var lastCols, lastRows uint16
	// The writer goroutine owns the session stream; the current goroutine owns
	// client frames. Neither is tied to the agent's message ctx or abort chain.
	go s.streamPtySession(conn, reg, botID, sid, w, done, stop, &closeRequested)

	if s.logger != nil {
		s.logger.Infow("pty_session_ws_open", "event", "pty_session_ws_open",
			"botID", botID, "sessionId", sid)
	}
	defer func() {
		if s.logger != nil {
			s.logger.Infow("pty_session_ws_close", "event", "pty_session_ws_close",
				"botID", botID, "sessionId", sid)
		}
	}()

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			return // tab closed / read deadline: DO NOT close the session
		}
		if mt != websocket.TextMessage {
			continue
		}
		var f ptyClientFrame
		if err := json.Unmarshal(msg, &f); err != nil {
			continue
		}
		switch f.Type {
		case ptyFrameInput:
			data, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil || len(data) == 0 || len(data) > ptyStreamReadLimit {
				continue
			}
			if err := reg.Send(botID, sid, data); err != nil {
				_ = w.json(ptyServerFrame{Type: ptyFrameError, Message: "input rejected: session is not writable"})
				continue
			}
			if s.logger != nil {
				s.logger.Infow("pty_session_ws_input", "event", "pty_session_ws_input",
					"botID", botID, "sessionId", sid, "bytes", len(data))
			}
		case ptyFrameResize:
			if f.Cols == 0 || f.Rows == 0 {
				continue
			}
			cols, rows := sandbox.ClampWinsize(f.Cols, f.Rows)
			if cols == lastCols && rows == lastRows {
				continue
			}
			if err := reg.Resize(botID, sid, cols, rows); err != nil {
				_ = w.json(ptyServerFrame{Type: ptyFrameError, Message: "resize rejected"})
				continue
			}
			lastCols, lastRows = cols, rows
			if s.logger != nil {
				s.logger.Infow("pty_session_ws_resize", "event", "pty_session_ws_resize",
					"botID", botID, "sessionId", sid, "cols", cols, "rows", rows)
			}
		case ptyFrameClose:
			// Explicit human action only. WS disconnect never reaches here.
			// Set the flag BEFORE removing the registry entry so the writer
			// emits a clean state=exited frame instead of racing into an error.
			closeRequested.Store(true)
			if err := reg.Close(botID, sid); err != nil {
				_ = w.json(ptyServerFrame{Type: ptyFrameError, Message: "close rejected"})
				continue
			}
			if s.logger != nil {
				s.logger.Infow("pty_session_ws_close_request", "event", "pty_session_ws_close_request",
					"botID", botID, "sessionId", sid)
			}
		}
	}
}

// streamPtySession replays raw PTY bytes using the Seq watermark. It must NOT
// use SessionRegistry.Poll: that clamps to SettledSeq and would both duplicate
// bytes and consume the agent's semantic cursor (docs §0.1 / §10.1).
func (s *Server) streamPtySession(conn *websocket.Conn, reg *sandbox.SessionRegistry,
	botID, sid string, w *ptyStreamWriter, done chan struct{}, stop func(),
	closeRequested *atomic.Bool) {
	defer stop()
	ticker := time.NewTicker(ptyStreamTick)
	ping := time.NewTicker(ptyStreamPingEvery)
	defer ticker.Stop()
	defer ping.Stop()

	seq := uint64(0) // fresh attach replays everything retained in the ring
	lastState := ""
	// RawSince already redacts each chunk, but a credential registered AFTER the
	// bytes were captured can be split across two 50ms ticks. A per-connection
	// stream masker carries the withheld suffix over frames so neither half is
	// ever emitted. Normal text has zero hold, so terminal latency is unchanged.
	mask := &sandbox.SecretStream{}
	flushMasked := func() {
		if tail := mask.Flush(); len(tail) > 0 {
			_ = w.binary(tail)
		}
	}
	for {
		select {
		case <-done:
			return
		case <-ping.C:
			_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(ptyStreamWriteLimit))
		case <-ticker.C:
			select {
			case <-done:
				return
			default:
			}
			if closeRequested.Load() {
				flushMasked()
				_ = w.json(ptyServerFrame{Type: ptyFrameState,
					State: string(sandbox.SessionExited), Seq: seq})
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session closed"),
					time.Now().Add(time.Second))
				return
			}
			chunk, err := reg.RawSince(botID, sid, seq)
			if err != nil {
				_ = w.json(ptyServerFrame{Type: ptyFrameError, Message: "session no longer available"})
				return
			}
			if chunk.Resync {
				_ = w.json(ptyServerFrame{Type: ptyFrameReset, Seq: chunk.Seq,
					Cols: chunk.Cols, Rows: chunk.Rows})
			}
			if safe := mask.Feed(chunk.Data); len(safe) > 0 {
				if err := w.binary(safe); err != nil {
					return
				}
			}
			seq = chunk.Seq
			state := string(chunk.State)
			if state != lastState {
				if err := w.json(ptyServerFrame{Type: ptyFrameState, State: state,
					Seq: chunk.Seq, Cols: chunk.Cols, Rows: chunk.Rows}); err != nil {
					return
				}
				lastState = state
			}
			if state != string(sandbox.SessionRunning) {
				// Final bytes are already delivered. Flush any withheld
				// credential prefix as a marker, then close the socket so the
				// reader goroutine unblocks; the session itself is untouched.
				flushMasked()
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"),
					time.Now().Add(time.Second))
				return
			}
		}
	}
}
