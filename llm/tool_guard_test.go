package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func guardCtx(cfg *ToolGuardConfig) context.Context {
	return withToolGuard(context.Background(), &OrchestrateConfig{ToolGuard: cfg})
}

func guardTools(fns map[string]func(input any) (any, error)) map[string]*Tool {
	m := map[string]*Tool{}
	for name, fn := range fns {
		fn := fn
		m[name] = &Tool{Name: name, Execute: func(_ *ToolExecContext, in any) (any, error) { return fn(in) }}
	}
	return m
}

func call(name string, input any) ToolCall {
	return ToolCall{ToolCallID: name, ToolName: name, Input: input}
}

func runStep(t *testing.T, ctx context.Context, tm map[string]*Tool, calls ...ToolCall) []ToolResultPart {
	t.Helper()
	res, err := executeTools(ctx, calls, tm, nil, nil, &OrchestrateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func resultText(p ToolResultPart) string {
	switch r := p.Result.(type) {
	case string:
		return r
	case map[string]any:
		s, _ := r["tool_guard"].(string)
		return s
	}
	return ""
}

func TestToolGuard_IdenticalCallsBlocked(t *testing.T) {
	ctx := guardCtx(nil)
	runs := 0
	tm := guardTools(map[string]func(any) (any, error){
		"browser__fetch": func(any) (any, error) { runs++; return "same page", nil },
		"read_file":      func(any) (any, error) { return "x", nil },
	})
	in := map[string]any{"url": "https://example.com"}
	for i := 0; i < 3; i++ {
		if r := runStep(t, ctx, tm, call("browser__fetch", in)); r[0].IsError {
			t.Fatalf("call %d should run: %v", i+1, r[0].Result)
		}
	}
	r := runStep(t, ctx, tm, call("browser__fetch", map[string]any{"url": "https://example.com"}))
	if !r[0].IsError || !strings.Contains(resultText(r[0]), "Not executed") || runs != 3 {
		t.Fatalf("4th identical call must be refused without running (runs=%d): %v", runs, r[0].Result)
	}
	// Another call in between resets the streak.
	runStep(t, ctx, tm, call("read_file", map[string]any{"path": "a"}))
	if r := runStep(t, ctx, tm, call("browser__fetch", in)); r[0].IsError {
		t.Fatalf("after a different call the identical streak resets: %v", r[0].Result)
	}
}

func TestToolGuard_PollingWithChangingOutputNotBlocked(t *testing.T) {
	ctx := guardCtx(nil)
	n := 0
	tm := guardTools(map[string]func(any) (any, error){
		"exec": func(any) (any, error) {
			n++
			return map[string]any{"exitCode": 0, "stdout": strings.Repeat(".", n)}, nil
		},
	})
	for i := 0; i < 6; i++ {
		if r := runStep(t, ctx, tm, call("exec", map[string]any{"command": "tail build.log"})); r[0].IsError {
			t.Fatalf("polling with new output must not be blocked (i=%d)", i)
		}
	}
}

func TestToolGuard_ParallelDuplicatesInOneStep(t *testing.T) {
	ctx := guardCtx(nil)
	tm := guardTools(map[string]func(any) (any, error){"web_fetch": func(any) (any, error) { return "ok", nil }})
	in := map[string]any{"url": "u"}
	r := runStep(t, ctx, tm, call("web_fetch", in), call("web_fetch", in), call("web_fetch", in), call("web_fetch", in))
	if r[0].IsError || r[2].IsError || !r[3].IsError {
		t.Fatalf("only the 4th identical call in one step is refused: %v", r)
	}
}

func TestToolGuard_ConsecutiveFailuresDisableTool(t *testing.T) {
	ctx := guardCtx(nil)
	runs := 0
	tm := guardTools(map[string]func(any) (any, error){
		"browser__fill":     func(any) (any, error) { runs++; return nil, errors.New("Element is not an <input>") },
		"browser__get_text": func(any) (any, error) { return "page", nil },
	})
	var last []ToolResultPart
	for i := 0; i < 5; i++ {
		last = runStep(t, ctx, tm, call("browser__fill", map[string]any{"selector": i}))
		// successes of OTHER tools do not reset the failure streak
		runStep(t, ctx, tm, call("browser__get_text", map[string]any{"i": i}))
		if i == 3 && !strings.Contains(resultText(last[0]), "one more failure") {
			t.Errorf("4th failure should warn: %q", resultText(last[0]))
		}
	}
	if !strings.Contains(resultText(last[0]), "disabled for the rest of this turn") {
		t.Fatalf("5th failure should disable: %q", resultText(last[0]))
	}
	r := runStep(t, ctx, tm, call("browser__fill", map[string]any{"selector": "new"}))
	if !r[0].IsError || runs != 5 || !strings.Contains(resultText(r[0]), "ask how to proceed") {
		t.Fatalf("disabled tool must not run (runs=%d): %v", runs, r[0].Result)
	}
}

func TestToolGuard_SuccessResetsFailures(t *testing.T) {
	ctx := guardCtx(nil)
	fail := true
	tm := guardTools(map[string]func(any) (any, error){
		"web_search": func(any) (any, error) {
			if fail {
				return nil, errors.New("boom")
			}
			return "ok", nil
		},
	})
	for i := 0; i < 4; i++ {
		runStep(t, ctx, tm, call("web_search", map[string]any{"q": i}))
	}
	fail = false
	runStep(t, ctx, tm, call("web_search", map[string]any{"q": "ok"}))
	fail = true
	for i := 0; i < 4; i++ {
		if r := runStep(t, ctx, tm, call("web_search", map[string]any{"q": 10 + i})); strings.Contains(resultText(r[0]), "disabled") {
			t.Fatal("a success must reset the failure streak")
		}
	}
}

func TestToolGuard_ExecNonZeroExitAndPerToolCap(t *testing.T) {
	ctx := guardCtx(&ToolGuardConfig{PerToolMaxFailures: map[string]int{"exec": 3}})
	tm := guardTools(map[string]func(any) (any, error){
		"exec": func(in any) (any, error) { return map[string]any{"exitCode": 1, "stderr": "no"}, nil },
	})
	var r []ToolResultPart
	for i := 0; i < 3; i++ {
		r = runStep(t, ctx, tm, call("exec", map[string]any{"command": i}))
	}
	m, ok := r[0].Result.(map[string]any)
	if !ok || m["exitCode"] != 1 || !strings.Contains(m["tool_guard"].(string), "disabled") {
		t.Fatalf("non-zero exits count as failures and the per-tool cap applies: %#v", r[0].Result)
	}
}

func TestToolGuard_StreakAdvisoryAndExempt(t *testing.T) {
	ctx := guardCtx(nil)
	tm := guardTools(map[string]func(any) (any, error){
		"exec": func(in any) (any, error) { return map[string]any{"exitCode": 0}, nil },
		"now":  func(any) (any, error) { return "t", nil },
	})
	var r []ToolResultPart
	for i := 0; i < 8; i++ {
		r = runStep(t, ctx, tm, call("exec", map[string]any{"command": i}))
		if i < 7 && resultText(r[0]) != "" {
			t.Fatalf("no advisory before the 8th call: %q", resultText(r[0]))
		}
	}
	if !strings.Contains(resultText(r[0]), "8 times in a row") {
		t.Fatalf("8th consecutive call should carry the advisory: %#v", r[0].Result)
	}
	for i := 0; i < 10; i++ {
		if r := runStep(t, ctx, tm, call("now", nil)); r[0].IsError {
			t.Fatal("exempt tools are never blocked")
		}
	}
}

func TestToolGuard_Disabled(t *testing.T) {
	ctx := guardCtx(&ToolGuardConfig{Disabled: true})
	tm := guardTools(map[string]func(any) (any, error){"x": func(any) (any, error) { return nil, errors.New("e") }})
	for i := 0; i < 10; i++ {
		if r := runStep(t, ctx, tm, call("x", nil)); strings.Contains(resultText(r[0]), "tool guard") {
			t.Fatal("disabled guard must not interfere")
		}
	}
}

func TestToolGuard_LiveSource(t *testing.T) {
	SetToolGuardSource(func() ToolGuardConfig { return ToolGuardConfig{MaxIdenticalCalls: 1} })
	defer SetToolGuardSource(nil)
	ctx := withToolGuard(context.Background(), &OrchestrateConfig{})
	tm := guardTools(map[string]func(any) (any, error){"a": func(any) (any, error) { return "same", nil }})
	runStep(t, ctx, tm, call("a", "1"))
	if r := runStep(t, ctx, tm, call("a", "1")); !r[0].IsError {
		t.Fatal("live source MaxIdenticalCalls=1 should refuse the 2nd identical call")
	}
}

func TestToolResultFailed(t *testing.T) {
	cases := []struct {
		p    ToolResultPart
		want bool
	}{
		{ToolResultPart{IsError: true}, true},
		{ToolResultPart{Result: map[string]any{"exit_code": 2}}, true},
		{ToolResultPart{Result: map[string]any{"exitCode": float64(0)}}, false},
		{ToolResultPart{Result: `{"exitCode": 127, "stdout": "..."}` + strings.Repeat("x", 100)}, true},
		{ToolResultPart{Result: "plain ok"}, false},
	}
	for i, c := range cases {
		if got := toolResultFailed(c.p); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}
