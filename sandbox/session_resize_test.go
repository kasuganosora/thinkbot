package sandbox

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

// Regression for the browser resize feedback loop found during real-machine
// acceptance: a ResizeObserver + xterm fit() loop emitted 4434 frames with rows
// climbing past 3000. The backend must never forward an unbounded winsize to
// pty.Setsize even if a client misbehaves.
type resizeSpyHandle struct {
	*coreTestHandle
	gotCols uint16
	gotRows uint16
	calls   int
}

func (h *resizeSpyHandle) Resize(cols, rows uint16) error {
	h.gotCols, h.gotRows, h.calls = cols, rows, h.calls+1
	return h.coreTestHandle.Resize(cols, rows)
}

type resizeSpySpawner struct {
	h   SessionHandle
	req SpawnRequest
}

func (s *resizeSpySpawner) Spawn(_ context.Context, _ string, req SpawnRequest) (SessionHandle, error) {
	s.req = req
	return s.h, nil
}

func TestClampWinsize(t *testing.T) {
	cases := []struct {
		name       string
		cols, rows uint16
		wantC      uint16
		wantR      uint16
	}{
		{"zero means backend default", 0, 0, 0, 0},
		{"normal", 120, 40, 120, 40},
		{"exact cap", MaxSessionCols, MaxSessionRows, MaxSessionCols, MaxSessionRows},
		{"runaway browser", 118, 3286, 118, MaxSessionRows},
		{"both oversized", 4000, 9000, MaxSessionCols, MaxSessionRows},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, r := ClampWinsize(tc.cols, tc.rows)
			if c != tc.wantC || r != tc.wantR {
				t.Fatalf("ClampWinsize(%d,%d) = (%d,%d), want (%d,%d)",
					tc.cols, tc.rows, c, r, tc.wantC, tc.wantR)
			}
		})
	}
}

func TestSessionRegistry_SpawnClampsWinsize(t *testing.T) {
	h := &resizeSpyHandle{coreTestHandle: &coreTestHandle{newTestCore(80, 24)}}
	sp := &resizeSpySpawner{h: h}
	reg := NewSessionRegistry(DefaultSessionConfig(), zap.NewNop().Sugar(), sp)

	if _, err := reg.Spawn(context.Background(), "bot", sessionAgent,
		SpawnRequest{Command: "cmd", Cols: 9000, Rows: 7000}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if sp.req.Cols != MaxSessionCols || sp.req.Rows != MaxSessionRows {
		t.Fatalf("spawner saw (%d,%d), want clamped (%d,%d)",
			sp.req.Cols, sp.req.Rows, MaxSessionCols, MaxSessionRows)
	}
}

func TestSessionRegistry_ResizeClampsAndRejectsZero(t *testing.T) {
	h := &resizeSpyHandle{coreTestHandle: &coreTestHandle{newTestCore(80, 24)}}
	reg := NewSessionRegistry(DefaultSessionConfig(), zap.NewNop().Sugar(), &resizeSpySpawner{h: h})
	if _, err := reg.Spawn(context.Background(), "bot", sessionAgent, SpawnRequest{Command: "cmd"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	if err := reg.Resize("bot", h.ID(), 0, 0); !errors.Is(err, ErrSessionBadInput) {
		t.Fatalf("Resize(0,0) err = %v, want ErrSessionBadInput", err)
	}
	if err := reg.Resize("bot", h.ID(), 118, 3286); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if h.gotCols != 118 || h.gotRows != MaxSessionRows {
		t.Fatalf("handle saw (%d,%d), want (118,%d)", h.gotCols, h.gotRows, MaxSessionRows)
	}
	if h.calls != 1 {
		t.Fatalf("handle Resize called %d times, want 1", h.calls)
	}
}

func TestSessionCore_ResizeScreenNeverExceedsCaps(t *testing.T) {
	c := newTestCore(80, 24)
	c.resizeScreen(9000, 7000)
	c.bufMu.Lock()
	cols, rows := c.cols, c.rows
	c.bufMu.Unlock()
	if cols > int(MaxSessionCols) || rows > int(MaxSessionRows) {
		t.Fatalf("grid = %dx%d, must stay within %dx%d",
			cols, rows, MaxSessionCols, MaxSessionRows)
	}
	// The real PTY is clamped by the registry to the same values, so grid and
	// winsize cannot silently diverge.
	cc, rr := ClampWinsize(9000, 7000)
	if cols != int(cc) || rows != int(rr) {
		t.Fatalf("grid %dx%d != clamped winsize %dx%d", cols, rows, cc, rr)
	}
}
