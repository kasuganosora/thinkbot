package llm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

func toolNames(tools []Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

func simpleTool(name string, deferred bool, exec func() (any, error)) Tool {
	return Tool{
		Name:         name,
		Description:  "tool " + name,
		Parameters:   map[string]any{"type": "object"},
		DeferredLoad: deferred,
		Execute: func(*ToolExecContext, any) (any, error) {
			if exec != nil {
				return exec()
			}
			return name + "-ok", nil
		},
	}
}

// scriptedToolProvider records the tool names of every step, calls callTool
// on the first step and stops on the second.
type scriptedToolProvider struct {
	mu       sync.Mutex
	callTool string
	steps    [][]string
}

func (p *scriptedToolProvider) Name() string { return "scripted" }
func (p *scriptedToolProvider) DoGenerate(_ context.Context, params GenerateParams) (*GenerateResult, error) {
	p.mu.Lock()
	p.steps = append(p.steps, toolNames(params.Tools))
	n := len(p.steps)
	p.mu.Unlock()
	if n == 1 && p.callTool != "" {
		return &GenerateResult{
			FinishReason: FinishReasonToolCalls,
			ToolCalls:    []ToolCall{{ToolCallID: "c1", ToolName: p.callTool, Input: map[string]any{}}},
		}, nil
	}
	return &GenerateResult{FinishReason: FinishReasonStop, Text: "done"}, nil
}
func (p *scriptedToolProvider) DoStream(context.Context, GenerateParams) (*StreamResult, error) {
	return nil, errors.New("not implemented")
}

// TestOrchestrate_ConcurrentTurnsDoNotOverwriteToolList reproduces the
// 2026-09-26 incident: a Telegram turn (sandbox tools) and a Misskey timeline
// turn (reduced tool set) of the same bot share one ToolDeferral. The Misskey
// turn runs completely while the Telegram turn is between two steps; the
// Telegram turn's next step must still see exec/read_file.
func TestOrchestrate_ConcurrentTurnsDoNotOverwriteToolList(t *testing.T) {
	shared := NewToolDeferral(true) // the old per-bot fallback

	misskeyStarted := make(chan struct{})
	misskeyDone := make(chan struct{})
	tgTools := []Tool{
		simpleTool("exec", false, func() (any, error) {
			close(misskeyStarted)
			<-misskeyDone // the concurrent turn runs while this tool executes
			return "exec-ok", nil
		}),
		simpleTool("read_file", false, nil),
		simpleTool("memory", false, nil),
		simpleTool("mcp__browser", true, nil),
	}
	mkTools := []Tool{
		simpleTool("memory", false, nil),
		simpleTool("misskey_search_notes", true, nil),
	}

	tgProv := &scriptedToolProvider{callTool: "exec"}
	mkProv := &scriptedToolProvider{}

	var wg sync.WaitGroup
	wg.Add(1)
	var tgErr error
	go func() {
		defer wg.Done()
		_, tgErr = OrchestrateGenerate(context.Background(), tgProv, &OrchestrateConfig{
			Params:       GenerateParams{Messages: []Message{UserMessage("fix the code")}, Tools: tgTools},
			MaxSteps:     10,
			ToolDeferral: shared,
		})
	}()
	<-misskeyStarted
	if _, err := OrchestrateGenerate(context.Background(), mkProv, &OrchestrateConfig{
		Params:       GenerateParams{Messages: []Message{UserMessage("timeline note")}, Tools: mkTools},
		MaxSteps:     10,
		ToolDeferral: shared,
	}); err != nil {
		t.Fatal(err)
	}
	close(misskeyDone)
	wg.Wait()
	if tgErr != nil {
		t.Fatal(tgErr)
	}

	if len(tgProv.steps) != 2 {
		t.Fatalf("telegram turn: want 2 steps, got %v", tgProv.steps)
	}
	for i, step := range tgProv.steps {
		joined := strings.Join(step, ",")
		for _, want := range []string{"exec", "read_file", "memory", "mcp__browser"} {
			if !strings.Contains(","+joined+",", ","+want+",") {
				t.Errorf("telegram step %d lost %q: %v", i, want, step)
			}
		}
		if strings.Contains(joined, "misskey_search_notes") {
			t.Errorf("telegram step %d got the misskey tool list: %v", i, step)
		}
	}
	for i, step := range mkProv.steps {
		if strings.Contains(strings.Join(step, ","), "exec") {
			t.Errorf("misskey step %d got telegram's tools: %v", i, step)
		}
	}
}

// TestOrchestrate_ConcurrentTurnsRace hammers one shared deferral from many
// concurrent runs with different tool sets (run with -race); every step of a
// run must see exactly its own tools.
func TestOrchestrate_ConcurrentTurnsRace(t *testing.T) {
	shared := NewToolDeferral(true)
	const runs = 16
	var wg sync.WaitGroup
	errs := make(chan error, runs)
	for r := 0; r < runs; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			own := fmt.Sprintf("tool_%d", r)
			tools := []Tool{
				simpleTool(own, false, nil),
				simpleTool(fmt.Sprintf("deferred_%d", r), true, nil),
			}
			prov := &scriptedToolProvider{callTool: own}
			if _, err := OrchestrateGenerate(context.Background(), prov, &OrchestrateConfig{
				Params:       GenerateParams{Messages: []Message{UserMessage("go")}, Tools: tools},
				MaxSteps:     5,
				ToolDeferral: shared,
			}); err != nil {
				errs <- err
				return
			}
			want := []string{fmt.Sprintf("deferred_%d", r), own, "tool_search"}
			sort.Strings(want)
			for i, step := range prov.steps {
				if strings.Join(step, ",") != strings.Join(want, ",") {
					errs <- fmt.Errorf("run %d step %d: tools %v, want %v", r, i, step, want)
					return
				}
			}
		}(r)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Tools loaded via tool_search in one run are still loaded in the next run of
// the same conversation (the fork's state is adopted back).
func TestOrchestrate_ForkAdoptKeepsLoadedTools(t *testing.T) {
	d := NewToolDeferral(true)
	prov := new(deferFakeProvider)
	weather := simpleTool("mcp__weather", true, nil)
	weather.Description = "Get weather for a city"
	cfg := &OrchestrateConfig{
		Params:       GenerateParams{Messages: []Message{UserMessage("weather")}, Tools: []Tool{simpleTool("exec", false, nil), weather}},
		MaxSteps:     10,
		ToolDeferral: d,
	}
	if _, err := OrchestrateGenerate(context.Background(), prov, cfg); err != nil {
		t.Fatal(err)
	}
	if !d.IsLoaded("mcp__weather") {
		t.Fatal("loaded state of the run must be adopted by the conversation deferral")
	}
}

func TestToolDeferral_ForkIsIndependent(t *testing.T) {
	d := NewToolDeferral(true)
	d.SetTools([]Tool{simpleTool("a", false, nil), simpleTool("x", true, nil), simpleTool("y", true, nil)})
	d.Load("x")
	f := d.Fork()
	if !f.IsLoaded("x") {
		t.Fatal("fork must inherit loaded state")
	}
	f.SetTools([]Tool{simpleTool("b", false, nil)})
	f.Load("y")
	if got := toolNames(d.View()); strings.Join(got, ",") != "a,tool_search,x,y" {
		t.Fatalf("parent view changed by fork: %v", got)
	}
	if d.IsLoaded("y") {
		t.Fatal("fork load leaked into parent before Adopt")
	}
	d.Adopt(f)
	if !d.IsLoaded("y") || !d.IsLoaded("x") {
		t.Fatal("Adopt must copy the fork's loaded state")
	}
}

func TestToolSearch_NoMatchListsActualTools(t *testing.T) {
	d := NewToolDeferral(true)
	d.SetTools([]Tool{
		simpleTool("memory", false, nil),
		simpleTool("compact_context", false, nil),
		simpleTool("misskey_search_notes", true, nil),
	})
	out, err := d.ExecTool().Execute(&ToolExecContext{Context: context.Background()}, map[string]any{"query": "zzqx"})
	if err != nil {
		t.Fatal(err)
	}
	s := out.(string)
	if !strings.Contains(s, "compact_context, memory") {
		t.Errorf("no-match reply must list the directly callable tools: %s", s)
	}
	for _, bogus := range []string{"exec", "read_file", "browser_*", "misskey_search_notes"} {
		if strings.Contains(s, bogus) {
			t.Errorf("no-match reply must not claim %q is available: %s", bogus, s)
		}
	}
	if !strings.Contains(s, "no such tool is in your current tool list") {
		t.Errorf("reply should tell the model to report the gap plainly: %s", s)
	}
}

func TestNoMatchReply_CapsLongLists(t *testing.T) {
	var names []string
	for i := 0; i < maxNoMatchToolNames+5; i++ {
		names = append(names, fmt.Sprintf("t%03d", i))
	}
	s := noMatchReply("q", names)
	if !strings.Contains(s, "(+5 more)") || strings.Contains(s, fmt.Sprintf("t%03d", maxNoMatchToolNames)) {
		t.Fatalf("long list not capped: %s", s)
	}
	if s := noMatchReply("q", nil); !strings.Contains(s, "no directly callable tools") {
		t.Fatalf("empty list: %s", s)
	}
}

func TestToolViewGuard_DetectsShrink(t *testing.T) {
	start := []Tool{{Name: "exec"}, {Name: "read_file"}, {Name: "mcp__x"}, {Name: "tool_search"}}
	g := newToolViewGuard(start)
	// tool_search disappearing (all deferred tools loaded) is fine.
	if m := g.missing([]Tool{{Name: "exec"}, {Name: "read_file"}, {Name: "mcp__x"}}); len(m) != 0 {
		t.Fatalf("tool_search removal must not count: %v", m)
	}
	m := g.missing([]Tool{{Name: "mcp__x"}, {Name: "memory"}})
	if strings.Join(m, ",") != "exec,read_file" {
		t.Fatalf("missing=%v", m)
	}
	// check must be nil-safe and not panic.
	var nilGuard *toolViewGuard
	nilGuard.check(context.Background(), 1, nil)
	g.check(context.Background(), 1, []Tool{{Name: "mcp__x"}})
	g.check(context.Background(), 2, []Tool{{Name: "mcp__x"}})
}
