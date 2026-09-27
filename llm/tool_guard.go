package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ============================================================================
// Tool call guard — per-run caps on wasted tool calls
//
// Production evidence (2026-09-26/27): exec called 10–18 times in a row,
// browser fill/click failing 11 times in one turn (65 calls, then
// javascript: URLs), replace_in_file retried 7 times with the same wrong
// old_str. The whole-step loop controller (orchestrate_loop.go) only catches
// a step repeated verbatim; these are per-tool patterns. The guard adds,
// per orchestration run (one user turn / one subagent task):
//
//  1. identical consecutive calls: the same tool with the same arguments
//     returning the same result MaxIdenticalCalls times in a row → further
//     identical calls are not executed and get an error telling the model to
//     change approach (polling whose output changes is never blocked);
//  2. consecutive failures per tool: after MaxConsecutiveFailures failed
//     calls of one tool without a success in between (errors, or non-zero
//     exitCode/exit_code), the tool is blocked for the rest of the run. One
//     failure before the cap the result carries a warning;
//  3. a streak advisory: the same tool called StreakAdvisory times in a row
//     (different arguments) gets a note suggesting batching into one script
//     or stopping to report — advisory only, nothing is blocked.
//
// All messages tell the model to change approach or ask the user.
// ============================================================================

// ToolGuardConfig configures the per-run tool call guard.
type ToolGuardConfig struct {
	// Disabled turns the guard off.
	Disabled bool
	// MaxIdenticalCalls: identical consecutive calls allowed (default 3).
	MaxIdenticalCalls int
	// MaxConsecutiveFailures: consecutive failures of one tool before it is
	// blocked for the rest of the run (default 5).
	MaxConsecutiveFailures int
	// PerToolMaxFailures overrides MaxConsecutiveFailures for specific tools
	// (defaults: exec-like tools 8, since non-zero exits are often expected).
	PerToolMaxFailures map[string]int
	// StreakAdvisory: consecutive calls of one tool before an advisory note
	// (default 8; <0 disables).
	StreakAdvisory int
	// Exempt tools are never counted or blocked.
	Exempt []string
}

// DefaultToolGuardConfig returns the defaults.
func DefaultToolGuardConfig() ToolGuardConfig {
	return ToolGuardConfig{
		MaxIdenticalCalls:      3,
		MaxConsecutiveFailures: 5,
		PerToolMaxFailures: map[string]int{
			"exec": 8, "sandbox_exec": 8, "run_code": 8, "shell": 8,
		},
		StreakAdvisory: 8,
		Exempt: []string{
			"now", "random", "uuid", "user_choice",
			"task", "task_detail", "task_control", "tool_search",
		},
	}
}

// withDefaults fills zero fields from DefaultToolGuardConfig.
func (c ToolGuardConfig) withDefaults() ToolGuardConfig {
	d := DefaultToolGuardConfig()
	if c.MaxIdenticalCalls <= 0 {
		c.MaxIdenticalCalls = d.MaxIdenticalCalls
	}
	if c.MaxConsecutiveFailures <= 0 {
		c.MaxConsecutiveFailures = d.MaxConsecutiveFailures
	}
	merged := make(map[string]int, len(d.PerToolMaxFailures)+len(c.PerToolMaxFailures))
	for k, v := range d.PerToolMaxFailures {
		merged[k] = v
	}
	for k, v := range c.PerToolMaxFailures {
		if v > 0 {
			merged[k] = v
		}
	}
	c.PerToolMaxFailures = merged
	if c.StreakAdvisory == 0 {
		c.StreakAdvisory = d.StreakAdvisory
	}
	if c.Exempt == nil {
		c.Exempt = d.Exempt
	}
	return c
}

var toolGuardSource atomic.Pointer[func() ToolGuardConfig]

// SetToolGuardSource installs a process-wide live config source (read once at
// the start of each orchestration run). nil restores the defaults.
func SetToolGuardSource(fn func() ToolGuardConfig) {
	if fn == nil {
		toolGuardSource.Store(nil)
		return
	}
	toolGuardSource.Store(&fn)
}

func currentToolGuardConfig(cfg *OrchestrateConfig) ToolGuardConfig {
	if cfg != nil && cfg.ToolGuard != nil {
		return cfg.ToolGuard.withDefaults()
	}
	if p := toolGuardSource.Load(); p != nil {
		return (*p)().withDefaults()
	}
	return DefaultToolGuardConfig()
}

// toolGuard is the per-run state.
type toolGuard struct {
	cfg    ToolGuardConfig
	exempt map[string]bool

	mu           sync.Mutex
	lastSig      string // signature of the last executed (non-exempt) call
	lastOut      string // digest of its result
	identical    int    // how many times lastSig ran in a row with the same result
	lastTool     string
	streak       int // consecutive calls of lastTool
	failures     map[string]int
	blocked      map[string]bool
	blockedCalls int
}

type toolGuardKey struct{}

// withToolGuard attaches a fresh guard for one orchestration run.
func withToolGuard(ctx context.Context, cfg *OrchestrateConfig) context.Context {
	gc := currentToolGuardConfig(cfg)
	if gc.Disabled {
		return context.WithValue(ctx, toolGuardKey{}, (*toolGuard)(nil))
	}
	g := &toolGuard{
		cfg:      gc,
		exempt:   make(map[string]bool, len(gc.Exempt)),
		failures: make(map[string]int),
		blocked:  make(map[string]bool),
	}
	for _, n := range gc.Exempt {
		g.exempt[n] = true
	}
	return context.WithValue(ctx, toolGuardKey{}, g)
}

func toolGuardFrom(ctx context.Context) *toolGuard {
	g, _ := ctx.Value(toolGuardKey{}).(*toolGuard)
	return g
}

func guardCallSignature(tc ToolCall) string {
	var args string
	switch in := tc.Input.(type) {
	case nil:
	case string:
		args = normalizeJSONString(in)
	case json.RawMessage:
		args = normalizeJSONString(string(in))
	case []byte:
		args = normalizeJSONString(string(in))
	default:
		if b, err := json.Marshal(in); err == nil {
			args = string(b)
		} else {
			args = fmt.Sprintf("%v", in)
		}
	}
	return tc.ToolName + "\x00" + args
}

// normalizeJSONString re-encodes a JSON document so key order/whitespace do
// not make identical calls look different.
func normalizeJSONString(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return strings.TrimSpace(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(s)
	}
	return string(b)
}

func (g *toolGuard) maxFailures(tool string) int {
	if n, ok := g.cfg.PerToolMaxFailures[tool]; ok && n > 0 {
		return n
	}
	return g.cfg.MaxConsecutiveFailures
}

// precheck decides whether a call of this step may run. seen carries the
// signatures already admitted in this step (identical parallel calls count).
// Returns a non-empty refusal when the call must not run.
func (g *toolGuard) precheck(tc ToolCall, seen map[string]int) string {
	if g == nil || g.exempt[tc.ToolName] {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.blocked[tc.ToolName] {
		g.blockedCalls++
		return fmt.Sprintf("[tool guard] %s is disabled for the rest of this turn: it failed %d times in a row. "+
			"Do not call it again now. Change approach (a different tool or method, or fix the root cause you saw in the errors), "+
			"or tell the user what is failing and ask how to proceed.", tc.ToolName, g.failures[tc.ToolName])
	}
	sig := guardCallSignature(tc)
	n := seen[sig]
	if sig == g.lastSig {
		n += g.identical
	}
	if n >= g.cfg.MaxIdenticalCalls {
		g.blockedCalls++
		return fmt.Sprintf("[tool guard] Not executed: %s was already called with exactly these arguments %d times in a row "+
			"and returned the same result each time; repeating it will not help. Use the result you already have, change the arguments or the approach, "+
			"or ask the user if you are stuck.", tc.ToolName, n)
	}
	seen[sig]++
	return ""
}

// record updates the state with executed results and returns a notice to
// append to each result ("" = none). Blocked calls (refusals) are skipped.
func (g *toolGuard) record(calls []ToolCall, results []ToolResultPart, refused []bool) []string {
	notices := make([]string, len(results))
	if g == nil {
		return notices
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, tc := range calls {
		if i >= len(results) || refused[i] || g.exempt[tc.ToolName] {
			continue
		}
		var notes []string
		sig, out := guardCallSignature(tc), resultDigest(results[i])
		if sig == g.lastSig && out == g.lastOut {
			g.identical++
		} else {
			g.lastSig, g.lastOut, g.identical = sig, out, 1
		}
		if tc.ToolName == g.lastTool {
			g.streak++
		} else {
			g.lastTool, g.streak = tc.ToolName, 1
		}

		if toolResultFailed(results[i]) {
			g.failures[tc.ToolName]++
			f, max := g.failures[tc.ToolName], g.maxFailures(tc.ToolName)
			switch {
			case f >= max:
				g.blocked[tc.ToolName] = true
				notes = append(notes, fmt.Sprintf("[tool guard] %s has now failed %d times in a row, so it is disabled for the rest of this turn. "+
					"Stop retrying variations of the same thing: change approach, or explain to the user what is failing and ask how to proceed.", tc.ToolName, f))
			case f == max-1:
				notes = append(notes, fmt.Sprintf("[tool guard] %s failed %d times in a row; one more failure disables it for this turn. "+
					"Before calling it again, read the error carefully and change something substantive (arguments, method or tool), or ask the user.", tc.ToolName, f))
			}
		} else {
			g.failures[tc.ToolName] = 0
		}

		if a := g.cfg.StreakAdvisory; a > 0 && g.streak >= a && g.streak%a == 0 {
			notes = append(notes, fmt.Sprintf("[tool guard] You have called %s %d times in a row. Batch the remaining work into one step "+
				"(e.g. a single script via run_code or one exec running several commands), or stop and report what you have so far.", tc.ToolName, g.streak))
		}
		notices[i] = strings.Join(notes, "\n")
	}
	return notices
}

// resultDigest hashes a result so identical repeats can be told apart from
// polling that returns new output.
func resultDigest(p ToolResultPart) string {
	var b []byte
	if s, ok := p.Result.(string); ok {
		b = []byte(s)
	} else if j, err := json.Marshal(p.Result); err == nil {
		b = j
	} else {
		b = []byte(fmt.Sprintf("%v", p.Result))
	}
	sum := sha256.Sum256(append(b, boolByte(p.IsError)))
	return hex.EncodeToString(sum[:8])
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

var exitCodeInText = regexp.MustCompile(`"exit_?[cC]ode"\s*:\s*(-?\d+)`)

// toolResultFailed reports whether a tool result is a failure: an error
// result, or a command result with a non-zero exit code.
func toolResultFailed(p ToolResultPart) bool {
	if p.IsError {
		return true
	}
	switch r := p.Result.(type) {
	case map[string]any:
		for _, k := range []string{"exitCode", "exit_code"} {
			if v, ok := r[k]; ok {
				return exitCodeNonZero(v)
			}
		}
	case string:
		// Truncated/offloaded command output is a string; the exit code is
		// near the start of the JSON rendering.
		head := r
		if len(head) > 4096 {
			head = head[:4096]
		}
		if m := exitCodeInText.FindStringSubmatch(head); m != nil {
			n, err := strconv.Atoi(m[1])
			return err == nil && n != 0
		}
	}
	return false
}

func exitCodeNonZero(v any) bool {
	switch n := v.(type) {
	case int:
		return n != 0
	case int64:
		return n != 0
	case int32:
		return n != 0
	case float64:
		return n != 0
	case json.Number:
		return n.String() != "0"
	case string:
		return n != "" && n != "0"
	}
	return false
}

// annotateToolResult appends a guard notice to a tool result.
func annotateToolResult(p *ToolResultPart, notice string) {
	if notice == "" {
		return
	}
	switch r := p.Result.(type) {
	case string:
		p.Result = r + "\n\n" + notice
	case map[string]any:
		cp := make(map[string]any, len(r)+1)
		for k, v := range r {
			cp[k] = v
		}
		cp["tool_guard"] = notice
		p.Result = cp
	case nil:
		p.Result = notice
	default:
		p.Result = map[string]any{"result": r, "tool_guard": notice}
	}
}
