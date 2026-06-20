package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// 辅助：创建测试用的 ToolManager
// ============================================================================

func newTestToolMgr(t *testing.T) *agenttools.ToolManager {
	t.Helper()
	// 需要真实的 prompt.Registry 来避免 nil panic
	reg := prompt.NewRegistry()
	mgr := agenttools.NewToolManager(reg, nil, nil)
	return mgr
}

func execTool(t *testing.T, tool llm.Tool, input any) any {
	t.Helper()
	result, err := tool.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		input,
	)
	if err != nil {
		t.Fatalf("tool %q failed: %v", tool.Name, err)
	}
	return result
}

// ============================================================================
// now 工具测试
// ============================================================================

func TestNow_Basic(t *testing.T) {
	tool := buildNowTool("Asia/Shanghai")
	result := execTool(t, tool, map[string]any{})
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", result)
	}
	if tz, _ := m["timezone"].(string); tz != "Asia/Shanghai" {
		t.Errorf("timezone: got %q", tz)
	}
	if _, ok := m["datetime"]; !ok {
		t.Error("missing datetime field")
	}
	if _, ok := m["weekday"]; !ok {
		t.Error("missing weekday field")
	}
}

func TestNow_InvalidTimezone_FallbackUTC(t *testing.T) {
	tool := buildNowTool("Invalid/Zone")
	result := execTool(t, tool, map[string]any{})
	m := result.(map[string]any)
	if tz, _ := m["timezone"].(string); tz != "UTC" {
		t.Errorf("expected UTC fallback, got %q", tz)
	}
}

func TestNow_Provider_PerBotTimezone(t *testing.T) {
	provider := &nowToolProvider{
		resolveTimezone: func(botID string) string {
			if botID == "tokyo-bot" {
				return "Asia/Tokyo"
			}
			return "UTC"
		},
	}

	// Tokyo bot
	tools, err := provider.Tools(context.Background(), &agenttools.ToolSessionContext{
		BotID: "tokyo-bot",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := execTool(t, tools[0], map[string]any{})
	m := result.(map[string]any)
	if tz, _ := m["timezone"].(string); tz != "Asia/Tokyo" {
		t.Errorf("tokyo-bot: expected Asia/Tokyo, got %q", tz)
	}

	// Other bot
	tools, _ = provider.Tools(context.Background(), &agenttools.ToolSessionContext{
		BotID: "other-bot",
	})
	result = execTool(t, tools[0], map[string]any{})
	m = result.(map[string]any)
	if tz, _ := m["timezone"].(string); tz != "UTC" {
		t.Errorf("other-bot: expected UTC, got %q", tz)
	}
}

func TestNow_Provider_NilContext(t *testing.T) {
	provider := &nowToolProvider{}
	tools, err := provider.Tools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	result := execTool(t, tools[0], map[string]any{})
	m := result.(map[string]any)
	if tz, _ := m["timezone"].(string); tz != "UTC" {
		t.Errorf("nil context: expected UTC, got %q", tz)
	}
}

// ============================================================================
// calculate 工具测试
// ============================================================================

func TestCalculate_Basic(t *testing.T) {
	def := calculateToolDef()
	cases := []struct {
		expr     string
		expected any
	}{
		{"1 + 2", int64(3)},
		{"(1 + 2) * 3", int64(9)},
		{"10 / 3", 10.0 / 3.0},
		{"2^10", int64(1024)},
		{"10 % 3", float64(1)},
		{"-5 + 3", int64(-2)},
		{"3.5 * 2", 7.0},
	}
	for _, tc := range cases {
		result := execTool(t, def.Tool, map[string]any{"expression": tc.expr})
		m := result.(map[string]any)
		got := m["result"]
		switch exp := tc.expected.(type) {
		case int64:
			if g, ok := got.(int64); !ok || g != exp {
				t.Errorf("%s: expected %d, got %v (%T)", tc.expr, exp, got, got)
			}
		case float64:
			if g, ok := got.(float64); !ok {
				// might be int64 if result is whole number
				if g2, ok2 := got.(int64); ok2 {
					if float64(g2) != exp {
						t.Errorf("%s: expected %f, got %d", tc.expr, exp, g2)
					}
				} else {
					t.Errorf("%s: expected %f, got %v (%T)", tc.expr, exp, got, got)
				}
			} else if g != exp {
				t.Errorf("%s: expected %f, got %f", tc.expr, exp, g)
			}
		}
	}
}

func TestCalculate_Functions(t *testing.T) {
	def := calculateToolDef()
	cases := []struct {
		expr     string
		expected float64
	}{
		{"sqrt(16)", 4},
		{"abs(-5)", 5},
		{"round(3.7)", 4},
		{"floor(3.9)", 3},
		{"ceil(3.1)", 4},
		{"min(1, 2, 3)", 1},
		{"max(1, 2, 3)", 3},
		{"sin(0)", 0},
		{"cos(0)", 1},
		{"ln(1)", 0},
		{"log10(100)", 2},
		{"exp(0)", 1},
	}
	for _, tc := range cases {
		result := execTool(t, def.Tool, map[string]any{"expression": tc.expr})
		m := result.(map[string]any)
		got := toFloat(m["result"])
		if abs(got-tc.expected) > 1e-9 {
			t.Errorf("%s: expected %f, got %f", tc.expr, tc.expected, got)
		}
	}
}

func TestCalculate_Constants(t *testing.T) {
	def := calculateToolDef()

	// pi
	result := execTool(t, def.Tool, map[string]any{"expression": "pi"})
	m := result.(map[string]any)
	if v := toFloat(m["result"]); abs(v-3.141592653589793) > 1e-9 {
		t.Errorf("pi: expected ~3.14159, got %f", v)
	}

	// e
	result = execTool(t, def.Tool, map[string]any{"expression": "e"})
	m = result.(map[string]any)
	if v := toFloat(m["result"]); abs(v-2.718281828459045) > 1e-9 {
		t.Errorf("e: expected ~2.71828, got %f", v)
	}
}

func TestCalculate_ComplexExpression(t *testing.T) {
	def := calculateToolDef()
	// (2 + 3) * (4 - 1) ^ 2 = 5 * 9 = 45
	result := execTool(t, def.Tool, map[string]any{"expression": "(2 + 3) * (4 - 1)^2"})
	m := result.(map[string]any)
	if v, ok := m["result"].(int64); !ok || v != 45 {
		t.Errorf("expected 45, got %v", m["result"])
	}
}

func TestCalculate_Error(t *testing.T) {
	def := calculateToolDef()

	// Invalid expressions return error in result map (not Go error)
	invalidExprs := []string{
		"1 +",         // incomplete
		"1 / 0",       // division by zero
		"unknown_var", // unknown constant
		"foo(1)",      // unknown function
		"(1 + 2",      // unbalanced paren
	}
	for _, expr := range invalidExprs {
		result := execTool(t, def.Tool, map[string]any{"expression": expr})
		m := result.(map[string]any)
		if _, ok := m["error"]; !ok {
			t.Errorf("expression %q should return error in result, got: %v", expr, m)
		}
	}

	// Empty expression returns Go error
	_, err := def.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"expression": ""},
	)
	if err == nil {
		t.Error("empty expression should return error")
	}
}

func TestCalculate_DivisionByZero(t *testing.T) {
	def := calculateToolDef()
	_, err := def.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"expression": "1 / 0"},
	)
	// division by zero returns error in result map, not as Go error
	if err != nil {
		t.Errorf("expected nil error (error in result), got %v", err)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// ============================================================================
// random 工具测试
// ============================================================================

func TestRandom_Int(t *testing.T) {
	def := randomToolDef()
	result := execTool(t, def.Tool, map[string]any{
		"min":  1,
		"max":  10,
		"type": "int",
	})
	m := result.(map[string]any)
	v := toFloat(m["result"])
	if v < 1 || v > 10 {
		t.Errorf("random int out of range [1,10]: %v", v)
	}
}

func TestRandom_Float(t *testing.T) {
	def := randomToolDef()
	result := execTool(t, def.Tool, map[string]any{
		"min":  0,
		"max":  1,
		"type": "float",
	})
	m := result.(map[string]any)
	v := toFloat(m["result"])
	if v < 0 || v > 1 {
		t.Errorf("random float out of range [0,1]: %v", v)
	}
}

func TestRandom_Choices(t *testing.T) {
	def := randomToolDef()
	choices := []any{"apple", "banana", "cherry"}
	result := execTool(t, def.Tool, map[string]any{
		"choices": choices,
	})
	m := result.(map[string]any)
	pick, _ := m["result"].(string)
	found := false
	for _, c := range choices {
		if c == pick {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("random choice not in list: %q", pick)
	}
}

func TestRandom_Multiple(t *testing.T) {
	def := randomToolDef()
	result := execTool(t, def.Tool, map[string]any{
		"min":   1,
		"max":   100,
		"count": 5,
	})
	m := result.(map[string]any)
	results, ok := m["results"].([]any)
	if !ok {
		t.Fatalf("expected results array, got %T", m["results"])
	}
	if len(results) != 5 {
		t.Errorf("expected 5 results, got %d", len(results))
	}
}

// ============================================================================
// uuid 工具测试
// ============================================================================

func TestUUID_Single(t *testing.T) {
	def := uuidToolDef()
	result := execTool(t, def.Tool, map[string]any{})
	m := result.(map[string]any)
	u, ok := m["uuid"].(string)
	if !ok || u == "" {
		t.Fatalf("expected non-empty uuid, got %v", m["uuid"])
	}
	// UUID v4 format: xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx
	if len(u) != 36 {
		t.Errorf("uuid length: expected 36, got %d (%q)", len(u), u)
	}
	if u[14] != '4' {
		t.Errorf("expected version 4 at position 14, got %q in %q", u[14], u)
	}
}

func TestUUID_Multiple(t *testing.T) {
	def := uuidToolDef()
	result := execTool(t, def.Tool, map[string]any{"count": 3})
	m := result.(map[string]any)
	uuids, ok := m["uuids"].([]string)
	if !ok {
		t.Fatalf("expected uuids array, got %T", m["uuids"])
	}
	if len(uuids) != 3 {
		t.Errorf("expected 3 uuids, got %d", len(uuids))
	}
	// All should be unique
	seen := map[string]bool{}
	for _, u := range uuids {
		if seen[u] {
			t.Errorf("duplicate uuid: %q", u)
		}
		seen[u] = true
	}
}

func TestUUID_Uniqueness(t *testing.T) {
	// Generate 100 UUIDs and ensure all unique
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u, err := generateUUIDv4()
		if err != nil {
			t.Fatal(err)
		}
		if seen[u] {
			t.Fatalf("duplicate uuid after %d iterations: %q", i+1, u)
		}
		seen[u] = true
	}
}

// ============================================================================
// RegisterTools 集成测试
// ============================================================================

func TestRegisterTools_StaticCount(t *testing.T) {
	mgr := newTestToolMgr(t)
	err := RegisterTools(mgr, Config{
		TimezoneResolver: func(botID string) string { return "UTC" },
	})
	if err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	// 4 static tools + 1 hidden meta = 5 static entries
	// But hidden meta has scope __never__ so it won't show in tool list
	staticCount := mgr.StaticCount()
	// web_fetch, calculate, random, uuid, datetime_calc, list_files,
	// text_hash, text_encode, text_diff, text_stats, web_search, __common_tools_meta = 12
	if staticCount != 12 {
		t.Errorf("expected 12 static tools, got %d", staticCount)
	}
}

func TestRegisterTools_ProviderCount(t *testing.T) {
	mgr := newTestToolMgr(t)
	err := RegisterTools(mgr, Config{})
	if err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	if mgr.ProviderCount() != 1 {
		t.Errorf("expected 1 provider (now), got %d", mgr.ProviderCount())
	}
}

func TestRegisterTools_ResolveAll(t *testing.T) {
	mgr := newTestToolMgr(t)
	err := RegisterTools(mgr, Config{
		TimezoneResolver: func(botID string) string { return "UTC" },
	})
	if err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}

	tools, err := mgr.ResolveTools(context.Background(), &agenttools.ToolSessionContext{
		BotID:    "test-bot",
		ChatType: "private",
	})
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}

	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}

	expected := []string{"now", "web_fetch", "calculate", "random", "uuid"}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("tool %q not found in resolved tools", name)
		}
	}
}

// ============================================================================
// Config defaults 测试
// ============================================================================

func TestConfig_Defaults(t *testing.T) {
	cfg := Config{}.defaults()
	if cfg.HTTPTimeout != 30*time.Second {
		t.Errorf("default HTTPTimeout: got %v", cfg.HTTPTimeout)
	}
	if cfg.MaxFetchSize != 32768 {
		t.Errorf("default MaxFetchSize: got %d", cfg.MaxFetchSize)
	}
	if cfg.UserAgent == "" {
		t.Error("default UserAgent should not be empty")
	}
}

func TestConfig_CustomValues(t *testing.T) {
	cfg := Config{
		HTTPTimeout:  10 * time.Second,
		MaxFetchSize: 1024,
		UserAgent:    "TestBot/2.0",
	}.defaults()
	if cfg.HTTPTimeout != 10*time.Second {
		t.Errorf("HTTPTimeout not preserved: got %v", cfg.HTTPTimeout)
	}
	if cfg.MaxFetchSize != 1024 {
		t.Errorf("MaxFetchSize not preserved: got %d", cfg.MaxFetchSize)
	}
	if cfg.UserAgent != "TestBot/2.0" {
		t.Errorf("UserAgent not preserved: got %q", cfg.UserAgent)
	}
}

// ============================================================================
// ParseTimezone 测试
// ============================================================================

func TestParseTimezone_Valid(t *testing.T) {
	loc := ParseTimezone("Asia/Shanghai")
	if loc.String() != "Asia/Shanghai" {
		t.Errorf("expected Asia/Shanghai, got %q", loc.String())
	}
}

func TestParseTimezone_Invalid(t *testing.T) {
	loc := ParseTimezone("Invalid/Zone")
	if loc != time.UTC {
		t.Errorf("expected UTC for invalid zone, got %q", loc.String())
	}
}

// ============================================================================
// web_fetch 测试（需要网络连接，使用 -short 跳过）
// ============================================================================

func TestWebFetch_InvalidScheme(t *testing.T) {
	def := webFetchToolDef(Config{}.defaults())
	_, err := def.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{"url": "ftp://example.com"},
	)
	if err == nil {
		t.Error("expected error for ftp:// scheme")
	}
	if !strings.Contains(err.Error(), "http://") {
		t.Errorf("error should mention http requirement: %v", err)
	}
}

func TestWebFetch_MissingURL(t *testing.T) {
	def := webFetchToolDef(Config{}.defaults())
	_, err := def.Execute(
		&llm.ToolExecContext{Context: context.Background()},
		map[string]any{},
	)
	if err == nil {
		t.Error("expected error for missing url")
	}
}
