package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// convo builds: u0, a(call c1), tool(c1), a0, u1, a(call c2 + c3), tool(c2), tool(c3), a1, u2
func selfCompactConvo() []Message {
	call := func(ids ...string) Message {
		parts := []MessagePart{TextPart{Text: "working"}}
		for _, id := range ids {
			parts = append(parts, ToolCallPart{ToolCallID: id, ToolName: "exec", Input: map[string]any{"cmd": "ls " + id}})
		}
		return Message{Role: MessageRoleAssistant, Content: parts}
	}
	res := func(id string) Message {
		return ToolMessage(ToolResultPart{ToolCallID: id, ToolName: "exec", Result: "output of " + id})
	}
	return []Message{
		UserMessage("u0"),      // 0
		call("c1"),             // 1
		res("c1"),              // 2
		AssistantMessage("a0"), // 3
		UserMessage("u1"),      // 4
		call("c2", "c3"),       // 5
		res("c2"),              // 6
		res("c3"),              // 7
		AssistantMessage("a1"), // 8
		UserMessage("u2"),      // 9
	}
}

func TestIsSafeCompactionBoundary_ToolPairing(t *testing.T) {
	msgs := selfCompactConvo()
	cases := map[int]bool{
		0:  false, // nothing to compact
		1:  true,  // tail starts with the assistant call; its result is in the tail too
		2:  false, // tail starts with a tool result
		3:  true,
		4:  true,
		6:  false, // tool result first
		7:  false, // tool result first (and c3 pairs with head call)
		8:  true,
		9:  true,
		10: false, // out of range
	}
	for b, want := range cases {
		if got := IsSafeCompactionBoundary(msgs, b); got != want {
			t.Errorf("boundary %d: got %v want %v", b, got, want)
		}
	}

	// A result stored in a non-tool-role message after the boundary must still
	// be detected as split from its call in the head.
	odd := []Message{
		UserMessage("u"),
		{Role: MessageRoleAssistant, Content: []MessagePart{ToolCallPart{ToolCallID: "x", ToolName: "t"}}},
		AssistantMessage("interleaved"),
		{Role: MessageRoleUser, Content: []MessagePart{ToolResultPart{ToolCallID: "x", ToolName: "t", Result: "r"}}},
		UserMessage("tail"),
	}
	if IsSafeCompactionBoundary(odd, 2) {
		t.Error("boundary 2 separates call x from its result, want unsafe")
	}
	if !IsSafeCompactionBoundary(odd, 4) {
		t.Error("boundary 4 keeps call x with its result, want safe")
	}
}

func TestSelectCompactionBoundary(t *testing.T) {
	msgs := selfCompactConvo()
	// keep 3 → target 7 (a tool result) → must move earlier; 4 is a user turn.
	if b := SelectCompactionBoundary(msgs, 3); b != 4 {
		t.Fatalf("keep=3: boundary=%d want 4", b)
	}
	// keep 2 → target 8 (assistant, safe) → prefers user turn within window? 8
	// is safe; searching back finds 4 (user) only 4 steps away → returns 4.
	b := SelectCompactionBoundary(msgs, 2)
	if b != 4 && b != 8 {
		t.Fatalf("keep=2: boundary=%d want 4 or 8", b)
	}
	if !IsSafeCompactionBoundary(msgs, b) {
		t.Fatalf("keep=2: boundary %d not safe", b)
	}
	// keep 1 → target 9 (user) → 9.
	if b := SelectCompactionBoundary(msgs, 1); b != 9 {
		t.Fatalf("keep=1: boundary=%d want 9", b)
	}
	// keep >= len → nothing.
	if b := SelectCompactionBoundary(msgs, len(msgs)); b != 0 {
		t.Fatalf("keep=len: boundary=%d want 0", b)
	}
	// Every result: tail never contains a result whose call is in the head.
	for keep := 1; keep < len(msgs); keep++ {
		if b := SelectCompactionBoundary(msgs, keep); b != 0 && !IsSafeCompactionBoundary(msgs, b) {
			t.Fatalf("keep=%d produced unsafe boundary %d", keep, b)
		}
		if b := SelectCompactionBoundary(msgs, keep); b != 0 && len(msgs)-b < keep {
			t.Fatalf("keep=%d kept only %d messages", keep, len(msgs)-b)
		}
	}
}

func TestLiveContext_ScheduleAndApply(t *testing.T) {
	msgs := selfCompactConvo()
	lc := NewLiveContext()
	lc.SetSnapshot(msgs)

	if err := lc.ScheduleReplaceHead(6, ConversationSummaryMessage("s")); !errors.Is(err, ErrSelfCompactBadBoundary) {
		t.Fatalf("unsafe boundary: err=%v want ErrSelfCompactBadBoundary", err)
	}
	if lc.Compacted() {
		t.Fatal("rejected schedule must not mark compacted")
	}
	if err := lc.ScheduleReplaceHead(4, ConversationSummaryMessage("the summary")); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if err := lc.ScheduleReplaceHead(4, ConversationSummaryMessage("again")); !errors.Is(err, ErrSelfCompactAlreadyDone) {
		t.Fatalf("second schedule: err=%v want ErrSelfCompactAlreadyDone", err)
	}

	// The loop appends this step's messages before applying.
	grown := append(append([]Message{}, msgs...),
		Message{Role: MessageRoleAssistant, Content: []MessagePart{ToolCallPart{ToolCallID: "cc", ToolName: "compact_context"}}},
		ToolMessage(ToolResultPart{ToolCallID: "cc", ToolName: "compact_context", Result: "ok"}),
	)
	out, ok := lc.applyPending(grown)
	if !ok {
		t.Fatal("applyPending did not apply")
	}
	if len(out) != 1+len(grown)-4 {
		t.Fatalf("len(out)=%d want %d", len(out), 1+len(grown)-4)
	}
	if out[0].Role != MessageRoleSystem || !strings.Contains(TextFromParts(out[0].Content), "the summary") {
		t.Fatalf("first message is not the summary: %+v", out[0])
	}
	if TextFromParts(out[1].Content) != "u1" {
		t.Fatalf("tail should start at u1, got %q", TextFromParts(out[1].Content))
	}
	// Pairing intact: PatchToolCalls must be a no-op.
	if patched := PatchToolCalls(out); len(patched) != len(out) {
		t.Fatalf("tool pairing broken after compaction: %d → %d", len(out), len(patched))
	}
	if !lc.Compacted() {
		t.Fatal("Compacted() should stay true after apply (once per run)")
	}
	if err := lc.ScheduleReplaceHead(2, ConversationSummaryMessage("x")); !errors.Is(err, ErrSelfCompactAlreadyDone) {
		t.Fatalf("schedule after apply: err=%v want ErrSelfCompactAlreadyDone", err)
	}
	if _, ok := lc.applyPending(out); ok {
		t.Fatal("nothing pending, applyPending must be a no-op")
	}
}

func TestLiveContext_DropsReplacementWhenHistoryRewritten(t *testing.T) {
	msgs := selfCompactConvo()
	lc := NewLiveContext()
	lc.SetSnapshot(msgs)
	if err := lc.ScheduleReplaceHead(4, ConversationSummaryMessage("s")); err != nil {
		t.Fatal(err)
	}
	// Loop state shrank (e.g. rewritten by a hook) → do not cut blindly.
	if _, ok := lc.applyPending(msgs[:3]); ok {
		t.Fatal("replacement must be dropped when messages shrank")
	}
}

func TestRenderMessagesForSummary_IncludesToolsAndClips(t *testing.T) {
	msgs := selfCompactConvo()
	msgs[2] = ToolMessage(ToolResultPart{ToolCallID: "c1", ToolName: "exec", Result: strings.Repeat("x", 5000), IsError: true})
	out := RenderMessagesForSummary(msgs, 100, 0)
	for _, want := range []string{`<tool_call name="exec">`, `<tool_error name="exec">`, "[user]: u0", "(truncated)"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q", want)
		}
	}
	// Total limit keeps the newest messages.
	small := RenderMessagesForSummary(msgs, 100, 40)
	if !strings.Contains(small, "u2") || !strings.Contains(small, "older messages omitted") {
		t.Errorf("total limit should keep newest and mark omission, got %q", small)
	}
}

type summaryFakeProvider struct {
	mu     sync.Mutex
	prompt string
	system string
	text   string
	err    error
}

func (p *summaryFakeProvider) Name() string { return "fake" }
func (p *summaryFakeProvider) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.system = params.System
	if len(params.Messages) > 0 {
		p.prompt = TextFromParts(params.Messages[0].Content)
	}
	if p.err != nil {
		return nil, p.err
	}
	return &GenerateResult{Text: p.text}, nil
}
func (p *summaryFakeProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	return nil, errors.New("not implemented")
}

func TestSummarizeForSelfCompact_FocusAndIncrementalAnchor(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig())
	p := &summaryFakeProvider{text: "## Goal\n- first"}
	head := selfCompactConvo()[:4]
	sum, err := c.SummarizeForSelfCompact(context.Background(), p, ChatModel("m"), head, "keep ticket #42")
	if err != nil {
		t.Fatal(err)
	}
	if sum != "## Goal\n- first" || c.PreviousSummary() != sum {
		t.Fatalf("summary/anchor mismatch: %q / %q", sum, c.PreviousSummary())
	}
	if p.system != CompactionSystemPrompt {
		t.Error("summarizer must reuse CompactionSystemPrompt")
	}
	for _, want := range []string{"keep ticket #42", "<must-preserve>", `<tool_result name="exec">`, "Create a new anchored summary"} {
		if !strings.Contains(p.prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// Second call updates the anchor incrementally.
	p.text = "## Goal\n- second"
	if _, err := c.SummarizeForSelfCompact(context.Background(), p, ChatModel("m"), head, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.prompt, "<previous-summary>") || !strings.Contains(p.prompt, "- first") {
		t.Error("second summary should update the previous anchor")
	}
	// Empty output is an error and keeps the anchor.
	p.text = "   "
	if _, err := c.SummarizeForSelfCompact(context.Background(), p, ChatModel("m"), head, ""); err == nil {
		t.Error("empty summary must be an error")
	}
	if c.PreviousSummary() != "## Goal\n- second" {
		t.Error("failed summary must not clobber the anchor")
	}
}

// selfCompactScriptProvider: step 0 calls "compact_now"; step 1 records the
// messages it receives and finishes.
type selfCompactScriptProvider struct {
	mu    sync.Mutex
	calls int
	seen  [][]Message
}

func (p *selfCompactScriptProvider) Name() string { return "fake" }

func (p *selfCompactScriptProvider) record(params GenerateParams) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.calls
	p.calls++
	cp := make([]Message, len(params.Messages))
	copy(cp, params.Messages)
	p.seen = append(p.seen, cp)
	return n
}

func (p *selfCompactScriptProvider) DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error) {
	if p.record(params) == 0 {
		return &GenerateResult{
			FinishReason: FinishReasonToolCalls,
			ToolCalls:    []ToolCall{{ToolCallID: "cc1", ToolName: "compact_now", Input: map[string]any{}}},
		}, nil
	}
	return &GenerateResult{Text: "done", FinishReason: FinishReasonStop}, nil
}

func (p *selfCompactScriptProvider) DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error) {
	n := p.record(params)
	ch := make(chan StreamPart, 8)
	go func() {
		defer close(ch)
		if n == 0 {
			ch <- &StreamToolCallPart{ToolCallID: "cc1", ToolName: "compact_now", Input: map[string]any{}}
			ch <- &FinishStepPart{FinishReason: FinishReasonToolCalls}
			ch <- &FinishPart{FinishReason: FinishReasonToolCalls}
			return
		}
		ch <- &TextDeltaPart{Text: "done"}
		ch <- &FinishStepPart{FinishReason: FinishReasonStop}
		ch <- &FinishPart{FinishReason: FinishReasonStop}
	}()
	return &StreamResult{Stream: ch}, nil
}

func compactNowTool(t *testing.T, boundary int) Tool {
	return Tool{
		Name:       "compact_now",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(ctx *ToolExecContext, input any) (any, error) {
			lc := LiveContextFromContext(ctx)
			if lc == nil {
				t.Error("no LiveContext in tool ctx")
				return nil, errors.New("no live context")
			}
			if got := len(lc.Snapshot()); got != 10 {
				t.Errorf("snapshot len=%d want 10", got)
			}
			return "ok", lc.ScheduleReplaceHead(boundary, ConversationSummaryMessage("S"))
		},
	}
}

func assertCompactedNextStep(t *testing.T, seen [][]Message) {
	t.Helper()
	if len(seen) != 2 {
		t.Fatalf("provider calls=%d want 2", len(seen))
	}
	next := seen[1]
	// [summary] + convo[4:] (6 msgs) + assistant(call cc1) + tool(result cc1)
	if len(next) != 1+6+2 {
		t.Fatalf("step1 messages=%d want 9", len(next))
	}
	if next[0].Role != MessageRoleSystem || !strings.Contains(TextFromParts(next[0].Content), "S") {
		t.Fatalf("step1 first message should be the summary, got %+v", next[0])
	}
	if TextFromParts(next[1].Content) != "u1" {
		t.Fatalf("step1 tail should start with u1, got %q", TextFromParts(next[1].Content))
	}
	last := next[len(next)-1]
	if last.Role != MessageRoleTool {
		t.Fatalf("compact tool result must be kept in the tail, got role %s", last.Role)
	}
	for _, p := range last.Content {
		if tr, ok := p.(ToolResultPart); ok && strings.Contains(stringifyResult(tr.Result), DefaultPatchPlaceholder) {
			t.Fatal("tool result was patched — pairing broken")
		}
	}
}

func TestOrchestrateGenerate_AppliesSelfCompaction(t *testing.T) {
	prov := &selfCompactScriptProvider{}
	cfg := &OrchestrateConfig{
		Params:   GenerateParams{Messages: selfCompactConvo(), Tools: []Tool{compactNowTool(t, 4)}},
		MaxSteps: 5,
	}
	if _, err := OrchestrateGenerate(context.Background(), prov, cfg); err != nil {
		t.Fatal(err)
	}
	assertCompactedNextStep(t, prov.seen)
}

func TestOrchestrateStream_AppliesSelfCompaction(t *testing.T) {
	prov := &selfCompactScriptProvider{}
	cfg := &OrchestrateConfig{
		Params:   GenerateParams{Messages: selfCompactConvo(), Tools: []Tool{compactNowTool(t, 4)}},
		MaxSteps: 5,
	}
	sr, err := OrchestrateStream(context.Background(), prov, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for range sr.Stream {
	}
	assertCompactedNextStep(t, prov.seen)
}
