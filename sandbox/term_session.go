package sandbox

import (
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/util/idgen"
)

// TerminalIdle is how long an interactive terminal stays attachable with no use.
// Opening the panel does not create a session; the first command does.
const TerminalIdle = 900 * time.Second

// TermSession is a reattachable command session. It remembers the working
// directory between calls. It is not a PTY: this repo has no terminal
// emulator dependency, and a graphical desktop stream is not started here.
type TermSession struct {
	ID    string
	BotID string
	Cwd   string
	Last  time.Time
}

// TermHub keeps terminal sessions so a refresh can resume one that is still
// inside the idle window.
type TermHub struct {
	mu    sync.Mutex
	idle  time.Duration
	now   func() time.Time
	items map[string]*TermSession
}

func NewTermHub(idle time.Duration) *TermHub {
	if idle <= 0 {
		idle = TerminalIdle
	}
	return &TermHub{idle: idle, now: time.Now, items: map[string]*TermSession{}}
}

// Attach returns an existing live session or creates one. expired is true when
// id was known but the idle window had passed, in which case a new session is
// returned.
func (h *TermHub) Attach(botID, id, cwd string) (s *TermSession, expired bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropExpiredLocked()
	if id != "" {
		if cur, ok := h.items[id]; ok && cur.BotID == botID {
			cur.Last = h.now()
			if cwd != "" {
				cur.Cwd = cwd
			}
			return cur, false
		}
		expired = true
	}
	s = &TermSession{ID: idgen.New("term"), BotID: botID, Cwd: cwd, Last: h.now()}
	h.items[s.ID] = s
	return s, expired
}

// Touch records activity and an updated cwd.
// Peek returns a live session without creating one.
func (h *TermHub) Peek(botID, id string) (*TermSession, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropExpiredLocked()
	cur, ok := h.items[id]
	if !ok || cur.BotID != botID {
		return nil, false
	}
	return cur, true
}

func (h *TermHub) Touch(id, cwd string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.items[id]; ok {
		cur.Last = h.now()
		if cwd != "" {
			cur.Cwd = cwd
		}
	}
}

func (h *TermHub) dropExpiredLocked() {
	now := h.now()
	for id, s := range h.items {
		if now.Sub(s.Last) > h.idle {
			delete(h.items, id)
		}
	}
}

// DesktopStatus describes the graphical surface ThinkBot can actually offer.
// The sandbox browser already runs on Xvfb. A full desktop the operator can
// click in the browser would need a VNC websocket proxy, which is not a
// dependency of this repo, so Available stays false.
type DesktopStatus struct {
	Available bool   `json:"available"`
	Surface   string `json:"surface,omitempty"`
	Reason    string `json:"reason"`
}

func DescribeDesktop() DesktopStatus {
	return DesktopStatus{
		Available: false,
		Surface:   "browser-xvfb",
		Reason:    "graphical desktop needs a VNC websocket proxy; the sandbox browser on Xvfb is the graphical surface that already exists",
	}
}
