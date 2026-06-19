package tools

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// ToolRegistry Tests
// ============================================================================

func TestToolRegistry_RegisterAndGet(t *testing.T) {
	r := NewToolRegistry()

	tool := ToolDef{
		Category: "utility",
		Tool: llm.Tool{
			Name:        "test_tool",
			Description: "A test tool",
		},
	}

	if err := r.Register(tool); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	got, ok := r.Get("test_tool")
	if !ok {
		t.Fatal("expected tool to be found")
	}
	if got.Name != "test_tool" {
		t.Errorf("got name %q", got.Name)
	}
}

func TestToolRegistry_RegisterValidation(t *testing.T) {
	r := NewToolRegistry()

	// 空名称
	err := r.Register(ToolDef{
		Tool: llm.Tool{Name: "", Description: "desc"},
	})
	if err == nil {
		t.Error("expected error for empty name")
	}

	// 空描述
	err = r.Register(ToolDef{
		Tool: llm.Tool{Name: "x", Description: ""},
	})
	if err == nil {
		t.Error("expected error for empty description")
	}
}

func TestToolRegistry_Unregister(t *testing.T) {
	r := NewToolRegistry()
	r.Register(ToolDef{
		Tool: llm.Tool{Name: "removable", Description: "d"},
	})

	if r.StaticCount() != 1 {
		t.Fatalf("expected 1, got %d", r.StaticCount())
	}

	r.Unregister("removable")
	if r.StaticCount() != 0 {
		t.Errorf("after unregister: expected 0, got %d", r.StaticCount())
	}
}

// ============================================================================
// Resolve Tests (static + dynamic + scope filtering)
// ============================================================================

func TestToolRegistry_ResolveStatic(t *testing.T) {
	r := NewToolRegistry()
	r.Register(ToolDef{
		Tool: llm.Tool{Name: "a", Description: "Tool A"},
	})
	r.Register(ToolDef{
		Tool: llm.Tool{Name: "b", Description: "Tool B"},
	})

	sctx := &ToolSessionContext{}
	tools, err := r.Resolve(context.Background(), sctx)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	// 应按名称排序
	if tools[0].Name != "a" {
		t.Errorf("expected 'a' first, got %q", tools[0].Name)
	}
}

func TestToolRegistry_ResolveWithDynamic(t *testing.T) {
	r := NewToolRegistry()
	r.Register(ToolDef{
		Tool: llm.Tool{Name: "static_tool", Description: "Static"},
	})

	// 动态提供者
	r.AddProvider(ToolFunc(func(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
		return []llm.Tool{
			{Name: "dynamic_tool", Description: "Dynamic"},
		}, nil
	}))

	tools, _ := r.Resolve(context.Background(), &ToolSessionContext{})
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (1 static + 1 dynamic), got %d", len(tools))
	}
}

func TestToolRegistry_ResolveDedup(t *testing.T) {
	r := NewToolRegistry()
	r.Register(ToolDef{
		Tool: llm.Tool{Name: "shared", Description: "Static version"},
	})

	// 动态提供者也返回同名工具
	r.AddProvider(ToolFunc(func(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
		return []llm.Tool{
			{Name: "shared", Description: "Dynamic version"},
		}, nil
	}))

	tools, _ := r.Resolve(context.Background(), &ToolSessionContext{})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool (dedup), got %d", len(tools))
	}
	// 静态工具优先
	if tools[0].Description != "Static version" {
		t.Errorf("expected static version, got %q", tools[0].Description)
	}
}

func TestToolRegistry_ResolveScopeFiltering(t *testing.T) {
	r := NewToolRegistry()
	r.Register(ToolDef{
		Tool:   llm.Tool{Name: "private_only", Description: "Private"},
		Scopes: []string{"private"},
	})
	r.Register(ToolDef{
		Tool:   llm.Tool{Name: "group_only", Description: "Group"},
		Scopes: []string{"group"},
	})
	r.Register(ToolDef{
		Tool:   llm.Tool{Name: "no_scope", Description: "Any"},
		Scopes: []string{}, // 无限制
	})

	// 私聊场景
	tools, _ := r.Resolve(context.Background(), &ToolSessionContext{ChatType: "private"})
	if len(tools) != 2 {
		t.Fatalf("private: expected 2 tools (private_only + no_scope), got %d", len(tools))
	}

	// 群聊场景
	tools, _ = r.Resolve(context.Background(), &ToolSessionContext{ChatType: "group"})
	if len(tools) != 2 {
		t.Fatalf("group: expected 2 tools (group_only + no_scope), got %d", len(tools))
	}

	// SubAgent 场景
	tools, _ = r.Resolve(context.Background(), &ToolSessionContext{IsSubagent: true})
	if len(tools) != 1 {
		t.Fatalf("subagent: expected 1 tool (no_scope only), got %d", len(tools))
	}
	if tools[0].Name != "no_scope" {
		t.Errorf("expected 'no_scope', got %q", tools[0].Name)
	}
}

func TestToolRegistry_ResolveDynamicError(t *testing.T) {
	r := NewToolRegistry()
	r.Register(ToolDef{
		Tool: llm.Tool{Name: "static", Description: "S"},
	})

	r.AddProvider(ToolFunc(func(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
		return nil, context.DeadlineExceeded
	}))

	// 提供者出错不应中断
	tools, err := r.Resolve(context.Background(), &ToolSessionContext{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(tools) != 1 {
		t.Errorf("expected 1 tool (static only), got %d", len(tools))
	}
}

// ============================================================================
// ToolManager Tests
// ============================================================================

func TestToolManager_RegisterAndPrompt(t *testing.T) {
	promptReg := prompt.NewRegistry()
	mgr := NewToolManager(promptReg, nil)

	tool := ToolDef{
		Category: "test",
		Tool: llm.Tool{
			Name:        "my_tool",
			Description: "Does something useful",
		},
		PromptSection: &ToolPromptSection{
			Name:    "my_tool",
			Order:   310,
			Content: "Use my_tool when needed.",
			Enabled: true,
		},
	}

	if err := mgr.Register(tool); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	// 验证工具已注册
	if mgr.StaticCount() != 1 {
		t.Errorf("expected 1 tool, got %d", mgr.StaticCount())
	}

	// 验证自定义提示词段落已注册
	sections := promptReg.List()
	found := false
	for _, s := range sections {
		if s.Name == "tool_my_tool" {
			found = true
			if s.Content != "Use my_tool when needed." {
				t.Errorf("prompt content mismatch: %q", s.Content)
			}
		}
	}
	if !found {
		t.Error("expected 'tool_my_tool' section in prompt registry")
	}

	// 自动描述段落也应该注册
	foundDesc := false
	for _, s := range sections {
		if s.Name == "tool_my_tool" && s.Order == 310 {
			foundDesc = true
		}
	}
	if !foundDesc {
		t.Error("expected auto-description section")
	}
}

func TestToolManager_UnregisterRemovesPrompt(t *testing.T) {
	promptReg := prompt.NewRegistry()
	mgr := NewToolManager(promptReg, nil)

	mgr.Register(ToolDef{
		Tool:         llm.Tool{Name: "temp_tool", Description: "Temporary"},
		PromptSection: &ToolPromptSection{Name: "temp_tool", Order: 310, Content: "temp", Enabled: true},
	})

	// 确认注册了
	sections := promptReg.List()
	if len(sections) < 2 {
		t.Fatalf("expected >=2 sections, got %d", len(sections))
	}

	mgr.Unregister("temp_tool")

	// 确认提示词段落被移除
	for _, s := range promptReg.List() {
		if s.Name == "tool_temp_tool" {
			t.Errorf("section 'tool_temp_tool' should have been removed")
		}
	}
}

func TestToolManager_SetHeaderAndRules(t *testing.T) {
	promptReg := prompt.NewRegistry()
	mgr := NewToolManager(promptReg, nil)

	mgr.SetHeaderSection(DefaultToolHeaderSection([]string{"tool_a", "tool_b"}))
	mgr.SetRulesSection(DefaultToolRulesSection())

	// 验证段落存在
	sections := promptReg.List()
	headerFound := false
	rulesFound := false
	for _, s := range sections {
		if s.Name == "tool__header" {
			headerFound = true
		}
		if s.Name == "tool__rules" {
			rulesFound = true
		}
	}
	if !headerFound {
		t.Error("header section not found")
	}
	if !rulesFound {
		t.Error("rules section not found")
	}
}

func TestToolManager_ResolveForEnvelope(t *testing.T) {
	promptReg := prompt.NewRegistry()
	mgr := NewToolManager(promptReg, nil)

	mgr.Register(ToolDef{
		Tool:   llm.Tool{Name: "env_tool", Description: "Env"},
		Scopes: []string{}, // 全场景
	})

	env := core.NewEnvelope(core.Message{
		ID:       "msg1",
		BotID:    "bot1",
		Channel:  "ch1",
		ChatType: "private",
		UserID:   "user1",
	})

	tools, err := mgr.ResolveForEnvelope(context.Background(), env)
	if err != nil {
		t.Fatalf("ResolveForEnvelope failed: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "env_tool" {
		t.Errorf("expected 'env_tool', got %q", tools[0].Name)
	}
}

// ============================================================================
// ToolPromptManager Tests
// ============================================================================

func TestToolPromptManager_RegisterAndUnregister(t *testing.T) {
	reg := prompt.NewRegistry()
	mgr := NewToolPromptManager(reg, "tool_")

	mgr.RegisterToolPrompt(&ToolPromptSection{
		Name:    "x",
		Order:   310,
		Content: "Tool X instructions",
		Enabled: true,
	})

	if reg.Len() != 1 {
		t.Fatalf("expected 1 section, got %d", reg.Len())
	}

	mgr.UnregisterAll()
	if reg.Len() != 0 {
		t.Errorf("after UnregisterAll: expected 0, got %d", reg.Len())
	}
}

func TestToolPromptManager_NilSkipped(t *testing.T) {
	reg := prompt.NewRegistry()
	mgr := NewToolPromptManager(reg, "tool_")

	// nil section should be skipped
	mgr.RegisterToolPrompt(nil)
	if reg.Len() != 0 {
		t.Error("nil section should not register")
	}

	// empty content should be skipped
	mgr.RegisterToolPrompt(&ToolPromptSection{
		Name:    "empty",
		Content: "   ",
		Enabled: true,
	})
	if reg.Len() != 0 {
		t.Error("empty content section should not register")
	}
}

// ============================================================================
// Prompt Section Builders Tests
// ============================================================================

func TestDefaultToolHeaderSection(t *testing.T) {
	s := DefaultToolHeaderSection([]string{"a", "b", "c"})
	if s == nil {
		t.Fatal("expected non-nil section")
	}
	if s.Order != 300 {
		t.Errorf("expected order 300, got %d", s.Order)
	}
}

func TestDefaultToolHeaderSection_Empty(t *testing.T) {
	s := DefaultToolHeaderSection(nil)
	if s != nil {
		t.Error("expected nil for empty tool list")
	}
}

func TestDefaultToolRulesSection(t *testing.T) {
	s := DefaultToolRulesSection()
	if s == nil {
		t.Fatal("expected non-nil section")
	}
	if s.Order != 301 {
		t.Errorf("expected order 301, got %d", s.Order)
	}
}

func TestBuildToolDescriptionSection(t *testing.T) {
	def := &ToolDef{
		Category: "test",
		Scopes:   []string{"private"},
		Tool: llm.Tool{
			Name:        "desc_tool",
			Description: "A tool with description",
		},
		RequireApproval: true,
	}

	s := BuildToolDescriptionSection(def)
	if s == nil {
		t.Fatal("expected non-nil section")
	}
	if s.Order != 310 {
		t.Errorf("expected order 310, got %d", s.Order)
	}
}

// ============================================================================
// Built-in Tools Tests
// ============================================================================

func TestBuiltin_CurrentTimeTool(t *testing.T) {
	def := CurrentTimeTool()
	if def.Name != "current_time" {
		t.Errorf("expected 'current_time', got %q", def.Name)
	}
	if def.Tool.Execute == nil {
		t.Error("Execute function should not be nil")
	}

	// 执行测试
	result, err := def.Tool.Execute(&llm.ToolExecContext{}, nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatal("expected map result")
	}
	if _, ok := m["time"]; !ok {
		t.Error("expected 'time' key in result")
	}
}

func TestBuiltin_EchoTool(t *testing.T) {
	def := EchoTool()
	if def.Name != "echo" {
		t.Errorf("expected 'echo', got %q", def.Name)
	}

	// 执行测试
	result, err := def.Tool.Execute(&llm.ToolExecContext{}, map[string]any{
		"message": "hello world",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatal("expected map result")
	}
	if m["echo"] != "hello world" {
		t.Errorf("expected 'hello world', got %v", m["echo"])
	}
	if m["length"] != 11 {
		t.Errorf("expected length 11, got %v", m["length"])
	}
}

func TestBuiltin_EchoTool_InvalidInput(t *testing.T) {
	def := EchoTool()
	_, err := def.Tool.Execute(&llm.ToolExecContext{}, "not-a-map")
	if err == nil {
		t.Error("expected error for invalid input")
	}
}

// ============================================================================
// SessionContext Tests
// ============================================================================

func TestSessionContext_GetString(t *testing.T) {
	ctx := &ToolSessionContext{
		Extra: map[string]any{
			"key1": "value1",
			"key2": 123,
		},
	}
	if ctx.GetString("key1") != "value1" {
		t.Errorf("expected 'value1', got %q", ctx.GetString("key1"))
	}
	if ctx.GetString("key2") != "" {
		t.Errorf("expected empty for non-string, got %q", ctx.GetString("key2"))
	}
	if ctx.GetString("nonexistent") != "" {
		t.Error("expected empty for nonexistent key")
	}
}

func TestSessionContext_GetBool(t *testing.T) {
	ctx := &ToolSessionContext{
		Extra: map[string]any{
			"flag": true,
			"num":  1,
		},
	}
	if !ctx.GetBool("flag") {
		t.Error("expected true for 'flag'")
	}
	if ctx.GetBool("num") {
		t.Error("expected false for non-bool value")
	}
	if ctx.GetBool("nonexistent") {
		t.Error("expected false for nonexistent key")
	}
}
