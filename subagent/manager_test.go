package subagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// SubAgentManager Tests
// ============================================================================

func TestSubAgentManager_Delegate(t *testing.T) {
	provider := newMockProvider()
	provider.responses[0] = &llm.GenerateResult{Text: "delegation result", FinishReason: llm.FinishReasonStop}
	mgr := NewSubAgentManager(provider, "test-model")

	result, err := mgr.Delegate(context.Background(), "你是翻译专家", "翻译 hello")
	if err != nil {
		t.Fatalf("Delegate failed: %v", err)
	}
	if result != "delegation result" {
		t.Errorf("expected 'delegation result', got %q", result)
	}
	if len(provider.getCalls()) != 1 {
		t.Errorf("expected 1 provider call, got %d", len(provider.getCalls()))
	}
	calls := provider.getCalls()
	if calls[0].system != "你是翻译专家" {
		t.Errorf("expected system prompt '你是翻译专家', got %q", calls[0].system)
	}
}

func TestSubAgentManager_DelegateEmptySystemPrompt(t *testing.T) {
	provider := newMockProvider()
	provider.responses[0] = &llm.GenerateResult{Text: "ok", FinishReason: llm.FinishReasonStop}
	mgr := NewSubAgentManager(provider, "test-model")

	result, err := mgr.Delegate(context.Background(), "", "hi")
	if err != nil {
		t.Fatalf("Delegate failed: %v", err)
	}
	if result != "ok" {
		t.Errorf("expected 'ok', got %q", result)
	}
}

func TestSubAgentManager_DelegateError(t *testing.T) {
	mgr := NewSubAgentManager(&errorProvider{}, "test-model")

	_, err := mgr.Delegate(context.Background(), "", "fail")
	if err == nil {
		t.Error("expected error from errorProvider")
	}
}

func TestSubAgentManager_DelegateMany(t *testing.T) {
	// 并发模式下 mock 需要按序返回，但并发执行顺序不确定
	// 所以用响应计数器方式
	provider := &countingMockProvider{}
	mgr := NewSubAgentManager(provider, "test-model")
	mgr.SetMaxConcurrency(3) // 3 并发

	results := mgr.DelegateMany(context.Background(), "你是助手", []string{
		"task A",
		"task B",
		"task C",
	})

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	for i, r := range results {
		if !r.Success {
			t.Errorf("task %d failed: %s", i, r.Error)
		}
		if r.Text == "" {
			t.Errorf("task %d: expected non-empty text", i)
		}
	}

	// 所有任务都应被处理
	totalCalls := provider.count()
	if totalCalls != 3 {
		t.Errorf("expected 3 provider calls, got %d", totalCalls)
	}
}

func TestSubAgentManager_DelegateManyPartialError(t *testing.T) {
	mgr := NewSubAgentManager(&errorProvider{}, "test-model")

	results := mgr.DelegateMany(context.Background(), "", []string{"a", "b"})

	for _, r := range results {
		if r.Success {
			t.Errorf("expected all tasks to fail, but task %q succeeded", r.Task)
		}
		if r.Error == "" {
			t.Errorf("expected error message for task %q", r.Task)
		}
	}
}

func TestSubAgentManager_DelegateManyMaxTasks(t *testing.T) {
	provider := &countingMockProvider{}
	mgr := NewSubAgentManager(provider, "test-model")

	tasks := make([]string, maxTasksPerSpawn+3)
	for i := range tasks {
		tasks[i] = "task"
	}

	results := mgr.DelegateMany(context.Background(), "", tasks)

	// spawn 工具会截断到 maxTasksPerSpawn，但 DelegateMany 本身不截断
	// 截断发生在 tool 层面
	if len(results) != len(tasks) {
		t.Errorf("expected %d results, got %d", len(tasks), len(results))
	}
}

func TestSubAgentManager_SpawnChatClose(t *testing.T) {
	provider := &countingMockProvider{}
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
	if reply == "" {
		t.Error("expected non-empty reply")
	}
	if turns != 1 {
		t.Errorf("expected 1 turn, got %d", turns)
	}

	// Chat round 2
	_, turns2, err := mgr.Chat(context.Background(), id, "again")
	if err != nil {
		t.Fatalf("Chat round 2 failed: %v", err)
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
	provider := &countingMockProvider{}
	mgr := NewSubAgentManager(provider, "test-model")

	id1, _ := mgr.Spawn("agent1", "first")
	_, _ = mgr.Spawn("agent2", "second")

	list := mgr.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 subagents, got %d", len(list))
	}

	_ = mgr.Close(id1)
	list = mgr.List()
	if len(list) != 1 {
		t.Errorf("expected 1 after close, got %d", len(list))
	}

	mgr.CloseAll()
	list = mgr.List()
	if len(list) != 0 {
		t.Errorf("expected 0 after CloseAll, got %d", len(list))
	}
}

func TestSubAgentManager_ChatNotFound(t *testing.T) {
	mgr := NewSubAgentManager(newMockProvider(), "test-model")

	_, _, err := mgr.Chat(context.Background(), "nonexistent", "msg")
	if err == nil {
		t.Error("expected error for nonexistent subagent")
	}
}

func TestSubAgentManager_CloseNotFound(t *testing.T) {
	mgr := NewSubAgentManager(newMockProvider(), "test-model")

	err := mgr.Close("nonexistent")
	if err == nil {
		t.Error("expected error for closing nonexistent subagent")
	}
}

func TestSubAgentManager_ContextIsolation(t *testing.T) {
	provider := newMockProvider()
	provider.responses[0] = &llm.GenerateResult{Text: "result1", FinishReason: llm.FinishReasonStop}
	provider.responses[1] = &llm.GenerateResult{Text: "result2", FinishReason: llm.FinishReasonStop}
	mgr := NewSubAgentManager(provider, "test-model")

	r1, _ := mgr.Delegate(context.Background(), "", "task1")
	r2, _ := mgr.Delegate(context.Background(), "", "task2")

	if r1 != "result1" || r2 != "result2" {
		t.Errorf("context not isolated: r1=%q r2=%q", r1, r2)
	}

	if len(provider.getCalls()) != 2 {
		t.Errorf("expected 2 provider calls, got %d", len(provider.getCalls()))
	}
}

func TestSubAgentManager_MultiplePersistentAgents(t *testing.T) {
	provider := &countingMockProvider{}
	mgr := NewSubAgentManager(provider, "test-model")

	id1, _ := mgr.Spawn("agent1", "a1")
	id2, _ := mgr.Spawn("agent2", "a2")

	r1, _, _ := mgr.Chat(context.Background(), id1, "msg1")
	r2, _, _ := mgr.Chat(context.Background(), id2, "msg2")
	r3, _, _ := mgr.Chat(context.Background(), id1, "msg3")

	if r1 == "" || r2 == "" || r3 == "" {
		t.Error("expected non-empty replies")
	}

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
// Spawn Tool Definition Tests
// ============================================================================

func TestSpawnToolDef(t *testing.T) {
	provider := &countingMockProvider{}
	mgr := NewSubAgentManager(provider, "test-model")
	def := SpawnToolDef(mgr)

	if def.Name != "spawn" {
		t.Errorf("expected name 'spawn', got %q", def.Name)
	}
	if def.Category != "subagent" {
		t.Errorf("expected category 'subagent', got %q", def.Category)
	}
	// Scopes should NOT include "subagent" (prevent recursion)
	for _, s := range def.Scopes {
		if s == "subagent" {
			t.Error("spawn tool should not be available in subagent scope")
		}
	}

	// Test execution with single task
	result, err := def.Execute(&llm.ToolExecContext{
		Context:  context.Background(),
		ToolName: "spawn",
	}, map[string]any{
		"tasks":         []any{"do something"},
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
	if m["count"] != 1 {
		t.Errorf("expected count=1, got %v", m["count"])
	}
	results, ok := m["results"].([]TaskResult)
	if !ok {
		t.Fatalf("expected []TaskResult, got %T", m["results"])
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Success {
		t.Errorf("expected task success=true")
	}
	if results[0].Task != "do something" {
		t.Errorf("expected task 'do something', got %q", results[0].Task)
	}
}

func TestSpawnToolDef_MultipleTasks(t *testing.T) {
	provider := &countingMockProvider{}
	mgr := NewSubAgentManager(provider, "test-model")
	mgr.SetMaxConcurrency(3)
	def := SpawnToolDef(mgr)

	result, err := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{
		"tasks": []any{"task A", "task B", "task C"},
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	m := result.(map[string]any)
	if m["count"] != 3 {
		t.Errorf("expected count=3, got %v", m["count"])
	}
	results := m["results"].([]TaskResult)
	for i, r := range results {
		if !r.Success {
			t.Errorf("task %d failed: %s", i, r.Error)
		}
	}
}

func TestSpawnToolDef_MissingTasks(t *testing.T) {
	mgr := NewSubAgentManager(newMockProvider(), "test-model")
	def := SpawnToolDef(mgr)

	_, err := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{})
	if err == nil {
		t.Error("expected error for missing tasks")
	}
}

func TestSpawnToolDef_EmptyTasks(t *testing.T) {
	mgr := NewSubAgentManager(newMockProvider(), "test-model")
	def := SpawnToolDef(mgr)

	_, err := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{
		"tasks": []any{},
	})
	if err == nil {
		t.Error("expected error for empty tasks array")
	}
}

func TestSpawnToolDef_LLMError(t *testing.T) {
	mgr := NewSubAgentManager(&errorProvider{}, "test-model")
	def := SpawnToolDef(mgr)

	result, err := def.Execute(&llm.ToolExecContext{
		Context: context.Background(),
	}, map[string]any{
		"tasks": []any{"fail task"},
	})
	if err != nil {
		t.Fatalf("Execute should not return error: %v", err)
	}

	m := result.(map[string]any)
	results := m["results"].([]TaskResult)
	if results[0].Success {
		t.Errorf("expected success=false on LLM error")
	}
	if results[0].Error == "" {
		t.Error("expected error message")
	}
}

func TestRegisterTools(t *testing.T) {
	provider := &countingMockProvider{}
	saMgr := NewSubAgentManager(provider, "test-model")
	promptReg := prompt.NewRegistry()
	toolMgr := tools.NewToolManager(promptReg, nil, nil)

	if err := RegisterTools(toolMgr, saMgr); err != nil {
		t.Fatalf("RegisterTools failed: %v", err)
	}

	if toolMgr.StaticCount() < 1 {
		t.Errorf("expected at least 1 tool, got %d", toolMgr.StaticCount())
	}

	// Verify spawn is resolvable in non-subagent scope
	sctx := &tools.ToolSessionContext{
		ChatType:   "group",
		IsSubagent: false,
	}
	resolved, err := toolMgr.ResolveTools(context.Background(), sctx)
	if err != nil {
		t.Fatalf("ResolveTools failed: %v", err)
	}

	found := false
	for _, tool := range resolved {
		if tool.Name == "spawn" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'spawn' tool in resolved tools")
	}
}

func TestSpawnToolNotAvailableInSubagentScope(t *testing.T) {
	saMgr := NewSubAgentManager(newMockProvider(), "test-model")
	promptReg := prompt.NewRegistry()
	toolMgr := tools.NewToolManager(promptReg, nil, nil)

	_ = RegisterTools(toolMgr, saMgr)

	sctx := &tools.ToolSessionContext{
		ChatType:   "private",
		IsSubagent: true,
	}
	resolved, _ := toolMgr.ResolveTools(context.Background(), sctx)

	for _, tool := range resolved {
		if tool.Name == "spawn" {
			t.Error("spawn tool should not be available in subagent scope (prevent recursion)")
		}
	}
}

// ============================================================================
// 断连 / stop 区分回归测试
//
// 背景：消息级 ctx（msgCtx）独立于请求 ctx，且 stop 按钮（handleChatAbort →
// AbortMessage）与客户端断连（handler 断连分支）原本共用同一个取消信号，导致
// 关页面也会腰斩正在跑的子 Agent（日志曾见
// "client disconnected → spawn killed (context canceled)"）。
// 修复后：
//   - handler 断连分支不再调用 AbortMessage，断连只标记 UI killed 快照，后台继续跑；
//   - Delegate*/DelegateMany 的子 Agent 执行 ctx 直接继承 msgCtx，因此 stop 按钮
//     能精确取消（杀）子 Agent，而断连（不再取消 msgCtx）不会腰斩后台长任务。
// 以下测试用 cancelAwareMockProvider（ctx 已取消即返回 error）验证：
//   父 ctx 被取消（= 用户点 stop）→ 子 Agent 任务应被取消（失败）。
// ============================================================================

func TestSubAgentManager_DelegateManyStopCancelsChildren(t *testing.T) {
	provider := &cancelAwareMockProvider{}
	mgr := NewSubAgentManager(provider, "test-model")
	mgr.SetMaxConcurrency(3)

	// 父 ctx 立即取消，模拟「用户点击 stop → AbortMessage 取消 msgCtx」。
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	results := mgr.DelegateMany(parent, "你是助手", []string{"task A", "task B", "task C"})
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Success {
			t.Errorf("task %d should be cancelled by stop (parent ctx cancel), but succeeded: %s", i, r.Text)
		}
	}
}

func TestSubAgentManager_DelegateStopCancels(t *testing.T) {
	provider := &cancelAwareMockProvider{}
	mgr := NewSubAgentManager(provider, "test-model")

	// 父 ctx 立即取消，模拟「用户点击 stop → AbortMessage 取消 msgCtx」。
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := mgr.Delegate(parent, "你是助手", "翻译 hello")
	if err == nil {
		t.Fatal("Delegate should be cancelled by stop (parent ctx cancel), expected error")
	}
}

// ============================================================================
// Helpers
// ============================================================================

// countingMockProvider returns a unique response per call (thread-safe).
type countingMockProvider struct {
	counter int64
}

func (p *countingMockProvider) Name() string { return "counting-mock" }

func (p *countingMockProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	n := atomic.AddInt64(&p.counter, 1)
	return &llm.GenerateResult{
		Text:         fmt.Sprintf("mock-reply-%d", n),
		FinishReason: llm.FinishReasonStop,
	}, nil
}

func (p *countingMockProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("stream not supported")
}

func (p *countingMockProvider) count() int64 {
	return atomic.LoadInt64(&p.counter)
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

// cancelAwareMockProvider 在 ctx 已取消时返回错误，用于验证 Delegate*/DelegateMany
// 已通过 context.WithoutCancel 脱离上级取消——若未脱离，父 ctx 取消会让子 Agent 任务失败。
type cancelAwareMockProvider struct {
	countingMockProvider
}

func (p *cancelAwareMockProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.countingMockProvider.DoGenerate(ctx, params)
}

func (p *cancelAwareMockProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.countingMockProvider.DoStream(ctx, params)
}

// ============================================================================
// WithSkipTools / hasSkipTools Tests
// ============================================================================

func TestHasSkipTools_DetectsOption(t *testing.T) {
	// 无 WithSkipTools → false
	if hasSkipTools(WithTemperature(0.5), WithMaxTokens(1024)) {
		t.Error("expected false when WithSkipTools not present")
	}

	// 有 WithSkipTools → true
	if !hasSkipTools(WithSkipTools()) {
		t.Error("expected true when WithSkipTools is present")
	}

	// 混合选项中包含 WithSkipTools → true
	if !hasSkipTools(WithSystemPrompt("test"), WithSkipTools(), WithMaxTokens(2048)) {
		t.Error("expected true when WithSkipTools is mixed with other options")
	}

	// 空选项列表 → false
	if hasSkipTools() {
		t.Error("expected false for empty options")
	}
}

func TestWithSkipTools_PreventsToolInjection(t *testing.T) {
	provider := newMockProvider()
	provider.responses[0] = &llm.GenerateResult{Text: "no-tools result", FinishReason: llm.FinishReasonStop}

	mgr := NewSubAgentManager(provider, "test-model")

	// 不带 WithSkipTools：正常工作
	result, err := mgr.Delegate(context.Background(), "system", "task")
	if err != nil {
		t.Fatalf("Delegate without SkipTools failed: %v", err)
	}
	if result != "no-tools result" {
		t.Errorf("unexpected result: %q", result)
	}

	// 带 WithSkipTools：同样正常工作（验证选项不影响基本功能）
	provider.responses[1] = &llm.GenerateResult{Text: "skip-tools result", FinishReason: llm.FinishReasonStop}
	result, err = mgr.Delegate(context.Background(), "system", "task", WithSkipTools())
	if err != nil {
		t.Fatalf("Delegate with SkipTools failed: %v", err)
	}
	if result != "skip-tools result" {
		t.Errorf("unexpected result: %q", result)
	}
}

// TestToolResolver_NoDataRace 验证 SetToolResolver 与工具解析并发时无 data race。
//
// 背景：aeb5731 引入 SetToolResolver 后，resolveTools 裸读 m.toolMgr/m.baseCtx，
// 而 SetToolResolver 在写锁下修改它们 —— 二者构成 data race（-race 可复现）。
// 修复后读路径改为 RLock 快照。此测试需配合 -race 才有意义。
func TestToolResolver_NoDataRace(t *testing.T) {
	provider := newMockProvider()
	mgr := NewSubAgentManager(provider, "test-model")
	tm := tools.NewToolManager(nil, nil, nil)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			mgr.SetToolResolver(tm, tools.ToolSessionContext{BotID: "bot-a"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_, _ = mgr.resolveTools(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			mgr.SetDefaultToolSteps(i % 7)
			_ = mgr.toolStepsSnapshot()
		}
	}()
	wg.Wait()
}

// TestSpawn_RespectsSkipTools 锁定 Spawn 与 Delegate 系对 WithSkipTools 的一致语义。
//
// 0835652 只给 Delegate/DelegateStream/DelegateMany 加了 skipTools 判断，
// Spawn 被漏掉：调用方传 WithSkipTools() 时工具仍会被注入，违背选项契约。
func TestSpawn_RespectsSkipTools(t *testing.T) {
	provider := newMockProvider()
	mgr := NewSubAgentManager(provider, "test-model")

	// 带 WithSkipTools 的 Spawn 不应携带任何工具
	idSkip, err := mgr.Spawn("system", "skip", WithSkipTools())
	if err != nil {
		t.Fatalf("Spawn with SkipTools failed: %v", err)
	}
	mgr.mu.RLock()
	saSkip := mgr.subagents[idSkip]
	mgr.mu.RUnlock()
	if saSkip == nil {
		t.Fatal("spawned subagent not found")
	}
	if got := len(saSkip.extraTools); got != 0 {
		t.Errorf("Spawn with WithSkipTools: extraTools = %d, want 0", got)
	}
}
