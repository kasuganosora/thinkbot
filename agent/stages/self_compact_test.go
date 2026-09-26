package stages

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

type selfCompactSummaryProvider struct {
	mu    sync.Mutex
	calls int
	text  string
	err   error
}

func (p *selfCompactSummaryProvider) Name() string { return "fake" }
func (p *selfCompactSummaryProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &llm.GenerateResult{Text: p.text}, nil
}
func (p *selfCompactSummaryProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("not implemented")
}

type memCheckpointStore struct {
	mu      sync.Mutex
	saved   []ContextCheckpoint
	prev    uint64
	lastAt  map[string]time.Time // session → newest checkpoint created_at
	lastErr error
}

func (m *memCheckpointStore) LatestContextCheckpointBoundary(botID, sessionID string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := len(m.saved); n > 0 {
		return m.saved[n-1].BoundaryMessageID, nil
	}
	return m.prev, nil
}

func (m *memCheckpointStore) LastContextCheckpointAt(botID, sessionID string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastErr != nil {
		return time.Time{}, m.lastErr
	}
	return m.lastAt[sessionID], nil
}

func (m *memCheckpointStore) setLastAt(sessionID string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastAt == nil {
		m.lastAt = map[string]time.Time{}
	}
	m.lastAt[sessionID] = t
}

func (m *memCheckpointStore) SaveContextCheckpoint(cp ContextCheckpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved = append(m.saved, cp)
	if m.lastAt == nil {
		m.lastAt = map[string]time.Time{}
	}
	m.lastAt[cp.SessionID] = time.Now()
	return nil
}

// longHistory returns n alternating user/assistant messages with ~tokens each.
func longHistory(n int) []llm.Message {
	var out []llm.Message
	body := strings.Repeat("some detailed discussion about the project ", 80)
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			out = append(out, llm.UserMessage(fmt.Sprintf("u%d %s", i, body)))
		} else {
			out = append(out, llm.AssistantMessage(fmt.Sprintf("a%d %s", i, body)))
		}
	}
	return out
}

func newSelfCompactStage(p llm.Provider, cfg *SelfCompactConfig) *LLMStage {
	return NewLLMStage("llm", p, LLMConfig{
		Model:       llm.ChatModel("m"),
		MaxSteps:    10,
		SelfCompact: cfg,
	}, nil, nil)
}

func webEnv(sessionID string) *core.Envelope {
	env := core.NewEnvelope(core.Message{
		ID: "m1", BotID: "bot-1", Source: "web", Channel: "web",
		Metadata: map[string]any{agenttools.ExtraKeyChatSessionID: sessionID},
	})
	return env
}

func execCtxWith(snapshot []llm.Message) (*llm.ToolExecContext, *llm.LiveContext) {
	lc := llm.NewLiveContext()
	lc.SetSnapshot(snapshot)
	return &llm.ToolExecContext{Context: llm.WithLiveContext(context.Background(), lc), ToolName: CompactContextToolName}, lc
}

func TestParseCompactContextArgs(t *testing.T) {
	a, err := parseCompactContextArgs(nil, 6)
	if err != nil || a.KeepRecent != 6 || a.Focus != "" {
		t.Fatalf("nil input: %+v %v", a, err)
	}
	a, _ = parseCompactContextArgs(map[string]any{"keep_recent": float64(1), "focus": "  ids  "}, 6)
	if a.KeepRecent != minSelfCompactKeepRecent || a.Focus != "ids" {
		t.Fatalf("clamp low: %+v", a)
	}
	a, _ = parseCompactContextArgs(`{"keep_recent": 500}`, 6)
	if a.KeepRecent != maxSelfCompactKeepRecent {
		t.Fatalf("clamp high: %+v", a)
	}
	a, _ = parseCompactContextArgs(map[string]any{"focus": strings.Repeat("é", 5000)}, 6)
	if n := len([]rune(a.Focus)); n != maxSelfCompactFocusRunes {
		t.Fatalf("focus not clipped: %d", n)
	}
	if _, err := parseCompactContextArgs("{bad", 6); err == nil {
		t.Fatal("invalid JSON must error")
	}
}

func TestCompactContext_ShortHistoryNoop(t *testing.T) {
	p := &selfCompactSummaryProvider{text: "S"}
	s := newSelfCompactStage(p, &SelfCompactConfig{})
	env := webEnv("sess-1")
	tool := s.newCompactContextTool(env, nil)
	ctx, lc := execCtxWith([]llm.Message{llm.UserMessage("hi"), llm.AssistantMessage("hello")})
	out, err := tool.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("short history must not error: %v", err)
	}
	res := out.(map[string]any)
	if res["status"] != "noop" || !strings.Contains(res["reason"].(string), "already short") {
		t.Fatalf("want noop/short, got %+v", res)
	}
	if p.calls != 0 || lc.Compacted() {
		t.Fatal("no-op must not call the summarizer or schedule a replacement")
	}
	// A no-op does not start the cooldown.
	ctx2, _ := execCtxWith(longHistory(20))
	if _, err := tool.Execute(ctx2, map[string]any{}); err != nil {
		t.Fatalf("after noop, real compaction should be allowed: %v", err)
	}
}

func TestCompactContext_SuccessPersistsCheckpoint(t *testing.T) {
	p := &selfCompactSummaryProvider{text: "## Goal\n- ship it"}
	store := &memCheckpointStore{}
	s := newSelfCompactStage(p, &SelfCompactConfig{
		Store: store,
		// history rows 101..116 are the first 16 messages; the rest is the
		// current turn (IDs unknown → not listed).
		HistoryMessageIDs: func(core.Message) []uint64 {
			ids := make([]uint64, 16)
			for i := range ids {
				ids[i] = uint64(101 + i)
			}
			return ids
		},
	})
	base := longHistory(20)
	env := webEnv("sess-1")
	tool := s.newCompactContextTool(env, base)
	ctx, lc := execCtxWith(base)

	out, err := tool.Execute(ctx, map[string]any{"keep_recent": float64(6), "focus": "remember PR #7"})
	if err != nil {
		t.Fatal(err)
	}
	res := out.(map[string]any)
	if res["status"] != "compacted" {
		t.Fatalf("status=%v", res["status"])
	}
	if res["compacted_messages"].(int) != 14 || res["kept_recent"].(int) != 6 || res["messages_after"].(int) != 7 {
		t.Fatalf("unexpected counts: %+v", res)
	}
	if res["est_tokens_after"].(int) >= res["est_tokens_before"].(int) {
		t.Fatalf("tokens did not shrink: %+v", res)
	}
	if !strings.Contains(res["summary_preview"].(string), "ship it") || res["persisted"] != true {
		t.Fatalf("preview/persisted wrong: %+v", res)
	}
	if !lc.Compacted() {
		t.Fatal("replacement not scheduled on the live context")
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved=%d want 1", len(store.saved))
	}
	cp := store.saved[0]
	// head = messages[:14] → history rows 101..114.
	if cp.BoundaryMessageID != 114 || cp.SessionID != "sess-1" || cp.BotID != "bot-1" || cp.Focus != "remember PR #7" {
		t.Fatalf("checkpoint wrong: %+v", cp)
	}

	// Same turn: second call refused.
	if _, err := tool.Execute(ctx, map[string]any{}); err == nil || !strings.Contains(err.Error(), "already compacted") {
		t.Fatalf("second call in same turn: err=%v", err)
	}
	// Next turn, same session: cooldown.
	tool2 := s.newCompactContextTool(env, base)
	ctx2, _ := execCtxWith(base)
	if _, err := tool2.Execute(ctx2, map[string]any{}); err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("cooldown: err=%v", err)
	}
	// Other session is independent.
	tool3 := s.newCompactContextTool(webEnv("sess-2"), base)
	ctx3, _ := execCtxWith(base)
	if _, err := tool3.Execute(ctx3, map[string]any{}); err != nil {
		t.Fatalf("other session should not be cooled down: %v", err)
	}
	// After the cooldown window it works again (in-memory and persisted).
	s.selfCompactCD.mark("chat:sess-1", time.Now().Add(-time.Hour))
	store.setLastAt("sess-1", time.Now().Add(-time.Hour))
	ctx4, _ := execCtxWith(base)
	if _, err := s.newCompactContextTool(env, base).Execute(ctx4, map[string]any{}); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
}

func TestCompactContext_SummarizerErrorLeavesContext(t *testing.T) {
	p := &selfCompactSummaryProvider{err: errors.New("boom")}
	store := &memCheckpointStore{}
	s := newSelfCompactStage(p, &SelfCompactConfig{Store: store})
	tool := s.newCompactContextTool(webEnv("sess-1"), nil)
	ctx, lc := execCtxWith(longHistory(20))
	_, err := tool.Execute(ctx, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "summarization failed") {
		t.Fatalf("want summarization error, got %v", err)
	}
	if lc.Compacted() || len(store.saved) != 0 {
		t.Fatal("failed summarization must not change anything")
	}
	// Failure does not start the (10min) success cooldown, but a shorter
	// failure backoff: an immediate retry is refused (2026-09-26: three
	// failed summaries in one turn), once it expires a retry works.
	p.err = nil
	p.text = "S"
	ctx2, _ := execCtxWith(longHistory(20))
	if _, err := s.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx2, map[string]any{}); err == nil ||
		!strings.Contains(err.Error(), "paused after a recent failed attempt") {
		t.Fatalf("immediate retry after failure must be refused: %v", err)
	}
	s.selfCompactFail.mu.Lock()
	st := s.selfCompactFail.m["chat:sess-1"]
	st.until = time.Now().Add(-time.Second)
	s.selfCompactFail.m["chat:sess-1"] = st
	s.selfCompactFail.mu.Unlock()
	ctx3, _ := execCtxWith(longHistory(20))
	if _, err := s.newCompactContextTool(webEnv("sess-1"), nil).Execute(ctx3, map[string]any{}); err != nil {
		t.Fatalf("retry after the failure backoff: %v", err)
	}
}

func TestCompactContext_NoChatSessionIsTurnOnly(t *testing.T) {
	p := &selfCompactSummaryProvider{text: "S"}
	store := &memCheckpointStore{}
	s := newSelfCompactStage(p, &SelfCompactConfig{Store: store})
	env := core.NewEnvelope(core.Message{ID: "n1", BotID: "bot-1", Source: "misskey", Channel: "mk"})
	ctx, lc := execCtxWith(longHistory(20))
	out, err := s.newCompactContextTool(env, nil).Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	res := out.(map[string]any)
	if res["persisted"] != false || !strings.Contains(res["persist_note"].(string), "rest of this turn") {
		t.Fatalf("misskey-like channel should be turn-only: %+v", res)
	}
	if !lc.Compacted() || len(store.saved) != 0 {
		t.Fatal("expected live compaction without a checkpoint")
	}
}

func TestCompactContext_NoLiveContext(t *testing.T) {
	s := newSelfCompactStage(&selfCompactSummaryProvider{text: "S"}, &SelfCompactConfig{})
	tool := s.newCompactContextTool(webEnv("x"), nil)
	if _, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, nil); err == nil {
		t.Fatal("outside an orchestration loop the tool must error")
	}
}

func TestCompactedHistoryBoundary_ConservativeOnMismatch(t *testing.T) {
	base := longHistory(6)
	ids := []uint64{11, 12, 13, 14, 15}
	if got := compactedHistoryBoundary(base, 4, base, ids); got != 14 {
		t.Fatalf("aligned: got %d want 14", got)
	}
	// Boundary past the history rows (into the current turn) → last history row.
	if got := compactedHistoryBoundary(base, 6, base, ids); got != 15 {
		t.Fatalf("beyond history: got %d want 15", got)
	}
	// Live messages rewritten (auto compaction put a summary first) → nothing trusted.
	rewritten := append([]llm.Message{llm.ConversationSummaryMessage("auto")}, base[3:]...)
	if got := compactedHistoryBoundary(rewritten, 3, base, ids); got != 0 {
		t.Fatalf("rewritten: got %d want 0", got)
	}
	// Synthetic summary entries carry ID 0 and never advance the boundary.
	withSum := append([]llm.Message{llm.ConversationSummaryMessage("ckpt")}, base...)
	if got := compactedHistoryBoundary(withSum, 3, withSum, append([]uint64{0}, ids...)); got != 12 {
		t.Fatalf("with synthetic summary: got %d want 12", got)
	}
}

func TestShouldOfferSelfCompact(t *testing.T) {
	s := newSelfCompactStage(&selfCompactSummaryProvider{}, &SelfCompactConfig{})
	tools := []llm.Tool{{Name: "exec"}}
	if !s.shouldOfferSelfCompact(webEnv("x"), tools) {
		t.Fatal("should be offered on a normal turn with tools")
	}
	if s.shouldOfferSelfCompact(webEnv("x"), nil) {
		t.Fatal("no tools (lurk/reaction) → not offered")
	}
	hb := core.NewEnvelope(core.Message{BotID: "b", Source: core.SourceHeartbeat})
	if s.shouldOfferSelfCompact(hb, tools) {
		t.Fatal("heartbeat → not offered")
	}
	hb2 := webEnv("x")
	hb2.Set(core.KVHeartbeatMode, true)
	if s.shouldOfferSelfCompact(hb2, tools) {
		t.Fatal("heartbeat mode → not offered")
	}
	if s.shouldOfferSelfCompact(webEnv("x"), []llm.Tool{{Name: CompactContextToolName}}) {
		t.Fatal("already present → not duplicated")
	}
	off := newSelfCompactStage(&selfCompactSummaryProvider{}, nil)
	if off.shouldOfferSelfCompact(webEnv("x"), tools) {
		t.Fatal("SelfCompact nil → disabled")
	}
	tool := s.newCompactContextTool(webEnv("x"), nil)
	if tool.DeferredLoad {
		t.Fatal("compact_context must not be deferred (needed when context is large)")
	}
	for _, w := range []string{"USE it when", "DO NOT use it when"} {
		if !strings.Contains(tool.Description, w) {
			t.Fatalf("description missing %q", w)
		}
	}
}
