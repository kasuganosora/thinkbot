package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

// fakeAutoRegistry is a controllable autoUpgradeSession. It never starts a
// real process: the point of these tests is the three-way decision, the
// no-orphan guarantee and the zero-output negative condition.
type fakeAutoRegistry struct {
	mu       sync.Mutex
	view     SessionView
	raw      []byte
	spawnErr error
	spawned  int
	closed   int
	req      SpawnRequest
	onView   func(*fakeAutoRegistry)
}

func (f *fakeAutoRegistry) Spawn(_ context.Context, _ string, _ string, req SpawnRequest) (SessionView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawned++
	f.req = req
	if f.spawnErr != nil {
		return SessionView{}, f.spawnErr
	}
	return f.view, nil
}

func (f *fakeAutoRegistry) View(_, _ string) (SessionView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onView != nil {
		f.onView(f)
	}
	return f.view, nil
}

func (f *fakeAutoRegistry) RawSince(_, _ string, _ uint64) (RawChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return RawChunk{Data: append([]byte(nil), f.raw...), Seq: uint64(len(f.raw)), State: f.view.State}, nil
}

func (f *fakeAutoRegistry) Close(_, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

var _ autoUpgradeSession = (*fakeAutoRegistry)(nil)

func autoTestConfig() SessionConfig {
	cfg := DefaultSessionConfig()
	cfg.AutoUpgradeWindow = 60 * time.Millisecond
	cfg.AutoUpgradeMaxWait = 250 * time.Millisecond
	return cfg
}

func TestAutoUpgrade_ExitedInWindowReturnsOneShotAndCloses(t *testing.T) {
	code := 0
	f := &fakeAutoRegistry{
		view: SessionView{
			SessionID: "s1", State: SessionExited, ExitCode: &code,
			Seq: 12, SettledSeq: 12, Screen: "hello\n",
		},
		raw: []byte("\x1b[32mhello\x1b[0m\r\n"),
	}
	out, err := runAutoUpgrade(context.Background(), f, "bot", SpawnRequest{Command: "echo hello", Cwd: "/data"}, autoTestConfig())
	if err != nil {
		t.Fatalf("runAutoUpgrade: %v", err)
	}
	if out["upgraded"] != false || out["reason"] != autoReasonExited {
		t.Fatalf("out = %#v", out)
	}
	stdout, _ := out["stdout"].(string)
	if !strings.Contains(stdout, "hello") {
		t.Errorf("already-captured output was lost: stdout=%q", stdout)
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Errorf("ANSI escape left in stdout: %q", stdout)
	}
	if out["exitCode"] != 0 || out["reliable"] != true {
		t.Errorf("one-shot contract broken: exitCode=%v reliable=%v", out["exitCode"], out["reliable"])
	}
	if out["workdir"] != "/data" {
		t.Errorf("workdir = %v", out["workdir"])
	}
	if f.closed != 1 {
		t.Errorf("non-upgrade branch left a live session: closed=%d", f.closed)
	}
}

func TestAutoUpgrade_AliveWithVisibleOutputUpgradesWithoutClosing(t *testing.T) {
	f := &fakeAutoRegistry{
		view: SessionView{
			SessionID: "s2", State: SessionRunning,
			Seq: 42, SettledSeq: 40, Screen: "  ❯ Continue?\n",
			Health: SessionHealth{Verdict: HealthAwaitingAction, Actionable: true},
		},
		raw: []byte("  ❯ Continue?\n"),
	}
	out, err := runAutoUpgrade(context.Background(), f, "bot", SpawnRequest{Command: "codebuddy"}, autoTestConfig())
	if err != nil {
		t.Fatalf("runAutoUpgrade: %v", err)
	}
	if out["upgraded"] != true || out["sessionId"] != "s2" {
		t.Fatalf("out = %#v", out)
	}
	if !strings.Contains(out["upgradeReason"].(string), "actionable_prompt") {
		t.Errorf("upgrade evidence missing: %v", out["upgradeReason"])
	}
	if out["settledSeq"] != uint64(40) {
		t.Errorf("cursor not surfaced: %v", out["settledSeq"])
	}
	if f.closed != 0 {
		t.Errorf("upgraded session must stay owned by the registry, closed=%d", f.closed)
	}
}

func TestAutoUpgrade_ZeroOutputAliveIsNeverUpgraded(t *testing.T) {
	// §3.2: real CLI startup failure = alive, no bytes, no screen.
	f := &fakeAutoRegistry{
		view: SessionView{
			SessionID: "s3", State: SessionRunning, Seq: 0, SettledSeq: 0, Screen: "   \n",
			BootDiag: "EADDRINUSE", StderrTail: "listen: address already in use",
		},
		raw: []byte("\x1b[2J\x1b[H\x00\x00"), // CSI-only + NULs: seq>0 but no content
	}
	out, err := runAutoUpgrade(context.Background(), f, "bot", SpawnRequest{Command: "cli"}, autoTestConfig())
	if err != nil {
		t.Fatalf("runAutoUpgrade: %v", err)
	}
	if out["upgraded"] != false || out["reason"] != autoReasonNoOutput {
		t.Fatalf("zero-output alive MUST NOT be upgraded: %#v", out)
	}
	if out["bootDiag"] != "EADDRINUSE" {
		t.Errorf("startup diagnostic not surfaced: %v", out["bootDiag"])
	}
	if out["reliable"] != false {
		t.Errorf("diagnostic result must not claim reliability: %v", out["reliable"])
	}
	if f.closed != 1 {
		t.Errorf("rejected session must be closed, closed=%d", f.closed)
	}
}

func TestAutoUpgrade_SlowStarterUpgradesOnceOutputAppears(t *testing.T) {
	f := &fakeAutoRegistry{
		view: SessionView{SessionID: "s4", State: SessionRunning, Screen: ""},
		raw:  nil,
	}
	// Output appears after the short window but before maxWait.
	f.onView = func(fr *fakeAutoRegistry) {
		if fr.view.Screen == "" {
			fr.view.Screen = "ready>"
			fr.view.Seq = 6
		}
	}
	out, err := runAutoUpgrade(context.Background(), f, "bot", SpawnRequest{Command: "slow"}, autoTestConfig())
	if err != nil {
		t.Fatalf("runAutoUpgrade: %v", err)
	}
	if out["upgraded"] != true {
		t.Fatalf("slow starter should upgrade after visible output: %#v", out)
	}
}

func TestAutoUpgrade_SpawnFailureIsSurfacedNotReExecuted(t *testing.T) {
	f := &fakeAutoRegistry{spawnErr: errors.New("unsupported")}
	_, err := runAutoUpgrade(context.Background(), f, "bot", SpawnRequest{Command: "x"}, autoTestConfig())
	if err == nil {
		t.Fatal("expected spawn error")
	}
	if f.spawned != 1 {
		t.Errorf("command must not be re-executed, spawned=%d", f.spawned)
	}
}

func TestAutoUpgrade_ContextCancelClosesUnhandedSession(t *testing.T) {
	f := &fakeAutoRegistry{
		view: SessionView{SessionID: "s5", State: SessionRunning, Screen: ""},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	cfg := autoTestConfig()
	cfg.AutoUpgradeWindow = 5 * time.Second
	cfg.AutoUpgradeMaxWait = 5 * time.Second
	out, err := runAutoUpgrade(ctx, f, "bot", SpawnRequest{Command: "hang"}, cfg)
	if err != nil {
		t.Fatalf("runAutoUpgrade: %v", err)
	}
	if out["upgraded"] != false || f.closed != 1 {
		t.Fatalf("abort must close the not-yet-handed-over session: %#v closed=%d", out, f.closed)
	}
}

func TestExecTool_BackgroundParamAbsentWithoutGuard(t *testing.T) {
	plain := buildExecTool(nil, "bot")
	props := plain.Parameters.(map[string]any)["properties"].(map[string]any)
	if _, ok := props["background"]; ok {
		t.Fatal("background parameter must NOT be visible when spawn is not permitted / no permission context")
	}
	guarded := buildExecToolWithAuto(nil, "bot", func() bool { return true })
	props2 := guarded.Parameters.(map[string]any)["properties"].(map[string]any)
	if _, ok := props2["background"]; !ok {
		t.Fatal("background parameter missing when guard is present")
	}
}

func TestExecTool_BackgroundAutoRejectsWhenSpawnDenied(t *testing.T) {
	tool := buildExecToolWithAuto(nil, "bot", func() bool { return false })
	_, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{"command": "echo x", "background": "auto"})
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("call-time permission re-check failed: %v", err)
	}
}

func TestExecTool_DefaultExecPathUnchanged(t *testing.T) {
	// The default (no background key) must behave exactly like the original
	// one-shot exec even when the auto guard IS present: real stdout + exitCode,
	// no session created, no upgrade keys in the result.
	mgr, botID, cleanup := newTestBotMgr(t)
	defer cleanup()
	guardCalls := 0
	tool := buildExecToolWithAuto(mgr, botID, func() bool { guardCalls++; return true })
	out, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"command": "echo sync_still_works"})
	if err != nil && strings.Contains(err.Error(), "background") {
		t.Fatalf("sync path was rerouted into auto logic: %v", err)
	}
	// The guard is consulted ONLY by the auto branch. Zero calls proves the
	// default path never entered the permission/session code at all.
	if guardCalls != 0 {
		t.Errorf("sync path touched the auto guard %d times", guardCalls)
	}
	if mgr.Sessions().Count(botID) != 0 {
		t.Error("sync path created a background session")
	}
	if err == nil {
		// On Linux the real exec succeeds; assert the full one-shot contract.
		res := out.(map[string]any)
		if !strings.Contains(res["stdout"].(string), "sync_still_works") {
			t.Errorf("stdout = %v", res["stdout"])
		}
		if _, ok := res["upgraded"]; ok {
			t.Error("sync path leaked auto-upgrade keys")
		}
	}
	// darwin: "confinement is not available on darwin" is a known baseline
	// environment limit (docs §11), not a regression of this change.
}

func TestStripTerminalBytes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ansi color", "\x1b[31mred\x1b[0m\n", "red\n"},
		{"crlf", "a\r\nb\r\n", "a\nb\n"},
		{"bare cr", "a\rb\r", "a\nb\n"},
		{"osc title", "\x1b]0;title\x07ok\n", "ok\n"},
		{"osc st", "\x1b]0;title\x1b\\ok\n", "ok\n"},
		{"csi cursor only", "\x1b[2J\x1b[H\x00", ""},
		{"nul bytes", "a\x00b\n", "ab\n"},
		{"plain utf8", "选择: 继续\n", "选择: 继续\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripTerminalBytes([]byte(tc.in)); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestHasVisibleOutput(t *testing.T) {
	if hasVisibleOutput(SessionView{State: SessionRunning, Seq: 200, Screen: "  \n\t"}) {
		t.Error("CSI-only output with an empty screen must count as zero output")
	}
	if !hasVisibleOutput(SessionView{State: SessionRunning, Seq: 4, Screen: " ok "}) {
		t.Error("visible screen content must count as output")
	}
}
