package sandbox

// ============================================================================
// SessionRegistry — the single owner of every background session process
//
// Tools never call exec.Command / Kill / Wait themselves. botID is always taken
// from the ToolProvider closure, never from model input. watch is read-only:
// it does not consume deltas, advance cursors, or touch lastTouch (docs §3.7).
//
// Reclamation has two layers:
//   - process-level idle TTL lives in the session's own readLoop (session_local.go),
//     so it fires even when no agent ever calls watch/spawn again;
//   - lazy GC here only drops finished registry entries, never kills processes.
// ============================================================================

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

type sessionEntry struct {
	handle    SessionHandle
	botID     string
	createdAt time.Time
	lastTouch time.Time
	orphaned  bool
	pgid      int
}

type SessionRegistry struct {
	mu        sync.RWMutex
	items     map[string]*sessionEntry
	cursors   map[string]map[string]uint64 // sessionId -> agent -> settledSeq
	cfg       SessionConfig
	logger    *zap.SugaredLogger
	spawner   SessionSpawner
	reclaimed int

	// changeCh is a broadcast wake-up for long-poll watchers. signalChange
	// closes the current channel and swaps in a fresh one, so every subscriber
	// is released (a buffered non-blocking send would wake only one waiter).
	changeMu sync.Mutex
	changeCh chan struct{}
}

func NewSessionRegistry(cfg SessionConfig, logger *zap.SugaredLogger, spawner SessionSpawner) *SessionRegistry {
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}
	return &SessionRegistry{
		items:    map[string]*sessionEntry{},
		cursors:  map[string]map[string]uint64{},
		cfg:      cfg,
		logger:   logger.With("component", "session_registry"),
		spawner:  spawner,
		changeCh: make(chan struct{}),
	}
}

// changeHookTarget is an optional SessionHandle capability. Both backends and
// the test core embed *sessionCore, so they all satisfy it without adding a
// method to the SessionHandle interface.
type changeHookTarget interface {
	SetChangeHook(func())
}

// subscribe returns the channel that closes on the next registry change.
func (r *SessionRegistry) subscribe() <-chan struct{} {
	r.changeMu.Lock()
	defer r.changeMu.Unlock()
	return r.changeCh
}

func (r *SessionRegistry) signalChange() {
	r.changeMu.Lock()
	ch := r.changeCh
	r.changeCh = make(chan struct{})
	r.changeMu.Unlock()
	close(ch)
}

// SetSpawner lets the channel backend replace the local PTY spawner after the
// workspace manager has been constructed.
func (r *SessionRegistry) SetSpawner(s SessionSpawner) {
	r.mu.Lock()
	r.spawner = s
	r.mu.Unlock()
}

func (r *SessionRegistry) Spawn(ctx context.Context, botID, agent string, req SpawnRequest) (SessionView, error) {
	if botID == "" {
		return SessionView{}, fmt.Errorf("%w: missing botID", ErrSessionBadInput)
	}
	if agent == "" {
		agent = "default"
	}
	r.GC()
	r.mu.Lock()
	if r.spawner == nil {
		r.mu.Unlock()
		return SessionView{}, ErrSessionUnsupported
	}
	perBot, total := 0, len(r.items)
	for _, e := range r.items {
		if e.botID == botID {
			perBot++
		}
	}
	if perBot >= r.cfg.MaxPerBot || total >= r.cfg.MaxTotal {
		r.mu.Unlock()
		return SessionView{}, ErrSessionLimit
	}
	spawner := r.spawner
	r.mu.Unlock()

	// Spawn-time winsize is as untrusted as a resize frame (it comes from the
	// web console POST body). Clamp before the backend calls pty.Setsize.
	req.Cols, req.Rows = ClampWinsize(req.Cols, req.Rows)
	h, err := spawner.Spawn(ctx, botID, req)
	if err != nil {
		return SessionView{}, err
	}
	h.SetOwner(botID)
	if hook, ok := h.(changeHookTarget); ok {
		hook.SetChangeHook(r.signalChange)
	}
	view := h.View()

	r.mu.Lock()
	r.items[view.SessionID] = &sessionEntry{
		handle: h, botID: botID, createdAt: time.Now(),
		lastTouch: time.Now(), pgid: view.PGID,
	}
	r.mu.Unlock()

	r.logger.Infow("interactive_session",
		"event", "session_registered", "sessionId", view.SessionID,
		"botID", botID, "pid", view.PID, "pgid", view.PGID,
		"backend", view.Backend, "perBot", perBot+1, "total", total+1)
	r.signalChange()
	return view, nil
}

func (r *SessionRegistry) get(botID, sessionID string) (*sessionEntry, error) {
	r.mu.RLock()
	e, ok := r.items[sessionID]
	r.mu.RUnlock()
	if !ok {
		return nil, r.notFoundError(botID, sessionID)
	}
	if e.botID != botID {
		return nil, ErrSessionWrongBot
	}
	return e, nil
}

// notFoundError makes a stale / invented sessionId self-healing.
//
// Sessions outlive a chat turn, and the model repeatedly tried to recall one
// across turns with a mnemonic label ("cursor-agent", "codebuddy-merge") that
// was never a real handle. A bare "session not found" gives it nothing to
// recover with, so it burns a whole step before falling back to session_watch.
// The wrapped sentinel keeps errors.Is(err, ErrSessionNotFound) working.
func (r *SessionRegistry) notFoundError(botID, sessionID string) error {
	live := r.List(botID)
	if len(live) == 0 {
		return fmt.Errorf("%w: %q; no live sessions for this bot — call session_watch to list them, or session_spawn to start one",
			ErrSessionNotFound, sessionID)
	}
	const maxHint = 8
	parts := make([]string, 0, maxHint)
	for _, v := range live {
		if len(parts) == maxHint {
			parts = append(parts, "...")
			break
		}
		parts = append(parts, fmt.Sprintf("%s(state=%s)", v.SessionID, v.State))
	}
	return fmt.Errorf("%w: %q is not a session id (never invent a name); live sessions: %s — copy one of these exact ids, or call session_watch",
		ErrSessionNotFound, sessionID, strings.Join(parts, ", "))
}

func (r *SessionRegistry) Poll(botID, agent, sessionID string, sinceSeq uint64, mode string, lines int, wait time.Duration) (SessionView, error) {
	e, err := r.get(botID, sessionID)
	if err != nil {
		return SessionView{}, err
	}
	if agent == "" {
		agent = "default"
	}
	// sinceSeq is supplied by the agent as a hint, but the authoritative
	// per-agent cursor lives here so a model cannot reset it by passing 0.
	r.mu.RLock()
	cur := r.cursors[sessionID][agent]
	r.mu.RUnlock()
	if sinceSeq == 0 && cur > 0 {
		sinceSeq = cur
	}
	view, err := e.handle.Poll(sinceSeq, mode, lines, wait)
	if err != nil {
		return SessionView{}, err
	}
	r.mu.Lock()
	if r.cursors[sessionID] == nil {
		r.cursors[sessionID] = map[string]uint64{}
	}
	if view.SettledSeq > r.cursors[sessionID][agent] {
		r.cursors[sessionID][agent] = view.SettledSeq
	}
	e.lastTouch = time.Now()
	r.mu.Unlock()
	return view, nil
}

// RawSince streams raw PTY bytes to a human web console. It deliberately does
// NOT go through Poll: the agent's semantic cursor and SettledSeq must stay
// untouched so opening a browser tab can never consume or replay the agent's
// deltas (docs §10 P2).
func (r *SessionRegistry) RawSince(botID, sessionID string, seq uint64) (RawChunk, error) {
	e, err := r.get(botID, sessionID)
	if err != nil {
		return RawChunk{}, err
	}
	c := e.handle.RawSince(seq)
	r.mu.Lock()
	e.lastTouch = time.Now()
	r.mu.Unlock()
	return c, nil
}

// View is a pure read-only snapshot used by control paths that must not look
// like agent activity: background=auto observation and web polling. Unlike
// Poll it does NOT Touch the session, settle deltas or advance any cursor, so
// an auto-upgrade probe cannot make an idle session look active nor consume
// output the agent has not seen yet.
func (r *SessionRegistry) View(botID, sessionID string) (SessionView, error) {
	e, err := r.get(botID, sessionID)
	if err != nil {
		return SessionView{}, err
	}
	return e.handle.View(), nil
}

func (r *SessionRegistry) Send(botID, sessionID string, data []byte) error {
	e, err := r.get(botID, sessionID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	e.lastTouch = time.Now()
	r.mu.Unlock()
	return e.handle.Send(data)
}

func (r *SessionRegistry) Signal(botID, sessionID, sig string) error {
	e, err := r.get(botID, sessionID)
	if err != nil {
		return err
	}
	return e.handle.Signal(sig)
}

func (r *SessionRegistry) Resize(botID, sessionID string, cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return fmt.Errorf("%w: zero winsize", ErrSessionBadInput)
	}
	cols, rows = ClampWinsize(cols, rows)
	e, err := r.get(botID, sessionID)
	if err != nil {
		return err
	}
	return e.handle.Resize(cols, rows)
}

func (r *SessionRegistry) Close(botID, sessionID string) error {
	r.mu.Lock()
	e, ok := r.items[sessionID]
	if ok && e.botID != botID {
		r.mu.Unlock()
		return ErrSessionWrongBot
	}
	if ok {
		delete(r.items, sessionID)
		delete(r.cursors, sessionID)
	}
	r.mu.Unlock()
	if !ok {
		return r.notFoundError(botID, sessionID)
	}
	escaped := []int{}
	err := e.handle.Close()
	if e.pgid > 0 && pgidAlive(e.pgid) {
		escaped = append(escaped, e.pgid)
	}
	r.logger.Infow("interactive_session",
		"event", "session_closed", "sessionId", sessionID, "botID", botID,
		"pgid", e.pgid, "escapedPids", escaped,
		"note", "registry empty does not imply no surviving processes")
	r.signalChange()
	return err
}

type watchPriority struct {
	priority  int
	reason    string
	suggested string
}

var watchPriorities = map[string]watchPriority{
	HealthAwaitingAction:           {0, "screen shows an actionable prompt", "poll then send"},
	HealthAwaitingSecret:           {0, "credential prompt visible", "poll then send the {{secret:name}} placeholder"},
	HealthConnectFailed:            {0, "authentication or connection failed", "report failure, do not retry blindly"},
	HealthSuspectedStuck:           {0, "no output and no actionable UI", "bootDiag"},
	HealthSuspectedStuckNoProgress: {0, "output but no semantic progress", "bootDiag"},
	HealthUnhealthyDaemon:          {0, "readLoop heartbeat stopped", "restart session"},
	HealthExited:                   {1, "collect final output", "poll then close"},
	HealthIdleAwaitingAgent:        {1, "input prompt visible", "send"},
	HealthProgressing:              {2, "working", "none"},
	HealthSpinning:                 {2, "repaint only", "none"},
	HealthIdle:                     {2, "recently active", "none"},
}

// Watch is a read-only attention queue. It deliberately does not Touch the
// sessions: lastTouch must reflect real agent interaction for the idle TTL.
// List is a pure read-only snapshot for the web session panel. It must NOT
// reuse Watch: Watch runs GC and ranks by agent attention, so a session that
// just exited could disappear before a human ever opened its replay, and quiet
// sessions would be hidden. Delta/Screen are cleared to keep the payload small;
// the WS endpoint replays those from the raw buffer instead.
func (r *SessionRegistry) List(botID string) []SessionView {
	r.mu.RLock()
	ids := make([]string, 0, len(r.items))
	for id, e := range r.items {
		if botID == "" || e.botID == botID {
			ids = append(ids, id)
		}
	}
	r.mu.RUnlock()
	sort.Strings(ids)

	out := make([]SessionView, 0, len(ids))
	for _, id := range ids {
		r.mu.RLock()
		e := r.items[id]
		orphaned := e != nil && e.orphaned
		r.mu.RUnlock()
		if e == nil {
			continue
		}
		v := e.handle.View()
		v.Delta, v.Screen, v.URLs = "", "", nil
		v.Orphaned = orphaned
		out = append(out, v)
	}
	return out
}

func (r *SessionRegistry) Watch(botID, agent string, limit int, includeQuiet bool) WatchResult {
	return r.watchSnapshot(botID, agent, limit, includeQuiet, r.GC())
}

// watchSnapshot builds the read-only attention queue without running GC.
// Long polling calls this on every wake-up, so it must have no side effects
// at all: no Touch, no delta consumption, no cursor advance, no reclamation.
func (r *SessionRegistry) watchSnapshot(botID, agent string, limit int, includeQuiet bool, reclaimed int) WatchResult {
	if agent == "" {
		agent = "default"
	}
	r.mu.RLock()
	type row struct {
		id  string
		e   *sessionEntry
		cur uint64
	}
	rows := make([]row, 0, len(r.items))
	for id, e := range r.items {
		if e.botID != botID {
			continue
		}
		cur := r.cursors[id][agent]
		rows = append(rows, row{id: id, e: e, cur: cur})
	}
	r.mu.RUnlock()

	items := make([]WatchItem, 0, len(rows))
	quiet := 0
	for _, rr := range rows {
		v := rr.e.handle.View()
		p, ok := watchPriorities[v.Health.Verdict]
		if !ok {
			p = watchPriority{3, "unknown health verdict", "poll"}
		}
		item := WatchItem{
			SessionID: rr.id,
			PID:       v.PID,
			PGID:      v.PGID,
			State:     string(v.State),
			Verdict:   v.Health.Verdict,
			Priority:  p.priority,
			Reason:    p.reason,
			Suggested: p.suggested,
			Seq:       v.Seq,
			SinceSeq:  rr.cur,
			Orphaned:  rr.e.orphaned,
			URLs:      v.URLs,
			IdleMs:    time.Since(rr.e.lastTouch).Milliseconds(),
		}
		if item.Orphaned {
			item.Priority = 1
			if item.Reason == "working" {
				item.Reason = "agent turn ended; session still running"
			}
		}
		if p.priority >= 2 && !item.Orphaned {
			quiet++
			if !includeQuiet {
				continue
			}
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		return items[i].Seq > items[j].Seq
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return WatchResult{
		BotID: botID, Total: len(rows), Attention: items,
		Quiet: quiet, Reclaimed: reclaimed,
		Note: "watch is read-only; act only on attention items via poll/send",
	}
}

// MarkOrphaned is called from the abort path. It does NOT kill the session:
// background sessions must outlive the message context (docs §3.7).
func (r *SessionRegistry) MarkOrphaned(botID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id, e := range r.items {
		if e.botID != botID || e.orphaned {
			continue
		}
		e.orphaned = true
		n++
		r.logger.Infow("interactive_session",
			"event", "session_orphaned", "sessionId", id, "botID", botID,
			"pid", e.pgid, "reason", "agent turn aborted; idle TTL will reclaim")
	}
	if n > 0 {
		r.signalChange()
	}
	return n
}

// CloseAll is the deterministic lifecycle hook. It must be wired into
// DestroyBot / StopBot / CloseAll / BotService.StopBot / Bot.Close.
func (r *SessionRegistry) CloseAll(botID string) int {
	r.mu.Lock()
	targets := make([]*sessionEntry, 0)
	ids := make([]string, 0)
	for id, e := range r.items {
		if botID == "" || e.botID == botID {
			targets = append(targets, e)
			ids = append(ids, id)
			delete(r.items, id)
			delete(r.cursors, id)
		}
	}
	r.mu.Unlock()

	escaped := []int{}
	for _, e := range targets {
		_ = e.handle.Close()
		if e.pgid > 0 && pgidAlive(e.pgid) {
			escaped = append(escaped, e.pgid)
		}
	}
	r.logger.Infow("interactive_session",
		"event", "session_close_all", "botID", botID, "closed", len(targets),
		"escapedPids", escaped,
		"note", "process group kill is not a guarantee; escaped children are not owned by any sessionId")
	if len(targets) > 0 {
		r.signalChange()
	}
	return len(targets)
}

// GC only removes finished registry entries. It never kills a process — that
// is the readLoop's job, so an agent that never polls again still gets reaped.
func (r *SessionRegistry) GC() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	now := time.Now()
	for id, e := range r.items {
		v := e.handle.View()
		if v.State == SessionRunning {
			continue
		}
		if now.Sub(e.lastTouch) < r.cfg.RetainAfterExit {
			continue
		}
		delete(r.items, id)
		delete(r.cursors, id)
		n++
	}
	r.reclaimed += n
	return n
}

func (r *SessionRegistry) Count(botID string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.items {
		if botID == "" || e.botID == botID {
			n++
		}
	}
	return n
}
