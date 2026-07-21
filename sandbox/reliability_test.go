package sandbox

import (
	"strings"
	"testing"
)

// ============================================================================
// stripOutputLimitingPipe — 剥离 LLM 自行追加的 `| head`/`| tail` 输出限制管道
// ============================================================================

func TestStripOutputLimitingPipe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		stri bool
	}{
		{"head -N", "golangci-lint run ./... 2>&1 | head -300", "golangci-lint run ./... 2>&1", true},
		{"head -n N", "go test ./... | head -n 50", "go test ./...", true},
		{"bare head", "cat file.log | head", "cat file.log", true},
		{"tail -N", "grep -r foo . | tail -20", "grep -r foo .", true},
		{"tail -n N", "ls -la | tail -n 5", "ls -la", true},
		{"no pipe", "golangci-lint run ./... --timeout 10m", "golangci-lint run ./... --timeout 10m", false},
		{"mid pipe kept", "cat a | grep x | head -10", "cat a | grep x", true},
		{"case insensitive", "go build ./... | HEAD -5", "go build ./...", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, stripped := stripOutputLimitingPipe(c.in)
			if stripped != c.stri {
				t.Errorf("stripped = %v, want %v", stripped, c.stri)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ============================================================================
// finalizeExecResult — 完整性 / 可信度信号
// 对应 docs/shell_reliable_result_design.md §9 测试计划
// ============================================================================

func TestFinalizeExecResult_Healthy(t *testing.T) {
	res := &ExecResult{ExitCode: 0, Stdout: "hello", Stderr: ""}
	finalizeExecResult(res)
	if res.Aborted {
		t.Error("expected not aborted")
	}
	if res.OOMKilled {
		t.Error("expected not oomKilled")
	}
	if !res.Reliable {
		t.Error("expected reliable=true for healthy exit 0")
	}
	if len(res.Warnings) != 0 {
		t.Errorf("expected no warnings, got %v", res.Warnings)
	}
}

func TestFinalizeExecResult_Timeout(t *testing.T) {
	res := &ExecResult{ExitCode: -1, Stderr: "partial"}
	finalizeExecResult(res)
	if !res.Aborted {
		t.Error("expected aborted for timeout (exit=-1)")
	}
	if res.Reliable {
		t.Error("expected reliable=false for timeout")
	}
	if !containsWarning(res.Warnings, "超时") {
		t.Errorf("expected timeout warning, got %v", res.Warnings)
	}
}

func TestFinalizeExecResult_Signal137(t *testing.T) {
	res := &ExecResult{ExitCode: 137}
	finalizeExecResult(res)
	if !res.Aborted {
		t.Error("expected aborted for exit=137")
	}
	if res.Reliable {
		t.Error("expected reliable=false for signal kill")
	}
}

func TestFinalizeExecResult_OOMTextScan(t *testing.T) {
	// 即使退出码为 0（被管道掩盖），输出文本含 "Killed" 也应判定不可信。
	res := &ExecResult{ExitCode: 0, Stderr: "make[1]: *** [all] Killed"}
	finalizeExecResult(res)
	if !res.Aborted {
		t.Error("expected aborted when output contains OOM/text feature")
	}
	if res.Reliable {
		t.Error("expected reliable=false when fatal text detected")
	}
	if !containsWarning(res.Warnings, "OOM/中止特征") {
		t.Errorf("expected fatal-text warning, got %v", res.Warnings)
	}
}

func TestFinalizeExecResult_PipelineMaskedExit0(t *testing.T) {
	// 关键场景：`cmd | tee | head` 让管道尾命令成功，外层退出码 0，
	// 但 cmd 实际被 OOM 杀死并在 stderr 留下 "out of memory"。
	res := &ExecResult{
		ExitCode: 0,
		Stdout:   "some output lines",
		Stderr:   "fatal error: runtime: out of memory",
	}
	finalizeExecResult(res)
	if !res.Aborted {
		t.Error("expected aborted for pipeline-masked OOM (exit 0 but oom text)")
	}
	if res.Reliable {
		t.Error("expected reliable=false for masked OOM")
	}
}

func TestFinalizeExecResult_OOMKilledFlagSet(t *testing.T) {
	// cgroup 对比在 ExecStream 层置 OOMKilled（调用方同时置 Aborted）。
	// finalizeExecResult 自身只保证 OOMKilled 时 Reliable=false，不重复置 Aborted。
	res := &ExecResult{ExitCode: 0, OOMKilled: true}
	finalizeExecResult(res)
	if res.Aborted {
		t.Error("finalizeExecResult must not set Aborted from OOMKilled (caller does)")
	}
	if res.Reliable {
		t.Error("expected reliable=false when OOMKilled")
	}
}

func TestFinalizeExecResult_TruncatedStillReliable(t *testing.T) {
	// Truncated 仅表示输出超长被截断，命令已完整执行，不应影响可信度。
	res := &ExecResult{ExitCode: 0, Truncated: true, Stdout: strings.Repeat("x", 50)}
	finalizeExecResult(res)
	if res.Aborted {
		t.Error("truncated output must not be treated as aborted")
	}
	if !res.Reliable {
		t.Error("truncated-but-complete output should remain reliable=true")
	}
}

func TestFinalizeExecResult_NilSafe(t *testing.T) {
	// 不 panic
	finalizeExecResult(nil)
}

// ============================================================================
// scanFatalText — 尾部 4KB 扫描 + 大小写不敏感
// ============================================================================

func TestScanFatalText_CaseInsensitive(t *testing.T) {
	if got := scanFatalText("FATAL ERROR: RUNTIME: OUT OF MEMORY"); got == "" {
		t.Error("expected match for uppercase OOM text")
	}
}

func TestScanFatalText_TailOnly(t *testing.T) {
	// 头部含 "killed" 但后面有大量无关内容，尾部没有 → 不应命中。
	head := strings.Repeat("killed-by-noise ", 0)
	head += "Killed"
	body := strings.Repeat("x", 6000)
	if got := scanFatalText(head + body); got != "" {
		t.Errorf("expected no tail match when fatal text only at head, got %q", got)
	}

	// 尾部含 "Killed" → 命中。
	if got := scanFatalText(body + "Killed"); got == "" {
		t.Error("expected tail match for fatal text at end")
	}
}

func TestScanFatalText_NoMatch(t *testing.T) {
	if got := scanFatalText("everything went fine, all tests passed"); got != "" {
		t.Errorf("expected empty for clean output, got %q", got)
	}
}

// ============================================================================
// parseOOMKill — v2 / v1 / 缺失
// ============================================================================

func TestParseOOMKill_V2(t *testing.T) {
	content := "low a\nhigh b\noom_kill 7\nevent_foo 0\n"
	n, ok := parseOOMKill(content)
	if !ok || n != 7 {
		t.Errorf("v2: got (%d, %v), want (7, true)", n, ok)
	}
}

func TestParseOOMKill_V1(t *testing.T) {
	content := "oom_kill 12\nunder_oom 0\noom_kill_setting 0\n"
	n, ok := parseOOMKill(content)
	if !ok || n != 12 {
		t.Errorf("v1: got (%d, %v), want (12, true)", n, ok)
	}
}

func TestParseOOMKill_Missing(t *testing.T) {
	if n, ok := parseOOMKill("some other metric 3\n"); ok {
		t.Errorf("expected ok=false for missing oom_kill, got n=%d", n)
	}
}

// ============================================================================
// isVerificationCommand — 验证型命令识别
// ============================================================================

func TestIsVerificationCommand(t *testing.T) {
	cases := map[string]bool{
		"golangci-lint run ./...":                    true,
		"go test ./...":                              true,
		"go build -o app .":                          true,
		"go vet ./...":                               true,
		"pytest -q":                                  true,
		"npm run build":                              true,
		"yarn test":                                  true,
		"make test":                                  true,
		"cargo test":                                 true,
		"grep -c 'func' *.go":                        true,
		"wc -l report.txt":                           true,
		"echo hello":                                 false,
		"ls -la":                                     false,
		"cat README.md":                              false,
		"curl https://example.com":                  false,
	}
	for cmd, want := range cases {
		if got := isVerificationCommand(cmd); got != want {
			t.Errorf("isVerificationCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestIsVerificationCommand_CaseInsensitive(t *testing.T) {
	if !isVerificationCommand("GOLANGCI-LINT run") {
		t.Error("expected case-insensitive match for verification command")
	}
}

// ============================================================================
// buildReliabilityWarning / execResultToToolOutput — 工具回传不可信提示
// ============================================================================

func TestBuildReliabilityWarning_OOM(t *testing.T) {
	res := &ExecResult{OOMKilled: true, Aborted: true, Warnings: []string{"oom"}}
	w := buildReliabilityWarning(res)
	if !strings.Contains(w, "OOM") {
		t.Errorf("expected OOM wording, got %q", w)
	}
	if !strings.Contains(w, "⚠️") {
		t.Errorf("expected warning emoji, got %q", w)
	}
}

func TestExecResultToToolOutput_Unreliable(t *testing.T) {
	res := &ExecResult{ExitCode: 0, Stdout: "partial", OOMKilled: true, Aborted: true, Warnings: []string{"oom"}}
	out := execResultToToolOutput(res, "/work")
	if out["reliable"] != false {
		t.Error("expected reliable=false in output")
	}
	if out["reliabilityWarning"] == nil {
		t.Error("expected reliabilityWarning field when unreliable")
	}
	stdout, ok := out["stdout"].(string)
	if !ok || !strings.HasPrefix(stdout, "⚠️") {
		t.Errorf("expected warning prepended to stdout, got %v", out["stdout"])
	}
}

func TestExecResultToToolOutput_Reliable(t *testing.T) {
	res := &ExecResult{ExitCode: 0, Stdout: "ok", Reliable: true}
	out := execResultToToolOutput(res, "/work")
	if out["reliabilityWarning"] != nil {
		t.Error("expected no reliabilityWarning when reliable")
	}
	if out["stdout"] != "ok" {
		t.Errorf("expected stdout unchanged when reliable, got %v", out["stdout"])
	}
}

// ============================================================================
// 辅助
// ============================================================================

func containsWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
