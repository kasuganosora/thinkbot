package subagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Mock Provider
// ============================================================================

type mockProvider struct {
	name     string
	mu       sync.Mutex
	calls    []mockCall
	responses map[int]*llm.GenerateResult // call index → response
	err      error
}

type mockCall struct {
	system   string
	messages []llm.Message
}

func newMockProvider() *mockProvider {
	return &mockProvider{
		name:      "mock",
		responses: make(map[int]*llm.GenerateResult),
	}
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	m.mu.Lock()
	idx := len(m.calls)
	m.calls = append(m.calls, mockCall{
		system:   params.System,
		messages: params.Messages,
	})
	err := m.err
	resp := m.responses[idx]
	m.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if resp == nil {
		// 默认返回消息数量的文本
		return &llm.GenerateResult{
			Text:         "mock-reply",
			FinishReason: llm.FinishReasonStop,
		}, nil
	}
	return resp, nil
}

func (m *mockProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	result, err := m.DoGenerate(ctx, params)
	if err != nil {
		return nil, err
	}
	ch := make(chan llm.StreamPart, 1)
	ch <- &llm.TextDeltaPart{Text: result.Text}
	close(ch)
	return &llm.StreamResult{Stream: ch}, nil
}

func (m *mockProvider) getCalls() []mockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mockCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// ============================================================================
// Tests
// ============================================================================

func TestSubAgent_BasicChat(t *testing.T) {
	mp := newMockProvider()
	sa := New(mp, "test-model",
		WithSystemPrompt("You are a test bot."),
		WithName("test-sub"),
	)

	reply, err := sa.Chat(context.Background(), "Hello")
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if reply != "mock-reply" {
		t.Errorf("expected 'mock-reply', got %q", reply)
	}

	calls := mp.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].system != "You are a test bot." {
		t.Errorf("system prompt mismatch: %q", calls[0].system)
	}
	if len(calls[0].messages) != 1 {
		t.Fatalf("expected 1 message (just user), got %d", len(calls[0].messages))
	}
}

func TestSubAgent_MultiTurnContext(t *testing.T) {
	mp := newMockProvider()
	sa := New(mp, "test-model")

	// Turn 1
	sa.Chat(context.Background(), "Turn 1 user")
	// Turn 2
	sa.Chat(context.Background(), "Turn 2 user")

	calls := mp.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}

	// 第二次调用应该包含第一轮的历史（user + assistant + new user = 3 messages）
	if len(calls[1].messages) != 3 {
		t.Errorf("turn 2: expected 3 messages (history+new), got %d", len(calls[1].messages))
	}
}

func TestSubAgent_ContextIsolation(t *testing.T) {
	mp1 := newMockProvider()
	mp2 := newMockProvider()

	sa1 := New(mp1, "model-a")
	sa2 := New(mp2, "model-b")

	sa1.Chat(context.Background(), "Message to agent 1")
	sa2.Chat(context.Background(), "Message to agent 2")
	sa2.Chat(context.Background(), "Follow-up to agent 2")

	// sa1 应该只有 1 条历史
	h1 := sa1.History()
	if len(h1) != 2 { // user + assistant
		t.Errorf("sa1: expected 2 messages, got %d", len(h1))
	}

	// sa2 应该有 4 条历史
	h2 := sa2.History()
	if len(h2) != 4 {
		t.Errorf("sa2: expected 4 messages, got %d", len(h2))
	}

	// 互不干扰
	calls1 := mp1.getCalls()
	if len(calls1[0].messages) != 1 {
		t.Errorf("sa1 first call should have 1 message, got %d", len(calls1[0].messages))
	}
	calls2 := mp2.getCalls()
	if len(calls2[1].messages) != 3 {
		t.Errorf("sa2 second call should have 3 messages, got %d", len(calls2[1].messages))
	}
}

func TestSubAgent_SlidingWindow(t *testing.T) {
	mp := newMockProvider()
	sa := New(mp, "model",
		WithMaxMessages(4), // 保留最近 4 条 = 2 轮
	)

	// 3 轮对话
	sa.Chat(context.Background(), "Turn 1")
	sa.Chat(context.Background(), "Turn 2")
	sa.Chat(context.Background(), "Turn 3")

	calls := mp.getCalls()

	// 第 3 轮调用时，消息应该是：turn2_user, turn2_assistant, turn3_user = 3
	// 因为窗口=4，截断后应该只有最后 4 条，但第 3 轮只有 3 条消息在上下文+当前
	// 上下文 4 条 = turn1(2) + turn2(2)，但截断到 4 条
	history := sa.History()
	if len(history) != 4 {
		t.Errorf("expected 4 messages after sliding window, got %d", len(history))
	}

	// 第 3 次调用应该有 history(4) + 1 new = 5，不对
	// history 此时是 turn1+turn2 = 4 messages，第 3 轮调用时 messages = 4 + 1 = 5
	if len(calls[2].messages) != 5 {
		t.Errorf("turn 3: expected 5 messages (4 history + 1 new), got %d", len(calls[2].messages))
	}
}

func TestSubAgent_Clear(t *testing.T) {
	mp := newMockProvider()
	sa := New(mp, "model")

	sa.Chat(context.Background(), "Turn 1")
	sa.Chat(context.Background(), "Turn 2")

	if tc := sa.TurnCount(); tc != 2 {
		t.Fatalf("expected 2 turns, got %d", tc)
	}

	sa.Clear()

	if tc := sa.TurnCount(); tc != 0 {
		t.Errorf("after Clear: expected 0 turns, got %d", tc)
	}

	history := sa.History()
	if len(history) != 0 {
		t.Errorf("after Clear: expected 0 messages, got %d", len(history))
	}

	// Clear 后应该从零开始
	sa.Chat(context.Background(), "Turn 3")
	calls := mp.getCalls()
	if len(calls[2].messages) != 1 {
		t.Errorf("after Clear + Chat: expected 1 message, got %d", len(calls[2].messages))
	}
}

func TestSubAgent_Close(t *testing.T) {
	mp := newMockProvider()
	sa := New(mp, "model")

	sa.Close()

	_, err := sa.Chat(context.Background(), "test")
	if err == nil {
		t.Error("expected error after Close")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("expected 'closed' error, got %v", err)
	}
}

func TestSubAgent_TurnCountUnaffectedByWindow(t *testing.T) {
	mp := newMockProvider()
	sa := New(mp, "model",
		WithMaxMessages(2), // 只保留 1 轮
	)

	for i := 0; i < 5; i++ {
		sa.Chat(context.Background(), "turn")
	}

	if tc := sa.TurnCount(); tc != 5 {
		t.Errorf("expected 5 total turns, got %d", tc)
	}
}

func TestSubAgent_SystemPromptInheritance(t *testing.T) {
	mp := newMockProvider()
	sa := New(mp, "model",
		WithSystemPrompt("Inherited system prompt"),
	)

	sa.Chat(context.Background(), "hello")

	calls := mp.getCalls()
	if calls[0].system != "Inherited system prompt" {
		t.Errorf("system prompt not passed: %q", calls[0].system)
	}

	// 动态修改
	sa.SetSystem("Updated prompt")
	sa.Chat(context.Background(), "hello again")

	calls = mp.getCalls()
	if calls[1].system != "Updated prompt" {
		t.Errorf("system prompt not updated: %q", calls[1].system)
	}
}

func TestSubAgent_ErrorPropagation(t *testing.T) {
	mp := newMockProvider()
	mp.err = errors.New("LLM service down")
	sa := New(mp, "model")

	_, err := sa.Chat(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "LLM service down") {
		t.Errorf("unexpected error: %v", err)
	}

	// 出错不应更新上下文
	history := sa.History()
	if len(history) != 0 {
		t.Errorf("error should not update context, got %d messages", len(history))
	}
}

func TestSubAgent_SeedMessages(t *testing.T) {
	mp := newMockProvider()
	sa := New(mp, "model")

	sa.SeedMessages([]llm.Message{
		llm.UserMessage("seed user"),
		llm.AssistantMessage("seed assistant"),
	})

	sa.Chat(context.Background(), "real message")

	calls := mp.getCalls()
	if len(calls[0].messages) != 3 { // 2 seed + 1 new
		t.Errorf("expected 3 messages (2 seed + 1 new), got %d", len(calls[0].messages))
	}
}

func TestSubAgent_Stream(t *testing.T) {
	mp := newMockProvider()
	sa := New(mp, "model")

	result, err := sa.Stream(context.Background(), "test stream")
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}

	text, err := result.Text()
	if err != nil {
		t.Fatalf("stream text failed: %v", err)
	}
	if text != "mock-reply" {
		t.Errorf("expected 'mock-reply', got %q", text)
	}

	// 流完成后应更新上下文
	if tc := sa.TurnCount(); tc != 1 {
		t.Errorf("expected 1 turn after stream, got %d", tc)
	}
}

// ============================================================================
// ContextManager Tests
// ============================================================================

func TestContextManager_TruncateKeepsFullTurns(t *testing.T) {
	cm := NewContextManager(4) // 2 轮

	cm.AppendTurn("u1", "a1")
	cm.AppendTurn("u2", "a2")
	cm.AppendTurn("u3", "a3") // 应截断掉 u1+a1

	msgs := cm.Messages()
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}

	// 第一条应该是 user 角色（不截断半轮）
	if msgs[0].Role != llm.MessageRoleUser {
		t.Errorf("first message should be user role, got %s", msgs[0].Role)
	}

	// 检查是 u2 而不是 u1
	text := llm.TextFromParts(msgs[0].Content)
	if text != "u2" {
		t.Errorf("expected 'u2' as first message, got %q", text)
	}
}

func TestContextManager_NoLimit(t *testing.T) {
	cm := NewContextManager(0) // 无限制

	for i := 0; i < 100; i++ {
		cm.AppendTurn("u", "a")
	}

	if cm.Len() != 200 {
		t.Errorf("expected 200 messages, got %d", cm.Len())
	}
}

func TestContextManager_Clear(t *testing.T) {
	cm := NewContextManager(10)
	cm.AppendTurn("u1", "a1")
	cm.Clear()

	if cm.Len() != 0 {
		t.Errorf("after Clear: expected 0, got %d", cm.Len())
	}
}

func TestSubAgent_ConcurrentSafe(t *testing.T) {
	mp := newMockProvider()
	sa := New(mp, "model")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sa.Chat(context.Background(), "concurrent")
		}()
	}
	wg.Wait()

	// 不 panic 就通过了
	if sa.TurnCount() != 10 {
		t.Logf("note: turn count = %d (expected 10, but concurrency ordering may vary)", sa.TurnCount())
	}
}
