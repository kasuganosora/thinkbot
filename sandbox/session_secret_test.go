package sandbox

// ============================================================================
// SSH-scenario regressions: credential masking, prompt verdicts and the
// unresolved-placeholder guard.
//
// All hosts / credentials below are synthetic. Real SSH experiment evidence
// lives only in the local work log, never in the repository (docs §14).
// ============================================================================

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/secret"
	ulog "github.com/kasuganosora/thinkbot/util/log"
)

const testCredential = "S3CR3T-PIN-9876"

// withSessionVault swaps the process-wide vault for an in-memory one. Every
// registered value also enters util/log's redaction table, so it must be
// unregistered again or it would mask unrelated text in later tests.
func withSessionVault(t *testing.T, name, value string) *secret.Vault {
	t.Helper()
	prev := secret.Default()
	v, err := secret.New("") // in-memory: no disk file
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	if err := v.Register("bot", name, value, "ssh regression"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	secret.SetDefault(v)
	t.Cleanup(func() {
		secret.SetDefault(prev)
		ulog.UnregisterSecret(value)
	})
	return v
}

func TestSecretMaskStream_SplitsAcrossChunks(t *testing.T) {
	withSessionVault(t, "pin", testCredential)
	m := &SecretStream{}

	first := m.Feed([]byte("PIN?" + testCredential[:5]))
	if strings.Contains(string(first), testCredential[:5]) {
		t.Fatalf("partial credential prefix leaked: %q", first)
	}
	if string(first) != "PIN?" {
		t.Fatalf("normal text was delayed: %q", first)
	}
	second := m.Feed([]byte(testCredential[5:] + "\r\n"))
	all := string(first) + string(second)
	if strings.Contains(all, testCredential) {
		t.Fatalf("credential leaked across chunks: %q", all)
	}
	if !strings.Contains(all, secret.Placeholder("pin")) {
		t.Fatalf("credential not replaced by placeholder: %q", all)
	}
	if tail := m.Flush(); len(tail) != 0 {
		t.Fatalf("unexpected withheld tail: %q", tail)
	}
}

func TestSecretMaskStream_NormalTextIsNotDelayed(t *testing.T) {
	withSessionVault(t, "pin", testCredential)
	m := &SecretStream{}
	in := []byte("root@devbox:~# echo SSHLAB_MARKER_42\r\n")
	got := m.Feed(in)
	if string(got) != string(in) {
		t.Fatalf("normal terminal bytes were altered: %q", got)
	}
	if tail := m.Flush(); len(tail) != 0 {
		t.Fatalf("normal text was withheld: %q", tail)
	}
}

func TestSecretMaskStream_FlushNeverEmitsPartialCredential(t *testing.T) {
	withSessionVault(t, "pin", testCredential)
	m := &SecretStream{}
	_ = m.Feed([]byte("PIN?" + testCredential[:9]))
	tail := m.Flush()
	if strings.Contains(string(tail), testCredential[:9]) {
		t.Fatalf("flush leaked a partial credential: %q", tail)
	}
	if string(tail) != "***" {
		t.Fatalf("flush = %q, want a redaction marker", tail)
	}
}

func TestSessionCore_EchoedCredentialNeverLeavesTheCore(t *testing.T) {
	withSessionVault(t, "pin", testCredential)
	c := newTestCore(100, 30)
	// Echo-ON prompt: the line discipline writes the typed value back into the
	// master stream. ssh's own password prompt sets ECHO=0, but `read -p`,
	// sudo-with-echo and PIN dialogs do not.
	c.consume([]byte("PIN?" + testCredential + "\r\n"))
	c.consume([]byte("GOT:" + testCredential + "\r\n"))
	c.consume([]byte("see https://example.com/t?token=" + testCredential + "\r\n"))

	v := c.View()
	if strings.Contains(v.Screen, testCredential) || strings.Contains(v.Delta, testCredential) {
		t.Fatalf("plaintext in view: screen=%q delta=%q", v.Screen, v.Delta)
	}
	for _, u := range v.URLs {
		if strings.Contains(u, testCredential) {
			t.Fatalf("plaintext in extracted URL: %q", u)
		}
	}
	if !strings.Contains(v.Screen, secret.Placeholder("pin")) {
		t.Fatalf("screen lost the placeholder: %q", v.Screen)
	}
	raw := c.RawSince(0)
	if strings.Contains(string(raw.Data), testCredential) {
		t.Fatalf("plaintext in WS replay: %q", raw.Data)
	}
	if raw.Seq != uint64(len(raw.Data)) {
		t.Fatalf("seq=%d but replay len=%d: screen and replay would diverge", raw.Seq, len(raw.Data))
	}
}

func withEmptySessionVault(t *testing.T) {
	t.Helper()
	prev := secret.Default()
	v, err := secret.New("")
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })
}

func TestSessionCore_LateRegisteredCredentialIsRedactedOnEgress(t *testing.T) {
	// No credential registered at capture time: ingress masking is a no-op.
	withEmptySessionVault(t)
	c := newTestCore(100, 30)
	c.consume([]byte("GOT:" + testCredential + "\r\n"))
	if !strings.Contains(string(c.RawSince(0).Data), testCredential) {
		t.Fatal("precondition failed: plaintext should be in the ring before registration")
	}
	withSessionVault(t, "pin", testCredential)
	if got := string(c.RawSince(0).Data); strings.Contains(got, testCredential) {
		t.Fatalf("late-registered credential leaked via WS replay: %q", got)
	}
	if got := c.View().Screen; strings.Contains(got, testCredential) {
		t.Fatalf("late-registered credential leaked via screen: %q", got)
	}
}

func TestSessionCore_FlushOnExitEmitsMarkerNotPartialCredential(t *testing.T) {
	withSessionVault(t, "pin", testCredential)
	c := newTestCore(100, 30)
	c.consume([]byte("PIN?" + testCredential[:9]))
	c.markFinished(0, SessionReasonExit)
	raw := c.RawSince(0)
	if strings.Contains(string(raw.Data), testCredential[:9]) {
		t.Fatalf("exit flush leaked a partial credential: %q", raw.Data)
	}
	if !strings.Contains(string(raw.Data), "***") {
		t.Fatalf("exit flush did not emit a marker: %q", raw.Data)
	}
	if raw.Seq != uint64(len(raw.Data)) {
		t.Fatalf("seq=%d replay len=%d after flush", raw.Seq, len(raw.Data))
	}
}

func TestPlaceholderNames(t *testing.T) {
	got := secret.PlaceholderNames("x {{secret:a}} y {{secret:a}} {{ secret:b }} plain")
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("PlaceholderNames = %v, want [a b]", got)
	}
	if secret.PlaceholderNames("plain {{other:x}}") != nil {
		t.Fatal("non-secret mustache token must not be treated as a placeholder")
	}
	if secret.PlaceholderNames("resolved-value") != nil {
		t.Fatal("resolved value must not look like a placeholder")
	}
}

func TestRejectUnresolvedPlaceholders(t *testing.T) {
	if err := rejectUnresolvedPlaceholders("send", "plain text", "y"); err != nil {
		t.Fatalf("plain input rejected: %v", err)
	}
	err := rejectUnresolvedPlaceholders("send", "p={{secret:missing}}")
	if err == nil || !errors.Is(err, ErrSessionBadInput) {
		t.Fatalf("want ErrSessionBadInput, got %v", err)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error does not name the missing secret: %v", err)
	}
}

func TestSessionSendTool_RejectsUnresolvedPlaceholder(t *testing.T) {
	c := newTestCore(80, 24)
	mgr := &BotWorkspaceManager{
		sessions: NewSessionRegistry(DefaultSessionConfig(), zap.NewNop().Sugar(),
			&coreTestSpawner{&coreTestHandle{c}}),
	}
	tool := buildSessionSendTool(mgr, "bot")
	_, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"sessionId": "sess-x", "text": "{{secret:missing}}"})
	if err == nil || !errors.Is(err, ErrSessionBadInput) {
		t.Fatalf("send with unresolved placeholder should fail loudly, got %v", err)
	}
}

func TestSessionSpawnTool_RejectsCredentialPlaceholderInCommand(t *testing.T) {
	c := newTestCore(80, 24)
	mgr := &BotWorkspaceManager{
		sessions: NewSessionRegistry(DefaultSessionConfig(), zap.NewNop().Sugar(),
			&coreTestSpawner{&coreTestHandle{c}}),
	}
	tool := buildSessionSpawnTool(mgr, "bot")
	_, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"command": "ssh -p {{secret:ssh.port}} example.com"})
	if err == nil || !errors.Is(err, ErrSessionBadInput) {
		t.Fatalf("spawn with unresolved placeholder should fail loudly, got %v", err)
	}
}

// TestHealthVerdict_RealSSHScreenTails replays the screen shapes observed in a
// real `ssh -tt` login (host key -> password -> remote shell -> nested read ->
// Ctrl+C -> exit) with every host and credential replaced by synthetic values.
// The pre-fix verdict was awaiting_action for ALL 8 stages: the first-connect
// question stayed in the 40-row grid and outranked everything afterwards.
func TestHealthVerdict_RealSSHScreenTails(t *testing.T) {
	cfg := DefaultSessionConfig()
	now := time.Now()
	hostKey := []string{
		"The authenticity of host 'example.com' can't be established.",
		"ED25519 key fingerprint is SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAA.",
		"This key is not known by any other names.",
		"Are you sure you want to continue connecting (yes/no/[fingerprint])?",
	}
	loginBanner := []string{
		"Linux devbox 6.1.0-54-amd64 #1 SMP PREEMPT_DYNAMIC",
		"The programs included with the Debian GNU/Linux system are free software;",
		"permitted by applicable law.",
		"Last login: Thu Nov 12 06:53:51 2026 from 198.51.100.7",
	}

	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"1 host key confirmation", hostKey, HealthAwaitingAction},
		{"2 password prompt with host key in scrollback",
			append(append([]string{}, hostKey...), "root@example.com's password:"), HealthAwaitingSecret},
		{"3 remote shell after login",
			append(append([]string{}, loginBanner...), "root@devbox:~#"), HealthIdleAwaitingAgent},
		{"4 command output then shell prompt",
			[]string{"SSHLAB_MARKER_42", "root", "Linux", "root@devbox:~#"}, HealthIdleAwaitingAgent},
		{"5 nested echo-on PIN prompt",
			[]string{"root@devbox:~# read -p 'PIN?' x", "PIN?"}, HealthAwaitingSecret},
		{"6 back to shell after nested answer",
			[]string{"PIN?***", "GOT:***", "root@devbox:~#"}, HealthIdleAwaitingAgent},
		{"7 after Ctrl+C",
			[]string{"root@devbox:~# ^C", "root@devbox:~#"}, HealthIdleAwaitingAgent},
		{"8 authentication failure then new password prompt",
			[]string{"Permission denied, please try again.", "root@example.com's password:"}, HealthConnectFailed},
		{"9 stale failure scrolled out of the prompt window",
			[]string{"Permission denied, please try again.", "step 1", "step 2", "step 3", "root@devbox:~#"}, HealthIdleAwaitingAgent},
		{"10 connection refused",
			[]string{"ssh: connect to host example.com port 22: Connection refused"}, HealthConnectFailed},
		{"11 blank rows below cursor do not dilute the tail",
			append(append([]string{}, loginBanner...), "root@devbox:~#", "", "", ""), HealthIdleAwaitingAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHealthTracker(cfg)
			h.Feed(10, 1, tc.lines, now)
			v := h.Verdict(SessionRunning, true, tc.lines, now)
			if v.Verdict != tc.want {
				t.Fatalf("verdict = %q (evidence %q), want %q", v.Verdict, v.Evidence, tc.want)
			}
		})
	}
}

func TestHealthVerdict_ActionableOnCurrentRowBeatsStaleHistory(t *testing.T) {
	cfg := DefaultSessionConfig()
	now := time.Now()
	lines := []string{
		"root@devbox:~# echo one",
		"one",
		"Do you want to proceed? [y/n]",
	}
	h := newHealthTracker(cfg)
	h.Feed(10, 1, lines, now)
	if v := h.Verdict(SessionRunning, true, lines, now); v.Verdict != HealthAwaitingAction {
		t.Fatalf("current confirmation lost to idle state: %+v", v)
	}
}
