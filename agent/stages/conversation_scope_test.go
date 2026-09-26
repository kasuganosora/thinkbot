package stages

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

func tgEnv(chat string) *core.Envelope {
	return core.NewEnvelope(core.Message{
		ID: "tg-1", BotID: "bot-1", Source: "telegram", Channel: chat, Text: "fix the code",
		Metadata: map[string]any{agenttools.ExtraKeyChatSessionID: "tg:" + chat},
	})
}

func misskeyTimelineEnv() *core.Envelope {
	return core.NewEnvelope(core.Message{
		ID: "mk-1", BotID: "bot-1", Source: "Misskey", Channel: "misskey:timeline", Text: "timeline note",
	})
}

func TestConversationKey(t *testing.T) {
	tg := conversationKey(tgEnv("76017910"))
	mk := conversationKey(misskeyTimelineEnv())
	if tg != "chat:tg:76017910" {
		t.Fatalf("telegram key=%q", tg)
	}
	if mk != "chan:bot-1:Misskey:misskey:timeline" {
		t.Fatalf("misskey key=%q", mk)
	}
	if tg == mk {
		t.Fatal("different conversations must get different keys")
	}
	env := misskeyTimelineEnv()
	env.Set("session.id", "s-9")
	if k := conversationKey(env); k != "sess:s-9" {
		t.Fatalf("session.id must win: %q", k)
	}
	if k := conversationKey(core.NewEnvelope(core.Message{BotID: "bot-1"})); k != "" {
		t.Fatalf("unidentifiable conversation must yield empty key, got %q", k)
	}
	if k := conversationKey(nil); k != "" {
		t.Fatalf("nil env: %q", k)
	}
}

func TestGetCompactor_ScopedPerConversationNeverShared(t *testing.T) {
	s := NewLLMStage("llm", &selfCompactSummaryProvider{text: "S"}, LLMConfig{
		Model: llm.ChatModel("m"), MaxSteps: 10, Compaction: func() *llm.CompactionConfig { c := llm.DefaultCompactionConfig(); return &c }(),
	}, nil, nil)
	a, ok := s.getCompactor(conversationKey(tgEnv("1")))
	b, _ := s.getCompactor(conversationKey(misskeyTimelineEnv()))
	if !ok || a == nil || b == nil || a == b {
		t.Fatal("different conversations must get different compactors")
	}
	if again, _ := s.getCompactor(conversationKey(tgEnv("1"))); again != a {
		t.Fatal("same conversation must keep its compactor across turns")
	}
	e1, _ := s.getCompactor("")
	e2, _ := s.getCompactor("")
	if e1 == nil || e1 == e2 {
		t.Fatal("empty key must give an ephemeral compactor, never a shared __default__")
	}
	if _, ok := s.compactors.Load("__default__"); ok {
		t.Fatal("no shared __default__ compactor may be created")
	}
	if c1, c2 := s.summaryCompactor(""), s.summaryCompactor(""); c1 == c2 {
		t.Fatal("summaryCompactor must not share an instance for an empty key")
	}
}

// sourceToolResolver gives telegram turns the sandbox tool set and Misskey
// timeline turns a reduced set without sandbox tools (as in production).
type sourceToolResolver struct {
	tg, mk []llm.Tool
}

func (r sourceToolResolver) ResolveForEnvelope(_ context.Context, env *core.Envelope) ([]llm.Tool, error) {
	if env.Message.Source == "telegram" {
		return append([]llm.Tool(nil), r.tg...), nil
	}
	return append([]llm.Tool(nil), r.mk...), nil
}

// turnRecordingProvider: turns that have "exec" call it once, then stop;
// other turns stop at once. Every step's tool names are recorded per turn.
type turnRecordingProvider struct {
	mu    sync.Mutex
	steps map[string][][]string // "tg" / "mk" → per-step tool names
}

func (p *turnRecordingProvider) Name() string { return "turns" }
func (p *turnRecordingProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	names := make([]string, 0, len(params.Tools))
	for _, t := range params.Tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	turn := "mk"
	for _, m := range params.Messages {
		if strings.Contains(llm.TextFromParts(m.Content), "fix the code") {
			turn = "tg"
		}
	}
	p.mu.Lock()
	p.steps[turn] = append(p.steps[turn], names)
	n := len(p.steps[turn])
	p.mu.Unlock()
	if turn == "tg" && n == 1 {
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls:    []llm.ToolCall{{ToolCallID: "c1", ToolName: "exec", Input: map[string]any{}}},
		}, nil
	}
	return &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: "done"}, nil
}
func (p *turnRecordingProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func stageTool(name string, deferred bool, exec func()) llm.Tool {
	return llm.Tool{
		Name: name, Description: "tool " + name, Parameters: map[string]any{"type": "object"}, DeferredLoad: deferred,
		Execute: func(*llm.ToolExecContext, any) (any, error) {
			if exec != nil {
				exec()
			}
			return name + "-ok", nil
		},
	}
}

// TestLLMStage_ConcurrentTurnsKeepTheirOwnTools is the end-to-end version of
// the 2026-09-26 incident: a Misskey timeline turn of the same bot runs while
// a Telegram turn is between steps; the Telegram turn must keep exec and the
// file tools, and the two conversations get separate deferral state.
func TestLLMStage_ConcurrentTurnsKeepTheirOwnTools(t *testing.T) {
	mkStarted, mkDone := make(chan struct{}), make(chan struct{})
	resolver := sourceToolResolver{
		tg: []llm.Tool{
			stageTool("exec", false, func() { close(mkStarted); <-mkDone }),
			stageTool("read_file", false, nil),
			stageTool("replace_in_file", false, nil),
			stageTool("memory", false, nil),
			stageTool("mcp__browser_open", true, nil),
		},
		mk: []llm.Tool{
			stageTool("memory", false, nil),
			stageTool("misskey_search_notes", true, nil),
		},
	}
	store := llm.NewDeferralStore(true)
	prov := &turnRecordingProvider{steps: map[string][][]string{}}
	s := NewLLMStage("llm", prov, LLMConfig{
		Model: llm.ChatModel("m"), MaxSteps: 10, ToolResolver: resolver, ToolDeferral: store,
	}, nil, zap.NewNop().Sugar())

	var wg sync.WaitGroup
	var tgErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, tgErr = s.Process(context.Background(), tgEnv("76017910"))
	}()
	<-mkStarted
	if _, err := s.Process(context.Background(), misskeyTimelineEnv()); err != nil {
		t.Fatal(err)
	}
	close(mkDone)
	wg.Wait()
	if tgErr != nil {
		t.Fatal(tgErr)
	}

	tg := prov.steps["tg"]
	if len(tg) != 2 {
		t.Fatalf("telegram turn: want 2 steps, got %v", tg)
	}
	for i, step := range tg {
		j := "," + strings.Join(step, ",") + ","
		for _, want := range []string{"exec", "read_file", "replace_in_file", "memory"} {
			if !strings.Contains(j, ","+want+",") {
				t.Errorf("telegram step %d lost %q: %v", i, want, step)
			}
		}
		if strings.Contains(j, "misskey_search_notes") {
			t.Errorf("telegram step %d got Misskey's tool list: %v", i, step)
		}
	}
	if store.Len() != 2 {
		t.Fatalf("the two conversations must resolve separate deferrals, store len=%d", store.Len())
	}
}
