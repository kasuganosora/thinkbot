package sandbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/llm"
	"go.uber.org/zap"
)

// Real-shaped ANSI flood: alt-screen, SGR, a partially repainted spinner,
// Unicode box drawing, a device-code URL and an actionable confirmation.
// The point is that the input IS transformed — using plain ASCII here would
// make the idempotency test degenerate into a no-op (docs §5).
var goldenANSIFlood = []byte("\x1b[?1049h\x1b[2J\x1b[H" +
	"┌──────────────────────┐\r\n" +
	"│ step 1/3             │\r\n" +
	"\x1b[38;5;79mworking...\x1b[0m\r\n" +
	"\x1b[2K\x1b[1G⠋ Waking… (0s · preparing…)\r\n" +
	"https://example.com/device?code=ABCD-1234\r\n" +
	"Do you want to proceed? [y/n]\r\n" +
	"\x1b[?25l> \x1b[?25h\x1b[?1049l")

func feedInChunks(raw []byte, size int) []string {
	d := newAnsiDecoder()
	s := newVTScreen(120, 40)
	for i := 0; i < len(raw); i += size {
		end := i + size
		if end > len(raw) {
			end = len(raw)
		}
		s.FeedEvents(d.Feed(raw[i:end]))
	}
	s.FeedEvents(d.Flush())
	return s.Lines()
}

func TestRenderScreen_IdempotentAndChunkBoundarySafe(t *testing.T) {
	want := RenderScreen(goldenANSIFlood, 120, 40).Lines()

	// Same input twice yields the same screen.
	if got := RenderScreen(goldenANSIFlood, 120, 40).Lines(); !reflect.DeepEqual(got, want) {
		t.Fatalf("render is not deterministic:\n got %q\nwant %q", got, want)
	}
	// Arbitrary chunk boundaries (including 1 byte, which splits every UTF-8
	// rune and every CSI sequence) must reproduce the same screen.
	for _, size := range []int{1, 2, 7, 13, 64, 4096} {
		if got := feedInChunks(goldenANSIFlood, size); !reflect.DeepEqual(got, want) {
			t.Errorf("chunk size %d produced a different screen:\n got %q\nwant %q", size, got, want)
		}
	}
	if len(want) == 0 {
		t.Fatal("golden input rendered an empty screen; the test degenerated")
	}
}

func TestRingBuffer_SnapshotAndSince(t *testing.T) {
	r := newRingBuffer(16)
	_, _ = r.Write([]byte("hello "))
	_, _ = r.Write([]byte("world"))
	if got := string(r.Snapshot()); got != "hello world" {
		t.Fatalf("snapshot = %q", got)
	}
	if got, _ := r.Since(6); string(got) != "world" {
		t.Errorf("Since(6) = %q", got)
	}
	_, _ = r.Write([]byte(" 1234567890")) // overflow the ring
	if !r.Truncated() {
		t.Error("expected truncated after overwrite")
	}
	if _, lost := r.Since(0); !lost {
		t.Error("Since(0) after overwrite must report a gap")
	}
	// A gap must be per-cursor, not sticky: otherwise the web console would
	// reset xterm on every frame forever after the first ring overflow.
	if _, lost := r.Since(r.Seq()); lost {
		t.Error("Since(current seq) must not report a gap after an old overflow")
	}
	if _, lost := r.Since(r.Seq() + 100); lost {
		t.Error("a future cursor is clamped, not a gap")
	}
}

func TestExtractURLs_UsesFullRaw(t *testing.T) {
	raw := []byte("go to https://example.com/a?x=1\x1b[0m\r\n" +
		"and https://example.com/b, then https://example.com/a?x=1 again")
	got := extractURLs(raw, 20)
	if len(got) != 2 {
		t.Fatalf("expected 2 unique URLs, got %v", got)
	}
	if got[0] != "https://example.com/a?x=1" || got[1] != "https://example.com/b" {
		t.Fatalf("unexpected URLs: %v", got)
	}
}

func TestDeltaLog_DoubleWatermarkNeverLosesContent(t *testing.T) {
	cfg := DefaultSessionConfig()
	cfg.StableWindow = 10 * time.Millisecond
	d := newDeltaLog(cfg)
	t0 := time.Now()

	d.OnBytes(10, map[int]int{0: 1}, t0)
	if d.MaybeSettle([]string{"line one"}, t0) {
		t.Fatal("settled before the stable window elapsed")
	}
	if s, seq := d.Delta(0); s != "" || seq != 0 {
		t.Fatalf("unsettled delta must be empty, got %q / seq=%d", s, seq)
	}

	if !d.MaybeSettle([]string{"line one"}, t0.Add(20*time.Millisecond)) {
		t.Fatal("expected a settlement after quiet period")
	}
	first, settled := d.Delta(0)
	if first != "line one" || settled != 10 {
		t.Fatalf("first delta = %q, settledSeq=%d", first, settled)
	}
	// Repeating the same cursor is idempotent (watch zero-side-effect depends
	// on this).
	if again, _ := d.Delta(0); again != first {
		t.Errorf("Delta(0) is not idempotent: %q vs %q", again, first)
	}

	// New bytes arrive inside a fresh window; an agent that (wrongly) uses the
	// raw seq as cursor must not lose this content. The poll layer clamps
	// sinceSeq down to SettledSeq, and Delta(10) then returns the new line.
	d.OnBytes(20, map[int]int{1: 1}, t0.Add(30*time.Millisecond))
	if _, seq := d.Delta(10); seq != 10 {
		t.Errorf("settledSeq advanced before settlement: %d", seq)
	}
	d.MaybeSettle([]string{"line one", "line two"}, t0.Add(60*time.Millisecond))
	got, newSeq := d.Delta(10)
	if got != "line two" || newSeq != 20 {
		t.Fatalf("second delta lost content: %q / settledSeq=%d", got, newSeq)
	}
}

func TestDeltaLog_CollapsesHighFrequencyRepaint(t *testing.T) {
	cfg := DefaultSessionConfig()
	cfg.StableWindow = time.Millisecond
	cfg.CollapseThreshold = 3
	d := newDeltaLog(cfg)
	now := time.Now()
	// One row rewritten 9 times inside a single window: only the final state
	// should be emitted, tagged collapsed:N.
	d.OnBytes(90, map[int]int{0: 9}, now)
	d.MaybeSettle([]string{"final spinner state"}, now.Add(10*time.Millisecond))
	got, _ := d.Delta(0)
	if !strings.Contains(got, "collapsed:9") || !strings.Contains(got, "final spinner state") {
		t.Fatalf("expected a collapsed final row, got %q", got)
	}
}

func TestHealthVerdict_Ordering(t *testing.T) {
	cfg := DefaultSessionConfig()
	now := time.Now()

	t.Run("actionable beats progressing", func(t *testing.T) {
		h := newHealthTracker(cfg)
		h.Feed(5000, 30, []string{"> Do you want to proceed? [y/n]"}, now)
		if v := h.Verdict(SessionRunning, true, []string{"Do you want to proceed? [y/n]"}, now); v.Verdict != HealthAwaitingAction {
			t.Fatalf("got %q, want %q", v.Verdict, HealthAwaitingAction)
		}
	})

	t.Run("changing lines beat screenStableMs", func(t *testing.T) {
		h := newHealthTracker(cfg)
		h.Feed(5000, 30, []string{"step 1", "step 2"}, now)
		h.mu.Lock()
		h.semSince = now.Add(-10 * time.Second) // force a huge stable duration
		h.mu.Unlock()
		if v := h.Verdict(SessionRunning, true, []string{"step N"}, now); v.Verdict != HealthProgressing {
			t.Fatalf("got %q, want %q", v.Verdict, HealthProgressing)
		}
	})

	t.Run("waiting does not veto inputReady", func(t *testing.T) {
		h := newHealthTracker(cfg)
		h.Feed(1, 0, []string{"✷ Waking… (0s · preparing…)", "> "}, now)
		if v := h.Verdict(SessionRunning, true, []string{"Waking… (0s · preparing…)", "> "}, now); v.Verdict != HealthIdleAwaitingAgent {
			t.Fatalf("got %q, want %q", v.Verdict, HealthIdleAwaitingAgent)
		}
	})

	t.Run("high rate without progress is spinning", func(t *testing.T) {
		h := newHealthTracker(cfg)
		h.Feed(5000, 1, []string{"working..."}, now)
		if v := h.Verdict(SessionRunning, true, []string{"working..."}, now); v.Verdict != HealthSpinning {
			t.Fatalf("got %q, want %q", v.Verdict, HealthSpinning)
		}
	})

	t.Run("dead readLoop is daemon fault", func(t *testing.T) {
		h := newHealthTracker(cfg)
		h.mu.Lock()
		h.beat = now.Add(-time.Hour)
		h.mu.Unlock()
		if v := h.Verdict(SessionRunning, true, []string{"anything"}, now); v.Verdict != HealthUnhealthyDaemon {
			t.Fatalf("got %q, want %q", v.Verdict, HealthUnhealthyDaemon)
		}
	})
}

func TestBuildSessionEnv_AllowlistAndRedaction(t *testing.T) {
	env, dropped := buildSessionEnv([]string{
		"FOO=bar",
		"ACC_PRODUCT_CONFIG_PATH=/some/host/path",
		"WORKBUDDY_ENDPOINT=https://internal.example.com",
		"MY_API_KEY=super-secret",
	}, "xterm-256color", 120, 40)
	joined := strings.Join(env, "\n")
	for _, want := range []string{"FOO=bar", "TERM=xterm-256color", "COLUMNS=120", "LINES=40"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in env", want)
		}
	}
	for _, banned := range []string{"ACC_PRODUCT_CONFIG_PATH", "WORKBUDDY_ENDPOINT", "MY_API_KEY", "super-secret", "internal.example.com"} {
		if strings.Contains(joined, banned) {
			t.Errorf("sensitive host variable leaked into session env: %q", banned)
		}
	}
	if dropped < 0 {
		t.Error("dropped count must never be negative")
	}
}

func TestBuildContainerSessionEnv_NoHostLeak(t *testing.T) {
	t.Setenv("PATH", "/host-only/bin")
	t.Setenv("HOME", "/opt/host-only-home")
	t.Setenv("SHELL", "/bin/host-only-shell")
	env, _ := buildContainerSessionEnv([]string{
		"FOO=bar",
		"ACC_PRODUCT_CONFIG_PATH=/some/host/path",
		"MY_API_KEY=super-secret",
	}, "xterm-256color", 120, 40)
	joined := strings.Join(env, "\n")
	for _, want := range []string{"FOO=bar", "TERM=xterm-256color", "COLUMNS=120", "LINES=40"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in container env: %s", want, joined)
		}
	}
	for _, banned := range []string{"/host-only", "HOME=", "SHELL=", "ACC_PRODUCT_CONFIG_PATH", "MY_API_KEY", "super-secret"} {
		if strings.Contains(joined, banned) {
			t.Errorf("host value leaked into container env: %q (%s)", banned, joined)
		}
	}
}

func TestFillSessionDefaults_PartialOverride(t *testing.T) {
	c := fillSessionDefaults(SessionConfig{IdleTTL: time.Second})
	if c.IdleTTL != time.Second {
		t.Errorf("explicit override was lost: %v", c.IdleTTL)
	}
	if c.MaxPerBot != 4 || c.MaxTotal != 32 || c.StableWindow != 120*time.Millisecond {
		t.Errorf("defaults not applied: %+v", c)
	}
	if DefaultConfig().Session.IdleTTL != 15*time.Minute {
		t.Error("DefaultConfig must carry the session defaults")
	}
}

func TestSessionToolNames_AreRegisteredAndNotShadowed(t *testing.T) {
	defs := botWorkspaceToolDefs(nil, "test-bot")
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for _, n := range sessionToolNames {
		if !names[n] {
			t.Errorf("session tool %q is not registered", n)
		}
	}
	// subagent/tools.go statically owns "spawn"; a dynamic tool with that name
	// would be silently dropped by ToolRegistry.Resolve.
	if names["spawn"] {
		t.Error("bare 'spawn' must not be registered by this package")
	}
	if len(defs) != len(WorkspaceToolNames()) {
		t.Error("WorkspaceToolNames() is out of sync with botWorkspaceToolDefs()")
	}
}

func TestSessionTools_HiddenFromSubagent(t *testing.T) {
	full := botWorkspaceToolDefsFor(nil, "b", true)
	sub := botWorkspaceToolDefsFor(nil, "b", false)
	if len(full)-len(sub) != len(sessionToolNames) {
		t.Fatalf("SubAgent filtering removed %d tools, expected %d", len(full)-len(sub), len(sessionToolNames))
	}
	for _, d := range sub {
		if strings.HasPrefix(d.Name, "sandbox_session_") {
			t.Errorf("SubAgent still sees %q", d.Name)
		}
	}
}

// ----------------------------------------------------------------------------
// Web-console streaming contract (docs §10 P2)
// ----------------------------------------------------------------------------

// coreTestHandle is the smallest SessionHandle that still owns a real
// sessionCore, so the tests exercise the actual buffer / screen / watermark
// code instead of a mock that just returns canned answers.
type coreTestHandle struct{ *sessionCore }

func (h *coreTestHandle) Send([]byte) error        { return nil }
func (h *coreTestHandle) Signal(string) error      { return nil }
func (h *coreTestHandle) Close() error             { return nil }
func (h *coreTestHandle) Resize(c, r uint16) error { h.resizeScreen(c, r); return nil }

type coreTestSpawner struct{ h SessionHandle }

func (s *coreTestSpawner) Spawn(_ context.Context, _ string, _ SpawnRequest) (SessionHandle, error) {
	return s.h, nil
}

var _ SessionHandle = (*coreTestHandle)(nil)

func newTestCore(cols, rows int) *sessionCore {
	cfg := DefaultSessionConfig()
	return newSessionCore(cfg, zap.NewNop().Sugar(), "sess-test", "cmd", "local", 1, 1,
		uint16(cols), uint16(rows))
}

func TestSessionCore_RawSinceUsesSeqAndIsIdempotent(t *testing.T) {
	c := newTestCore(80, 24)
	c.consume([]byte("hello\n"))

	a := c.RawSince(0)
	if string(a.Data) != "hello\n" || a.Seq != 6 || a.Resync {
		t.Fatalf("first chunk = %q seq=%d resync=%v", a.Data, a.Seq, a.Resync)
	}
	if a.Cols != 80 || a.Rows != 24 {
		t.Errorf("chunk size = %dx%d, want 80x24", a.Cols, a.Rows)
	}

	c.consume([]byte("world\n"))
	b := c.RawSince(a.Seq)
	if string(b.Data) != "world\n" || b.Seq != 12 {
		t.Fatalf("second chunk = %q seq=%d, want exactly the new bytes", b.Data, b.Seq)
	}
	// A reconnect with the same cursor must replay exactly the same bytes.
	if again := c.RawSince(a.Seq); string(again.Data) != string(b.Data) {
		t.Errorf("RawSince is not idempotent: %q vs %q", again.Data, b.Data)
	}
	if empty := c.RawSince(b.Seq); len(empty.Data) != 0 {
		t.Errorf("unexpected bytes at head: %q", empty.Data)
	}
	// A cursor from the future must clamp, not produce a hole.
	if clamped := c.RawSince(999); clamped.Seq != 12 || len(clamped.Data) != 0 {
		t.Errorf("future cursor not clamped: seq=%d data=%q", clamped.Seq, clamped.Data)
	}
}

func TestSessionCore_RawSinceReportsResyncWhenWindowOverflows(t *testing.T) {
	cfg := DefaultSessionConfig()
	cfg.RawBufferBytes = 16
	c := newSessionCore(cfg, zap.NewNop().Sugar(), "sess-test", "cmd", "local", 1, 1, 80, 24)
	c.consume([]byte("0123456789"))
	c.consume([]byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ"))
	chunk := c.RawSince(0)
	if !chunk.Resync {
		t.Fatal("ring overflow must be reported so the web client resets xterm")
	}
	if len(chunk.Data) == 0 || chunk.Seq != 36 {
		t.Errorf("resync snapshot len=%d seq=%d, want a non-empty window and seq=36", len(chunk.Data), chunk.Seq)
	}
}

func TestSessionRegistry_RawSinceDoesNotConsumeAgentDeltas(t *testing.T) {
	cfg := DefaultSessionConfig()
	cfg.StableWindow = 0 // settle immediately so Poll has a semantic delta
	c := newSessionCore(cfg, zap.NewNop().Sugar(), "sess-test", "cmd", "local", 1, 1, 80, 24)
	c.consume([]byte("AAA\n"))
	reg := NewSessionRegistry(cfg, zap.NewNop().Sugar(), &coreTestSpawner{&coreTestHandle{c}})
	v, err := reg.Spawn(context.Background(), "bot-a", "default", SpawnRequest{Command: "cmd"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	sid := v.SessionID

	// A human attaching a browser tab must not advance the agent's cursor.
	chunk, err := reg.RawSince("bot-a", sid, 0)
	if err != nil {
		t.Fatalf("RawSince: %v", err)
	}
	if string(chunk.Data) != "AAA\n" {
		t.Errorf("web console got %q, want the full raw stream", chunk.Data)
	}
	if cur := reg.cursors[sid]["default"]; cur != 0 {
		t.Errorf("RawSince advanced the agent cursor to %d", cur)
	}
	if _, err := reg.RawSince("bot-b", sid, 0); err != ErrSessionWrongBot {
		t.Errorf("cross-bot RawSince error = %v, want ErrSessionWrongBot", err)
	}

	// The agent must still see its own delta after the web console attached.
	pv, err := reg.Poll("bot-a", "default", sid, 0, "screen", 0, 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !strings.Contains(pv.Screen+pv.Delta, "AAA") {
		t.Errorf("agent lost its content: screen=%q delta=%q", pv.Screen, pv.Delta)
	}
	again, err := reg.RawSince("bot-a", sid, chunk.Seq)
	if err != nil {
		t.Fatalf("second RawSince: %v", err)
	}
	if len(again.Data) != 0 {
		t.Errorf("agent poll rewound the raw stream: %q", again.Data)
	}
}

func TestSessionRegistry_ListIsReadOnlyAndIncludesQuietSessions(t *testing.T) {
	cfg := DefaultSessionConfig()
	cfg.RetainAfterExit = time.Hour // keep exited entries alive for this test
	c := newSessionCore(cfg, zap.NewNop().Sugar(), "sess-test", "cmd", "local", 1, 1, 80, 24)
	c.consume([]byte("quiet\n"))
	c.markFinished(0, SessionReasonExit)
	reg := NewSessionRegistry(cfg, zap.NewNop().Sugar(), &coreTestSpawner{&coreTestHandle{c}})
	v, err := reg.Spawn(context.Background(), "bot-a", "default", SpawnRequest{Command: "cmd"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	rows := reg.List("bot-a")
	if len(rows) != 1 || rows[0].SessionID != v.SessionID {
		t.Fatalf("List returned %d rows (%+v), want the exited session", len(rows), rows)
	}
	if rows[0].State != SessionExited || rows[0].Delta != "" || rows[0].Screen != "" {
		t.Errorf("List row is not a compact read-only snapshot: %+v", rows[0])
	}
	if reg.Count("bot-a") != 1 {
		t.Error("List ran GC and removed an exited session")
	}
	if cur := reg.cursors[v.SessionID]["default"]; cur != 0 {
		t.Errorf("List advanced the agent cursor to %d", cur)
	}
	if other := reg.List("bot-b"); len(other) != 0 {
		t.Errorf("bot-b sees %d sessions", len(other))
	}
}

func TestSessionCore_ResizeScreenRebuildsGrid(t *testing.T) {
	c := newTestCore(20, 5)
	c.consume([]byte(strings.Repeat("x", 30)))
	before := c.screen.Lines()
	if len(before) != 2 || len(before[0]) != 20 || len(before[1]) != 10 {
		t.Fatalf("baseline wrap = %d lines %v, want 20+10", len(before), lineLens(before))
	}

	c.resizeScreen(40, 10)
	if c.cols != 40 || c.rows != 10 {
		t.Fatalf("core size = %dx%d, want 40x10", c.cols, c.rows)
	}
	after := c.screen.Lines()
	if len(after) != 1 || len(after[0]) != 30 {
		t.Fatalf("rebuilt screen = %d lines %v, want one 30-char line", len(after), lineLens(after))
	}
	// poll's default line budget must follow the new winsize, not the spawn one.
	if v := c.View(); v.Screen != strings.Repeat("x", 30) {
		t.Errorf("View() after resize = %q", v.Screen)
	}
	c.resizeScreen(0, 0) // invalid winsize must be a no-op
	if c.cols != 40 || c.rows != 10 {
		t.Error("zero winsize changed the grid size")
	}
}

func lineLens(lines []string) []int {
	out := make([]int, len(lines))
	for i, l := range lines {
		out[i] = len([]rune(l))
	}
	return out
}

// Run with -race: consume() lives on the readLoop goroutine while Poll / View /
// RawSince / Resize come from tool and web-console goroutines.
func TestSessionCore_ConcurrentReadPollAndResize(t *testing.T) {
	c := newTestCore(80, 24)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			c.consume([]byte("line\r\n"))
			if i%20 == 0 {
				c.settle(time.Now())
			}
		}
	}()
	for i := 0; i < 200; i++ {
		_ = c.View()
		_, _ = c.Poll(0, "screen", 0, 0)
		_ = c.RawSince(0)
		if i%10 == 0 {
			c.resizeScreen(uint16(80+i%7), 24)
		}
	}
	<-done
}

// ----------------------------------------------------------------------------
// Production regressions found in the 2026-11-13 logs
// ----------------------------------------------------------------------------

// A program that clears its screen (or emits CR-only progress) can advance
// settledSeq while producing ZERO semantic delta records. Production showed
// "delta=” screen=” seq=1610 -> 6121": the agent polled blind, repeated the
// identical call, and the step guard killed the turn.
func testCoreImmediateSettle() *sessionCore {
	cfg := DefaultSessionConfig()
	cfg.StableWindow = 0 // settle on every ingest chunk, no wall-clock wait
	return newSessionCore(cfg, zap.NewNop().Sugar(), "sess-test", "cmd", "local", 1, 1, 80, 24)
}

func TestSessionCore_PollFallsBackToPlainTextTail(t *testing.T) {
	c := testCoreImmediateSettle()
	c.consume([]byte("PLAIN_OUTPUT_LINE\r\n\x1b[2J"))

	v, err := c.Poll(0, "screen", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.Screen != "" {
		t.Fatalf("screen should be blank after a clear, got %q", v.Screen)
	}
	if v.SettledSeq == 0 || v.Seq == 0 {
		t.Fatalf("watermarks did not advance: seq=%d settled=%d", v.Seq, v.SettledSeq)
	}
	if !strings.Contains(v.Delta, "PLAIN_OUTPUT_LINE") {
		t.Fatalf("agent is blind: delta = %q", v.Delta)
	}
	if strings.Contains(v.Delta, "\x1b") {
		t.Errorf("fallback leaked escape bytes: %q", v.Delta)
	}
	if !strings.Contains(v.Delta, "plain-text tail") {
		t.Errorf("fallback should be labelled so it is not mistaken for a screen diff: %q", v.Delta)
	}

	// Cursor discipline: the next poll must not replay the same bytes.
	v2, _ := c.Poll(v.SettledSeq, "screen", 0, 0)
	if v2.Delta != "" {
		t.Errorf("repeated cursor replayed output: %q", v2.Delta)
	}
	c.consume([]byte("SECOND_LINE\r\n\x1b[2J"))
	v3, _ := c.Poll(v.SettledSeq, "screen", 0, 0)
	if !strings.Contains(v3.Delta, "SECOND_LINE") || strings.Contains(v3.Delta, "PLAIN_OUTPUT_LINE") {
		t.Errorf("incremental fallback is wrong: %q", v3.Delta)
	}
}

func TestSessionCore_ModeTailIsAnExplicitEscapeHatch(t *testing.T) {
	c := testCoreImmediateSettle()
	c.consume([]byte("VISIBLE_LINE\r\n"))
	v, err := c.Poll(0, "tail", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.Delta, "VISIBLE_LINE") || !strings.Contains(v.Delta, "plain-text tail") {
		t.Fatalf("mode=tail must force the plain-text view, got %q", v.Delta)
	}
}

func TestSessionCore_PlainTextTailKeepsUTF8Valid(t *testing.T) {
	c := testCoreImmediateSettle()
	// > sessionTailMaxBytes of multi-byte text, ending in a blank screen.
	c.consume([]byte(strings.Repeat("中文输出行", 4000) + "\x1b[2J"))
	v, err := c.Poll(0, "screen", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(v.Delta) {
		t.Error("plain-text tail cut a UTF-8 rune in half")
	}
	if len(v.Delta) > sessionTailMaxBytes+256 {
		t.Errorf("tail is unbounded: %d bytes", len(v.Delta))
	}
	if !v.Truncated {
		t.Error("a clipped tail must be marked truncated")
	}
}

func TestSessionRegistry_NotFoundNamesLiveSessions(t *testing.T) {
	reg, _ := watchTestRegistry(t, DefaultSessionConfig())
	_, err := reg.Poll("bot", sessionAgent, "codebuddy-merge", 0, "screen", 0, 0)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("sentinel broken: %v", err)
	}
	if !strings.Contains(err.Error(), "sess-test") || !strings.Contains(err.Error(), "session_watch") {
		t.Errorf("error is not self-healing: %v", err)
	}
	if !strings.Contains(err.Error(), "never invent a name") {
		t.Errorf("error does not explain the contract: %v", err)
	}

	empty := NewSessionRegistry(DefaultSessionConfig(), zap.NewNop().Sugar(), nil)
	_, err = empty.RawSince("bot", "bogus", 0)
	if !errors.Is(err, ErrSessionNotFound) || !strings.Contains(err.Error(), "no live sessions") {
		t.Errorf("empty-registry error = %v", err)
	}
}

func TestSessionSendTool_DoesNotEmitUnmeasuredWarning(t *testing.T) {
	reg, _ := watchTestRegistry(t, DefaultSessionConfig())
	mgr := &BotWorkspaceManager{sessions: reg}
	tool := buildSessionSendTool(mgr, "bot")
	out, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"sessionId": "sess-test", "keys": "Enter"})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", out)
	}
	if _, found := m["warning"]; found {
		t.Errorf("send reports a discrepancy it never measured: %#v", m)
	}
	if m["written"] != 1 {
		t.Errorf("written = %v, want 1", m["written"])
	}
	if hint, _ := m["hint"].(string); !strings.Contains(hint, "does NOT prove") {
		t.Errorf("hint lost the consumption caveat: %q", hint)
	}
}
