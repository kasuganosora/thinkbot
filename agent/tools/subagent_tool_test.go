package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/llm"
)

// mockProvider 是用于测试的 LLM Provider 模拟器。
type mockProvider struct {
	mu         sync.Mutex
	responses  []string // 按调用顺序依次返回
	calls      int
	lastSystem string
}

func (p *mockProvider) Name() string { return "mock" }

func (p *mockProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	p.lastSystem = params.System

	idx := p.calls - 1
	resp := "default response"
	if idx < len(p.responses) {
		resp = p.responses[idx]
	}

	return &llm.GenerateResult{
		Text:         resp,
		FinishReason: llm.FinishReasonStop,
	}, nil
}

func (p *mockProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

func (p *mockProvider) getCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *mockProvider) getLastSystem() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSystem
}

func newMockProvider(responses ...string) *mockProvider {
	return &mockProvider{responses: responses}
}

// errorProvider always returns an error.
type errorProvider struct{}

func (p *errorProvider) Name() string { return "error" }
func (p *errorProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	return nil, context.DeadlineExceeded
}
func (p *errorProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, context.DeadlineExceeded
}

// ============================================================================

func TestSubAgentManager_Delegate(t *testing.T) {
	provider := newMockProvider("delegation result")
	mgr := NewSubAgentManager(provider, "test-model")

	result, err := mgr.Delegate(context.Background(), "你是翻译专家", "翻译 hello")
	if err != nil {
		t.Fatalf("Delegate failed: %v", err)
	}
	if result != "delegation result" {
		t.Errorf("expected 'delegation result', got %q", result)
	}
	if provider.getCalls() != 1 {
		t.Errorf("expected 1 provider call, got %d", provider.getCalls())
	}
	if provider.getLastSystem() != "你是翻译专家" {
		t.Errorf("expected system prompt '你是翻译专家', got %q", provider.getLastSystem())
	}
}

func TestSubAgentManager_DelegateEmptySystemPrompt(t *testing.T) {
	provider := newMockProvider("ok")
	mgr := NewSubAgentManager(provider, "test-model")

	result, err := mgr.Delegate(context.Background(), "", "hi")
	if err != nil {
		t.Fatalf("Delegate failed: %v", err)
	}
	if result != "ok" {
		t.Errorf("expected 'ok', got %q", result)
	}
	if provider.getLastSystem() != "" {
		t.Errorf("expected empty system prompt, got %q", provider.getLastSystem())
	}
}

func TestSubAgentManager_SpawnChatClose(t *testing.T) {
	provider := newMockProvider("reply1", "reply2")
	mgr := NewSubAgentManager(provider, "test-model")

	// Spawn
	id, err := mgr.Spawn("你是助手", "test-bot")
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty id")
	}

	// Chat round 1
	reply, turns, err := mgr.Chat(context.Background(), id, "hello")
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if reply != "reply1" {
		t.Errorf("expected 'reply1', got %q", reply)
	}
	if turns != 1 {
		t.Errorf("expected 1 turn, got %d", turns)
	}

	// Chat round 2 (context maintained)
	reply2, turns2, err := mgr.Chat(context.Background(), id, "again")
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if reply2 != "reply2" {
		t.Errorf("expected 'reply2', got %q", reply2)
	}
	if turns2 != 2 {
		t.Errorf("expected 2 turns, got %d", turns2)
	}

	// Close
	if err := mgr.Close(id); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Chat after close should fail
	_, _, err = mgr.Chat(context.Background(), id, "test")
	if err == nil {
		t.Error("expected error after close")
	}
}

func TestSubAgentManager_List(t *testing.T) {
	provider := newMockProvider("ok")
	mgr := NewSubAgentManager(provider, "test-model")

	id1, _ := mgr.Spawn("agent1", "first")
	id2, _ := mgr.Spawn("agent2", "second")

	list := mgr.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 subagents, got %d", len(list))
	}

	ids := map[string]bool{}
	for _, info := range list {
		ids[info.ID] = true
		if info.ID == id1 && info.Name != "first" {
			t.Errorf("expected name 'first' for %s, got %q", id1, info.Name)
		}
	}
	if !ids[id1] || !ids[id2] {
		t.Errorf("expected ids %s and %s in list", id1, id2)
	}

	mgr.Close(id1)
	list = mgr.List()
	if len(list) != 1 {
		t.Errorf("expected 1 subagent after close, got %d", len(list))
	}

	mgr.CloseAll()
	list = mgr.List()
	if len(list) != 0 {
		t.Errorf("expected 0 subagents after CloseAll, got %d", len(list))
	}
}

func TestSubAgentManager_ChatNotFound(t *testing.T) {
	provider := newMockProvider("ok")
	mgr := NewSubAgentManager(provider, "test-model")

	_, _, err := mgr.Chat(context.Background(), "nonexistent", "msg")
	if err == nil {
		t.Error("expected error for nonexistent subagent")
	}
}

func TestSubAgentManager_CloseNotFound(t *testing.T) {
	provider := newMockProvider("ok")
	mgr := NewSubAgentManager(provider, "test-model")

	err := mgr.Close("nonexistent")
	if err == nil {
		t.Error("expected error for closing nonexistent subagent")
	}
}

func TestSubAgentManager_ContextIsolation(t *testing.T) {
	// SubAgent 的上下文应与主 Agent 隔离。
	// 验证：两个独立的 Delegate 调用不应共享上下文。
	provider := newMockProvider("result1", "result2")
	mgr := NewSubAgentManager(provider, "test-model")

	r1, _ := mgr.Delegate(context.Background(), "", "task1")
	r2, _ := mgr.Delegate(context.Background(), "", "task2")

	if r1 != "result1" || r2 != "result2" {
		t.Errorf("context not isolated: r1=%q r2=%q", r1, r2)
	}

	// 每次 Delegate 只应调用 provider 1 次
	if provider.getCalls() != 2 {
		t.Errorf("expected 2 provider calls, got %d", provider.getCalls())
	}
}

func TestSubAgentManager_DelegateError(t *testing.T) {
	mgr := NewSubAgentManager(&errorProvider{}, "test-model")

	_, err := mgr.Delegate(context.Background(), "", "fail")
	if err == nil {
		t.Error("expected error from errorProvider")
	}
}

func TestSubAgentManager_MultiplePersistentAgents(t *testing.T) {
	provider := newMockProvider("a1r1", "a2r1", "a1r2")
	mgr := NewSubAgentManager(provider, "test-model")

	id1, _ := mgr.Spawn("agent1", "a1")
	id2, _ := mgr.Spawn("agent2", "a2")

	// Interleave chats
	r1, _, _ := mgr.Chat(context.Background(), id1, "msg1")
	r2, _, _ := mgr.Chat(context.Background(), id2, "msg2")
	r3, _, _ := mgr.Chat(context.Background(), id1, "msg3")

	if r1 != "a1r1" || r2 != "a2r1" || r3 != "a1r2" {
		t.Errorf("unexpected interleaved results: r1=%q r2=%q r3=%q", r1, r2, r3)
	}

	// Verify turns
	list := mgr.List()
	turnsByID := map[string]int{}
	for _, info := range list {
		turnsByID[info.ID] = info.Turns
	}
	if turnsByID[id1] != 2 {
		t.Errorf("expected 2 turns for %s, got %d", id1, turnsByID[id1])
	}
	if turnsByID[id2] != 1 {
		t.Errorf("expected 1 turn for %s, got %d", id2, turnsByID[id2])
	}

	mgr.CloseAll()
}

// ============================================================================
// Tool Definition Tests
// ============================================================================

func TestDelegateToolDef(t *testing.T) {
	provider := newMockProvider("tool result")
	mgr := NewSubAgentManager(provider, "test-model")
	def := DelegateToolDef(mgr)

	if def.Name != "delegate" {
		t.Errorf("expected name 'delegate', got %q", def.Name)
	}
	if def.Category != "subagent" {
		t.Errorf("expected category 'subagent', got %q", def.Category)
	}
	// Scopes should NOT include "subagent" (prevent recursion)
	for _, s := range def.Scopes {
		if s == "subagent" {
			t.Error("delegate tool should not be available in subagent scope")
		}
	}

	// Test execution
	result, err := def.Execute(&llm.ToolExecContext{
		Context:    context.Background(),
		ToolCallID: "tc1",
		ToolName:   "delegate",
	}, map[string]any{
		"task":          "do something",
		"system_prompt": "you are a helper",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	if m["success"] != true {
		t.Errorf("expected success=true, got %v", m["success"])
	}
	if m["result"] != "tool result" {
		t.Errorf("expected result 'tool result', got %v", m["result"])
	}
}

func TestDelegateToolDef_MissingTask(t *testing.T) {
	provider := newMockProvider("ok")
	mgr := NewSubAgentManager(provider, "test-model")
	def := DelegateToolDef(mgr)

	_, err := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{})
	if err == nil {
		t.Error("expected error for missing task")
	}
}

func TestDelegateToolDef_LLMError(t *testing.T) {
	// 当 LLM 出错时，工具应返回 success=false 而非 error
	mgr := NewSubAgentManager(&errorProvider{}, "test-model")
	def := DelegateToolDef(mgr)

	result, err := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{
		"task": "fail",
	})
	if err != nil {
		t.Fatalf("Execute should not return error: %v", err)
	}

	m := result.(map[string]any)
	if m["success"] != false {
		t.Errorf("expected success=false on LLM error")
	}
}

func TestSpawnSubAgentToolDef(t *testing.T) {
	provider := newMockProvider("ok")
	mgr := NewSubAgentManager(provider, "test-model")
	def := SpawnSubAgentToolDef(mgr)

	if def.Name != "spawn_subagent" {
		t.Errorf("expected name 'spawn_subagent', got %q", def.Name)
	}

	result, err := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{
		"system_prompt": "你是专家",
		"name":          "expert",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	m := result.(map[string]any)
	if m["success"] != true {
		t.Errorf("expected success=true")
	}
	id, _ := m["id"].(string)
	if id == "" {
		t.Error("expected non-empty id")
	}
	if m["name"] != "expert" {
		t.Errorf("expected name 'expert', got %v", m["name"])
	}

	mgr.CloseAll()
}

func TestChatSubAgentToolDef(t *testing.T) {
	provider := newMockProvider("chat reply")
	mgr := NewSubAgentManager(provider, "test-model")

	id, _ := mgr.Spawn("assistant", "test")
	def := ChatSubAgentToolDef(mgr)

	result, err := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{
		"id":      id,
		"message": "hello",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	m := result.(map[string]any)
	if m["success"] != true {
		t.Errorf("expected success=true")
	}
	if m["reply"] != "chat reply" {
		t.Errorf("expected 'chat reply', got %v", m["reply"])
	}

	mgr.CloseAll()
}

func TestChatSubAgentToolDef_NotFound(t *testing.T) {
	provider := newMockProvider("ok")
	mgr := NewSubAgentManager(provider, "test-model")
	def := ChatSubAgentToolDef(mgr)

	result, _ := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{
		"id":      "nonexistent",
		"message": "hi",
	})

	m := result.(map[string]any)
	if m["success"] != false {
		t.Error("expected success=false for nonexistent subagent")
	}
}

func TestCloseSubAgentToolDef(t *testing.T) {
	provider := newMockProvider("ok")
	mgr := NewSubAgentManager(provider, "test-model")

	id, _ := mgr.Spawn("assistant", "")
	def := CloseSubAgentToolDef(mgr)

	result, err := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{
		"id": id,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	m := result.(map[string]any)
	if m["success"] != true {
		t.Errorf("expected success=true")
	}

	// Verify it's closed
	list := mgr.List()
	if len(list) != 0 {
		t.Errorf("expected 0 subagents after close, got %d", len(list))
	}
}

func TestListSubAgentsToolDef(t *testing.T) {
	provider := newMockProvider("ok")
	mgr := NewSubAgentManager(provider, "test-model")
	def := ListSubAgentsToolDef(mgr)

	// Empty list
	result, err := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	m := result.(map[string]any)
	if m["count"] != 0 {
		t.Errorf("expected count 0, got %v", m["count"])
	}

	// With subagents
	mgr.Spawn("a1", "first")
	mgr.Spawn("a2", "second")

	result, _ = def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{})
	m = result.(map[string]any)
	if m["count"] != 2 {
		t.Errorf("expected count 2, got %v", m["count"])
	}

	mgr.CloseAll()
}

func TestRegisterSubAgentTools(t *testing.T) {
	provider := newMockProvider("ok")
	saMgr := NewSubAgentManager(provider, "test-model")
	promptReg := prompt.NewRegistry()
	toolMgr := NewToolManager(promptReg, nil)

	if err := RegisterSubAgentTools(toolMgr, saMgr); err != nil {
		t.Fatalf("RegisterSubAgentTools failed: %v", err)
	}

	if toolMgr.StaticCount() < 5 {
		t.Errorf("expected at least 5 tools, got %d", toolMgr.StaticCount())
	}

	// Verify tools are resolvable in non-subagent scope
	sctx := &ToolSessionContext{
		ChatType:   "group",
		IsSubagent: false,
	}
	tools, err := toolMgr.ResolveTools(context.Background(), sctx)
	if err != nil {
		t.Fatalf("ResolveTools failed: %v", err)
	}

	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	for _, expected := range []string{"delegate", "spawn_subagent", "chat_subagent", "close_subagent", "list_subagents"} {
		if !names[expected] {
			t.Errorf("expected tool %q in resolved tools", expected)
		}
	}
}

func TestSubAgentToolsNotAvailableInSubagentScope(t *testing.T) {
	provider := newMockProvider("ok")
	saMgr := NewSubAgentManager(provider, "test-model")
	promptReg := prompt.NewRegistry()
	toolMgr := NewToolManager(promptReg, nil)

	RegisterSubAgentTools(toolMgr, saMgr)

	// In subagent scope, subagent tools should NOT be available (prevent recursion)
	sctx := &ToolSessionContext{
		ChatType:   "private",
		IsSubagent: true,
	}
	tools, _ := toolMgr.ResolveTools(context.Background(), sctx)

	for _, tool := range tools {
		switch tool.Name {
		case "delegate", "spawn_subagent", "chat_subagent", "close_subagent", "list_subagents":
			t.Errorf("subagent tool %q should not be available in subagent scope", tool.Name)
		}
	}
}
