package llm

import (
	"context"
	"testing"
)

func deferTestTool(name string, deferred bool) Tool {
	return Tool{
		Name:         name,
		Description:  "desc for " + name,
		Parameters:   map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
		DeferredLoad: deferred,
		Execute: func(ctx *ToolExecContext, input any) (any, error) {
			return "ok", nil
		},
	}
}

func TestToolDeferral_ViewStripsUnloaded(t *testing.T) {
	d := NewToolDeferral(true)
	normal := deferTestTool("exec", false)
	deferred := deferTestTool("mcp__srv__foo", true)
	d.SetTools([]Tool{normal, deferred})

	if !d.HasDeferred() {
		t.Fatal("expected HasDeferred true")
	}

	view := d.View()
	if len(view) != 3 {
		// normal + deferred(stripped) + tool_search
		t.Fatalf("expected 3 tools in view (incl tool_search), got %d: %+v", len(view), view)
	}

	for _, tt := range view {
		switch tt.Name {
		case "exec":
			if tt.Parameters == nil {
				t.Errorf("normal tool %q should keep Parameters", tt.Name)
			}
		case "mcp__srv__foo":
			if tt.Parameters != nil {
				t.Errorf("deferred tool %q should have Parameters stripped in view, got %v", tt.Name, tt.Parameters)
			}
			if !tt.DeferredLoad {
				t.Errorf("deferred flag should survive in view")
			}
		case "tool_search":
			// expected injected search tool
		default:
			t.Errorf("unexpected tool in view: %q", tt.Name)
		}
	}
}

// TestToolDeferral_MatchAvailableRedirectsNonDeferred verifies that
// matchAvailable surfaces always-available (non-deferred) tools so tool_search
// can steer the model back to them instead of reporting a misleading
// "No matching tools found" (which previously made the model abandon file
// tools mid-task).
func TestToolDeferral_MatchAvailableRedirectsNonDeferred(t *testing.T) {
	d := NewToolDeferral(true)
	exec := deferTestTool("exec", false)      // always available, non-deferred
	read := deferTestTool("read_file", false) // always available, non-deferred
	deferred := deferTestTool("mcp__srv__foo", true)
	d.SetTools([]Tool{exec, read, deferred})

	// A query matching an always-available tool must be reported.
	got := d.matchAvailable("exec")
	if len(got) != 1 || got[0].Name != "exec" {
		t.Fatalf("expected exec reported as available, got %+v", got)
	}
	// Substring match on name/description also works.
	got = d.matchAvailable("read")
	if len(got) != 1 || got[0].Name != "read_file" {
		t.Fatalf("expected read_file reported, got %+v", got)
	}

	// An unloaded deferred tool must NOT be reported by matchAvailable — that
	// is Search()'s job (load on demand).
	if got := d.matchAvailable("mcp__srv__foo"); len(got) != 0 {
		t.Fatalf("unloaded deferred tool should not be in matchAvailable, got %+v", got)
	}

	// A truly unknown query returns nothing.
	if got := d.matchAvailable("nonexistent-xyz"); got != nil {
		t.Fatalf("expected nil for unknown query, got %+v", got)
	}

	// Once a deferred tool is loaded it becomes directly callable, so
	// matchAvailable should surface it too.
	d.Load("mcp__srv__foo")
	if got := d.matchAvailable("mcp__srv__foo"); len(got) != 1 {
		t.Fatalf("loaded deferred tool should be reported as available, got %+v", got)
	}
}

// TestToolDeferral_SearchExecReportsAvailable ensures the search closure points
// the model at an already-available tool rather than a false "No matching".
func TestToolDeferral_SearchExecReportsAvailable(t *testing.T) {
	d := NewToolDeferral(true)
	exec := deferTestTool("exec", false)
	read := deferTestTool("read_file", false)
	deferred := deferTestTool("mcp__srv__foo", true)
	d.SetTools([]Tool{exec, read, deferred})

	out, err := d.searchTool().Execute(&ToolExecContext{}, map[string]any{"query": "exec"})
	if err != nil {
		t.Fatalf("searchTool.Execute error: %v", err)
	}
	s, ok := out.(string)
	if !ok {
		t.Fatalf("expected string result, got %T", out)
	}
	if !contains(s, "exec") || !contains(s, "ALREADY") {
		t.Fatalf("expected search to redirect to available exec tool, got: %q", s)
	}
	if contains(s, "No matching tools found") {
		t.Fatalf("search must not report false negative for available tool, got: %q", s)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (indexOf(haystack, needle) >= 0)
}

// TestToolDeferral_SearchAliasRedirectsToAvailable 验证：模型用自然语言能力词
// （"edit file" / "run git" / "shell command"）搜 tool_search 时，应当回指到已经
// 直接可用的 exec/read_file/replace_in_file，而不是返回误导性的「No matching」。
// 这是 cfblog 长任务反复「工具消失」幻觉的根因修复。
func TestToolDeferral_SearchAliasRedirectsToAvailable(t *testing.T) {
	d := NewToolDeferral(true)
	exec := deferTestTool("exec", false)
	read := deferTestTool("read_file", false)
	replace := deferTestTool("replace_in_file", false)
	d.SetTools([]Tool{exec, read, replace})

	cases := []struct {
		query   string
		mustSee []string // 返回里应出现的已可用工具名
	}{
		{"edit file", []string{"read_file", "replace_in_file"}},
		{"write a new file", []string{"read_file", "replace_in_file"}},
		{"run git commit", []string{"exec"}},
		{"execute a shell command", []string{"exec"}},
		{"list directory", []string{"read_file"}}, // list_dir 未注册，但 read_file 也在别名映射里兜底
	}
	for _, c := range cases {
		out, err := d.ExecTool().Execute(&ToolExecContext{}, map[string]any{"query": c.query})
		if err != nil {
			t.Fatalf("query %q: Execute error: %v", c.query, err)
		}
		s := out.(string)
		if contains(s, "No matching tools found") {
			t.Fatalf("query %q: must not emit misleading 'No matching tools found', got: %q", c.query, s)
		}
		for _, name := range c.mustSee {
			if !contains(s, name) {
				t.Fatalf("query %q: expected redirect to available tool %q, got: %q", c.query, name, s)
			}
		}
	}
}

// TestToolDeferral_SearchUnknownCapabilityNoFalseNegative 验证：对真正未知的能力，
// 兜底消息不再使用「No matching tools found」这种会被模型解读为「工具不存在」的措辞，
// 而是提醒常驻工具（exec/read_file 等）始终可直接调用。
func TestToolDeferral_SearchUnknownCapabilityNoFalseNegative(t *testing.T) {
	d := NewToolDeferral(true)
	exec := deferTestTool("exec", false)
	d.SetTools([]Tool{exec})

	out, err := d.ExecTool().Execute(&ToolExecContext{}, map[string]any{"query": "post to twitter"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	s := out.(string)
	if contains(s, "No matching tools found") {
		t.Fatalf("must not emit misleading 'No matching tools found', got: %q", s)
	}
	if !contains(s, "exec") {
		t.Fatalf("fallback should remind that exec is always callable, got: %q", s)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestToolDeferral_SearchLoadsAndShowsFull(t *testing.T) {
	d := NewToolDeferral(true)
	deferred := deferTestTool("mcp__srv__weather", true)
	deferred.Keywords = []string{"forecast", "temperature"}
	d.SetTools([]Tool{deferred})

	hits := d.Search("weather")
	if len(hits) != 1 || hits[0].Name != "mcp__srv__weather" {
		t.Fatalf("expected 1 hit 'mcp__srv__weather', got %+v", hits)
	}
	if !d.IsLoaded("mcp__srv__weather") {
		t.Fatal("tool should be loaded after Search")
	}

	// After load, View shows the full schema (not stripped).
	view := d.View()
	var found *Tool
	for i := range view {
		if view[i].Name == "mcp__srv__weather" {
			found = &view[i]
		}
	}
	if found == nil {
		t.Fatal("loaded tool should appear in view")
	}
	if found.Parameters == nil {
		t.Error("loaded tool should show full Parameters in view")
	}

	// keyword match also works
	d2 := NewToolDeferral(true)
	kwTool := deferTestTool("mcp__srv__weather", true)
	kwTool.Keywords = []string{"forecast", "temperature"}
	d2.SetTools([]Tool{kwTool})
	d2.Search("forecast")
	if !d2.IsLoaded("mcp__srv__weather") {
		t.Error("keyword 'forecast' should match via Keywords")
	}
}

func TestToolDeferral_NoDeferredNoSearchTool(t *testing.T) {
	d := NewToolDeferral(true)
	normal := deferTestTool("exec", false)
	d.SetTools([]Tool{normal})

	if d.HasDeferred() {
		t.Fatal("expected HasDeferred false")
	}
	view := d.View()
	if len(view) != 1 {
		t.Fatalf("expected exactly 1 tool, got %d", len(view))
	}
	if view[0].Name != "exec" {
		t.Errorf("unexpected tool: %q", view[0].Name)
	}
}

func TestToolDeferral_DisabledReturnsAll(t *testing.T) {
	d := NewToolDeferral(false)
	deferred := deferTestTool("mcp__srv__foo", true)
	d.SetTools([]Tool{deferred})

	view := d.View()
	if len(view) != 1 {
		t.Fatalf("disabled deferral should return all tools unchanged, got %d", len(view))
	}
	if view[0].Parameters == nil {
		t.Error("disabled deferral must not strip Parameters")
	}
}

func TestLoadTriggeredDeferredTools(t *testing.T) {
	d := NewToolDeferral(true)
	normal := deferTestTool("exec", false)
	deferred := deferTestTool("mcp__srv__foo", true)
	d.SetTools([]Tool{normal, deferred})
	toolMap := buildToolMap([]Tool{normal, deferred})

	// Unloaded deferred call → triggers load.
	trig := loadTriggeredDeferredTools([]ToolCall{{ToolName: "mcp__srv__foo"}}, toolMap, d)
	if len(trig) != 1 || trig[0] != "mcp__srv__foo" {
		t.Fatalf("expected trigger for unloaded deferred tool, got %+v", trig)
	}

	// After loading, no trigger.
	d.Load("mcp__srv__foo")
	trig = loadTriggeredDeferredTools([]ToolCall{{ToolName: "mcp__srv__foo"}}, toolMap, d)
	if len(trig) != 0 {
		t.Fatalf("loaded deferred tool should not trigger, got %+v", trig)
	}

	// Normal tool never triggers.
	trig = loadTriggeredDeferredTools([]ToolCall{{ToolName: "exec"}}, toolMap, d)
	if len(trig) != 0 {
		t.Fatalf("normal tool should not trigger, got %+v", trig)
	}

	// Unknown tool → no trigger (not in map).
	trig = loadTriggeredDeferredTools([]ToolCall{{ToolName: "nope"}}, toolMap, d)
	if len(trig) != 0 {
		t.Fatalf("unknown tool should not trigger, got %+v", trig)
	}
}

func TestToolDeferral_SearchToolExecutes(t *testing.T) {
	d := NewToolDeferral(true)
	deferred := deferTestTool("mcp__srv__weather", true)
	d.SetTools([]Tool{deferred})

	st := d.ExecTool()
	if st.Name != "tool_search" {
		t.Fatalf("expected tool_search, got %q", st.Name)
	}
	out, err := st.Execute(&ToolExecContext{Context: context.Background()}, map[string]any{"query": "weather"})
	if err != nil {
		t.Fatalf("search execute failed: %v", err)
	}
	if out == nil || out.(string) == "" {
		t.Fatal("expected non-empty search result")
	}
	if !d.IsLoaded("mcp__srv__weather") {
		t.Error("search should have loaded the matched tool")
	}
}

func TestToolDeferral_IdleEviction(t *testing.T) {
	d := NewToolDeferral(true)
	d.SetCapacity(0, 5) // unlimited cap, idle-evict after 5 idle steps
	deferred := deferTestTool("mcp__srv__foo", true)
	d.SetTools([]Tool{deferred})

	d.SetStep(0)
	d.Load("mcp__srv__foo")
	if !d.IsLoaded("mcp__srv__foo") {
		t.Fatal("should be loaded at step 0")
	}

	// Step 5: 5-0 = 5, not > 5 → still loaded.
	d.SetStep(5)
	if !d.IsLoaded("mcp__srv__foo") {
		t.Error("should remain loaded at step 5 (idle threshold not exceeded)")
	}

	// Step 6: 6-0 = 6 > 5 → idle-evicted.
	d.SetStep(6)
	if d.IsLoaded("mcp__srv__foo") {
		t.Error("should be idle-evicted by step 6")
	}
}

func TestToolDeferral_TouchKeepsHot(t *testing.T) {
	d := NewToolDeferral(true)
	d.SetCapacity(0, 5)
	deferred := deferTestTool("mcp__srv__foo", true)
	d.SetTools([]Tool{deferred})

	d.SetStep(0)
	d.Load("mcp__srv__foo") // lastUsed = 0

	// Keep it hot by touching it within each step, as the orchestration loop
	// does right after executing a deferred tool.
	for s := 0; s <= 10; s++ {
		d.SetStep(s)
		d.Touch("mcp__srv__foo")
	}
	if !d.IsLoaded("mcp__srv__foo") {
		t.Fatal("should remain loaded while touched every step")
	}

	// Now idle: it was last used at step 10, so step 16 (16-10 = 6 > 5)
	// should idle-evict it.
	d.SetStep(16)
	if d.IsLoaded("mcp__srv__foo") {
		t.Error("should be evicted by step 16 after going idle post-touch")
	}
}

func TestToolDeferral_CapacityEvictsLRU(t *testing.T) {
	d := NewToolDeferral(true)
	d.SetCapacity(2, 0) // hard cap 2, no idle eviction
	a := deferTestTool("a", true)
	b := deferTestTool("b", true)
	c := deferTestTool("c", true)
	d.SetTools([]Tool{a, b, c})

	d.SetStep(0)
	d.Load("a") // recency [a]
	d.SetStep(1)
	d.Load("b") // recency [a, b]
	d.SetStep(2)
	d.Load("c") // over cap → evict LRU (a)

	if d.IsLoaded("a") {
		t.Error("a should be LRU-evicted (cap 2)")
	}
	if !d.IsLoaded("b") || !d.IsLoaded("c") {
		t.Error("b and c should remain loaded")
	}
	if d.LoadedCount() != 2 {
		t.Errorf("expected 2 loaded after cap eviction, got %d", d.LoadedCount())
	}
}

func TestToolDeferral_HasUnloaded(t *testing.T) {
	d := NewToolDeferral(true)
	a := deferTestTool("a", true)
	b := deferTestTool("b", true)
	d.SetTools([]Tool{a, b})

	if !d.HasUnloaded() {
		t.Fatal("should report unloaded tools initially")
	}
	d.SetStep(0)
	d.Load("a")
	d.Load("b")
	if d.HasUnloaded() {
		t.Error("no deferred tool should be unloaded after loading all")
	}
	d.Unload("a")
	if !d.HasUnloaded() {
		t.Error("unloading a should make HasUnloaded true again")
	}
}

func TestToolDeferral_ViewToolSearchOnlyWhenUnloaded(t *testing.T) {
	d := NewToolDeferral(true)
	deferred := deferTestTool("mcp__srv__foo", true)
	d.SetTools([]Tool{deferred})

	view := d.View()
	foundSearch := false
	for _, tt := range view {
		if tt.Name == "tool_search" {
			foundSearch = true
		}
	}
	if !foundSearch {
		t.Fatal("tool_search should be injected while a deferred tool is unloaded")
	}

	d.SetStep(0)
	d.Load("mcp__srv__foo")
	view = d.View()
	for _, tt := range view {
		if tt.Name == "tool_search" {
			t.Error("tool_search should NOT be injected once all deferred tools are loaded")
		}
	}
}
