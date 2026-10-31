package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/secret"
)

// recallLLM is a stand-in for the on-demand recall model. It never touches a
// network. failIfCalled locks the explicit-query light path.
type recallLLM struct {
	mu           sync.Mutex
	calls        int
	keywords     string
	refine       string
	blockOnCtx   bool
	failIfCalled bool
	tb           testing.TB
	err          error
	inspect      func(llm.GenerateParams)
}

func (p *recallLLM) Name() string { return "recall-test" }

func (p *recallLLM) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	if p.inspect != nil {
		p.inspect(params)
	}
	if p.err != nil {
		return nil, p.err
	}
	if p.failIfCalled && p.tb != nil {
		p.tb.Errorf("deep recall model was called")
	}
	if p.blockOnCtx {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	text := p.refine
	if strings.Contains(params.System, deepKeywordMarker) {
		text = p.keywords
	}
	return &llm.GenerateResult{Text: text, FinishReason: llm.FinishReasonStop}, nil
}

func (p *recallLLM) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, nil
}

func (p *recallLLM) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func recallTurns(lines ...llm.Message) context.Context {
	lc := llm.NewLiveContext()
	lc.SetSnapshot(lines)
	return llm.WithLiveContext(llm.WithToolTurnBudget(context.Background()), lc)
}

func deepTool(repo Repository, prov llm.Provider, extra DeepRecallConfig) llm.Tool {
	cfg := ToolConfig{Repo: repo, Recall: extra}
	cfg.Recall.Provider = prov
	return Tools(cfg)[0].Tool
}

func TestMemoryTool_AlwaysResident(t *testing.T) {
	repo := NewMemoryRepository()
	def := Tools(ToolConfig{Repo: repo})[0]
	if def.DeferredLoad {
		t.Fatal("memory must stay on the always-available tool list")
	}
	params, _ := def.Parameters.(map[string]any)
	props, _ := params["properties"].(map[string]any)
	if _, ok := props["deep"]; !ok {
		t.Fatal("search schema missing deep flag")
	}
	if _, ok := props["query"]; !ok {
		t.Fatal("search schema missing query")
	}

	d := llm.NewToolDeferral(true)
	d.SetTools([]llm.Tool{
		def.Tool,
		{Name: "rare_tool", Description: "rare", DeferredLoad: true, Parameters: map[string]any{"type": "object"}},
	})
	var found bool
	for _, tool := range d.View() {
		if tool.Name != "memory" {
			continue
		}
		found = true
		if tool.Parameters == nil {
			t.Fatal("memory schema is hidden; the tool is lazy")
		}
		if tool.DeferredLoad {
			t.Fatal("memory is marked deferred in the model-facing view")
		}
	}
	if !found {
		t.Fatal("memory missing from the always-available view")
	}
}

func TestMemorySearch_EmptyParamsDeepRecall(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	if err := repo.Append(ctx, Entry{
		ID: "mem-code", Scope: ChannelScope("ch1"),
		Content: "项目代号 lighthouse，仓库在内部文档",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(ctx, Entry{
		ID: "mem-lunch", Scope: ChannelScope("ch1"),
		Content: "午饭偏好是拉面",
	}); err != nil {
		t.Fatal(err)
	}

	prov := &recallLLM{
		keywords: `{"keywords":["lighthouse","拉面"]}`,
		refine:   `{"keep":["mem-code"]}`,
	}
	tool := deepTool(repo, prov, DeepRecallConfig{})
	exec := &llm.ToolExecContext{Context: recallTurns(
		llm.UserMessage("今天中午吃什么"),
		llm.AssistantMessage("还没定"),
	), UserRequest: "之前那个项目叫什么"}

	got, err := tool.Execute(exec, map[string]any{
		"action": "search", "scope_kind": "channel", "scope_id": "ch1",
	})
	if err != nil {
		t.Fatalf("deep search: %v", err)
	}
	m := got.(map[string]any)
	if m["mode"] != "deep" {
		t.Fatalf("mode = %#v, want deep", m["mode"])
	}
	if m["count"].(int) != 1 {
		t.Fatalf("count = %v, want 1 (refined): %#v", m["count"], m)
	}
	entries := m["entries"].([]EntryResult)
	if entries[0].ID != "mem-code" {
		t.Fatalf("refined entry = %+v", entries[0])
	}
	if prov.callCount() != 2 {
		t.Fatalf("deep path should call the model twice (keywords + refine), got %d", prov.callCount())
	}
}

func TestMemorySearch_ExplicitQueryStaysLight(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.Append(context.Background(), Entry{
		Scope: ChannelScope("ch1"), Content: "用户喜欢 Go 语言",
	}); err != nil {
		t.Fatal(err)
	}
	prov := &recallLLM{tb: t, failIfCalled: true, keywords: `{"keywords":["Go"]}`, refine: `{"keep":[]}`}
	tool := deepTool(repo, prov, DeepRecallConfig{})

	got, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"action": "search", "query": "Go", "scope_kind": "channel", "scope_id": "ch1",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if _, ok := m["mode"]; ok {
		t.Fatalf("explicit query must stay on the lexical path, got mode %#v", m["mode"])
	}
	if m["count"].(int) != 1 {
		t.Fatalf("count = %v", m["count"])
	}
	if m["entries"].([]EntryResult)[0].Content != "用户喜欢 Go 语言" {
		t.Fatalf("light search content changed: %+v", m["entries"])
	}
	if prov.callCount() != 0 {
		t.Fatalf("light path called the model %d times", prov.callCount())
	}
}

func TestMemorySearch_DeepFlagForcesLoop(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.Append(context.Background(), Entry{
		ID: "mem-go", Scope: ChannelScope("ch1"), Content: "用户喜欢 Go 语言",
	}); err != nil {
		t.Fatal(err)
	}
	prov := &recallLLM{
		keywords: `{"keywords":["Go 语言"]}`,
		refine:   `{"keep":["mem-go"]}`,
	}
	tool := deepTool(repo, prov, DeepRecallConfig{})
	got, err := tool.Execute(&llm.ToolExecContext{
		Context:     recallTurns(llm.UserMessage("他常用的语言是什么")),
		UserRequest: "他常用的语言是什么",
	}, map[string]any{
		"action": "search", "query": "Go", "deep": true,
		"scope_kind": "channel", "scope_id": "ch1",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["mode"] != "deep" || m["count"].(int) != 1 {
		t.Fatalf("deep flag should run the loop, got %#v", m)
	}
	if prov.callCount() != 2 {
		t.Fatalf("calls = %d, want 2", prov.callCount())
	}
}

func TestMemorySearch_BudgetAndTimeout(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		repo := NewMemoryRepository()
		if err := repo.Append(context.Background(), Entry{
			ID: "mem-code", Scope: ChannelScope("ch1"), Content: "项目代号 lighthouse",
		}); err != nil {
			t.Fatal(err)
		}
		prov := &recallLLM{
			keywords: `{"keywords":["lighthouse"]}`,
			refine:   `{"keep":["mem-code"]}`,
		}
		tool := deepTool(repo, prov, DeepRecallConfig{MaxCallsPerTurn: 1})
		ctx := recallTurns(llm.UserMessage("那个项目叫什么"))
		exec := &llm.ToolExecContext{Context: ctx, UserRequest: "那个项目叫什么"}
		args := map[string]any{"action": "search", "scope_kind": "channel", "scope_id": "ch1"}

		if _, err := tool.Execute(exec, args); err != nil {
			t.Fatal(err)
		}
		after := prov.callCount()
		second, err := tool.Execute(exec, args)
		if err != nil {
			t.Fatal(err)
		}
		m := second.(map[string]any)
		if m["count"].(int) != 0 {
			t.Fatalf("over-budget call should fail closed, got %#v", m)
		}
		msg, _ := m["message"].(string)
		if !strings.Contains(strings.ToLower(msg), "budget") {
			t.Fatalf("budget message = %q", msg)
		}
		if prov.callCount() != after {
			t.Fatalf("over-budget call still invoked the model (%d -> %d)", after, prov.callCount())
		}
	})

	t.Run("timeout", func(t *testing.T) {
		repo := NewMemoryRepository()
		prov := &recallLLM{blockOnCtx: true}
		tool := deepTool(repo, prov, DeepRecallConfig{Timeout: 40 * time.Millisecond})
		exec := &llm.ToolExecContext{
			Context:     recallTurns(llm.UserMessage("那个项目叫什么")),
			UserRequest: "那个项目叫什么",
		}
		start := time.Now()
		got, err := tool.Execute(exec, map[string]any{
			"action": "search", "scope_kind": "channel", "scope_id": "ch1",
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("deep recall hung the turn: %s", elapsed)
		}
		m := got.(map[string]any)
		if m["count"].(int) != 0 {
			t.Fatalf("timeout should fail closed, got %#v", m)
		}
		msg, _ := m["message"].(string)
		if !strings.Contains(strings.ToLower(msg), "timed out") {
			t.Fatalf("timeout message = %q", msg)
		}
	})
}

func TestMemorySearch_CoverageDropsRecentOverlapOnly(t *testing.T) {
	repo := NewMemoryRepository()
	ctx := context.Background()
	for _, e := range []Entry{
		{ID: "mem-code", Scope: ChannelScope("ch1"), Content: "项目代号 lighthouse，仓库在内部文档"},
		{ID: "mem-ramen", Scope: ChannelScope("ch1"), Content: "今天中午吃拉面，公司楼下那家"},
	} {
		if err := repo.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	prov := &recallLLM{
		keywords: `{"keywords":["lighthouse","拉面"]}`,
		// The model keeps both. Coverage must still drop the one already
		// visible in the last few turns, and must not use the older turns.
		refine: `{"keep":["mem-ramen","mem-code"]}`,
	}
	tool := deepTool(repo, prov, DeepRecallConfig{})
	turns := recallTurns(
		llm.UserMessage("项目代号是 lighthouse，仓库在内部文档"),
		llm.AssistantMessage("记下了"),
		llm.UserMessage("后来又改过一次代号"),
		llm.AssistantMessage("好"),
		llm.UserMessage("今天中午吃拉面"),
		llm.AssistantMessage("好，公司楼下那家"),
		llm.UserMessage("那家面怎么样"),
		llm.AssistantMessage("还行"),
	)
	got, err := tool.Execute(&llm.ToolExecContext{Context: turns, UserRequest: "那家面怎么样"}, map[string]any{
		"action": "search", "scope_kind": "channel", "scope_id": "ch1",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	entries := m["entries"].([]EntryResult)
	if len(entries) != 1 || entries[0].ID != "mem-code" {
		t.Fatalf("recent-window overlap should drop mem-ramen and keep the older fact, got %+v", entries)
	}
}

func TestMemorySearch_TimeBrowseSkipsDeepLoop(t *testing.T) {
	repo := NewMemoryRepository()
	_, _, _ = seedTimeSpanRepo(t, repo)
	prov := &recallLLM{tb: t, failIfCalled: true}
	tool := deepTool(repo, prov, DeepRecallConfig{})
	got, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"action": "search", "scope_kind": "channel", "scope_id": "ch1",
		"limit": 10, "order": "oldest",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if _, ok := m["mode"]; ok {
		t.Fatalf("time browse must not enter deep mode, got %#v", m["mode"])
	}
	if m["entries"].([]EntryResult)[0].Content != "八月的记忆" {
		t.Fatalf("order=oldest changed: %+v", m["entries"])
	}
	if prov.callCount() != 0 {
		t.Fatal("time browse called the deep model")
	}
}

func TestMemorySearch_DeepUsesVectorHits(t *testing.T) {
	repo := NewMemoryRepository()
	if err := repo.Append(context.Background(), Entry{
		ID: "mem-vec", Scope: ChannelScope("ch1"),
		Content: "northstar is the archive label",
	}); err != nil {
		t.Fatal(err)
	}
	// Retrieve hides the row so only the vector index can surface it.
	hidden := hideRetrieveRepo{MemoryRepository: repo, hide: "mem-vec"}
	vec := &fakeVec{hits: map[string][]Hit{
		"lighthouse": {{EntryID: "mem-vec", Distance: 0}},
	}}
	prov := &recallLLM{
		keywords: `{"keywords":["lighthouse"]}`,
		refine:   `{"keep":["mem-vec"]}`,
	}
	tool := deepTool(hidden, prov, DeepRecallConfig{Vec: vec})
	got, err := tool.Execute(&llm.ToolExecContext{
		Context:     recallTurns(llm.UserMessage("那个代号对应的档案标签是什么")),
		UserRequest: "那个代号对应的档案标签是什么",
	}, map[string]any{
		"action": "search", "scope_kind": "channel", "scope_id": "ch1",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	entries, _ := m["entries"].([]EntryResult)
	if len(entries) != 1 || entries[0].ID != "mem-vec" {
		t.Fatalf("vector hit was not loaded into the refined set: %#v", m)
	}
}

func TestMemorySearch_DeepCapsResultSize(t *testing.T) {
	repo := NewMemoryRepository()
	long := strings.Repeat("甲乙丙丁戊己庚辛壬癸", 40) // 400 runes, distinct from the window
	var keep []string
	for i := 0; i < 6; i++ {
		id := "mem-long-" + string(rune('a'+i))
		keep = append(keep, id)
		body := long + " " + id
		if err := repo.Append(context.Background(), Entry{
			ID: id, Scope: ChannelScope("ch1"), Content: body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	prov := &recallLLM{
		keywords: `{"keywords":["甲乙"]}`,
		refine:   `{"keep":["` + strings.Join(keep, `","`) + `"]}`,
	}
	tool := deepTool(repo, prov, DeepRecallConfig{})
	got, err := tool.Execute(&llm.ToolExecContext{
		Context:     recallTurns(llm.UserMessage("hello there")),
		UserRequest: "hello there",
	}, map[string]any{
		"action": "search", "scope_kind": "channel", "scope_id": "ch1",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	entries := m["entries"].([]EntryResult)
	if len(entries) == 0 || len(entries) > deepRecallMaxResults {
		t.Fatalf("result count = %d, want 1..%d", len(entries), deepRecallMaxResults)
	}
	total := 0
	for _, e := range entries {
		total += len([]rune(e.Content))
	}
	if total > deepRecallMaxChars {
		t.Fatalf("refined text is %d runes, cap is %d", total, deepRecallMaxChars)
	}
}

func TestRecentRecallPrompt_SummaryStaysOutOfCoverageWindow(t *testing.T) {
	summary := llm.SystemMessage(llm.FormatConversationSummary("project lighthouse from earlier"))
	thin := recallTurns(summary, llm.UserMessage("hi"))
	window, prompt := recentRecallView(thin, "", deepRecallRecentTurns)
	if !strings.Contains(prompt, "lighthouse") {
		t.Fatal("a thin recent window should pass the older topic summary to keyword expansion")
	}
	if strings.Contains(window, "lighthouse") {
		t.Fatal("the topic summary must not enter the coverage window")
	}

	fat := recallTurns(
		summary,
		llm.UserMessage("今天中午吃拉面"),
		llm.AssistantMessage("好，公司楼下那家"),
		llm.UserMessage("那家面怎么样"),
		llm.AssistantMessage("还行"),
	)
	_, fatPrompt := recentRecallView(fat, "", deepRecallRecentTurns)
	if strings.Contains(fatPrompt, "Older topic summary") {
		t.Fatal("a full recent window should not attach the older summary")
	}
}

func TestRecentRecallWindow_UsesTailNotFullHistory(t *testing.T) {
	ctx := recallTurns(
		llm.UserMessage("项目代号是 lighthouse，仓库在内部文档"),
		llm.AssistantMessage("记下了"),
		llm.UserMessage("后来又改过一次代号"),
		llm.AssistantMessage("好"),
		llm.UserMessage("今天中午吃拉面"),
		llm.AssistantMessage("好，公司楼下那家"),
		llm.UserMessage("那家面怎么样"),
		llm.AssistantMessage("还行"),
	)
	window, _ := recentRecallView(ctx, "那家面怎么样", deepRecallRecentTurns)
	if strings.Contains(window, "lighthouse") {
		t.Fatalf("coverage window included turns outside the last %d:\n%s", deepRecallRecentTurns, window)
	}
	if !strings.Contains(window, "拉面") {
		t.Fatalf("coverage window dropped the recent turns:\n%s", window)
	}
}

// hideRetrieveRepo drops one id from Retrieve so a test can prove the vector
// path loads it by id. FetchEntries still sees the row.
type hideRetrieveRepo struct {
	*MemoryRepository
	hide string
}

func (h hideRetrieveRepo) Retrieve(ctx context.Context, q Query) ([]Entry, error) {
	es, err := h.MemoryRepository.Retrieve(ctx, q)
	if err != nil || h.hide == "" {
		return es, err
	}
	out := make([]Entry, 0, len(es))
	for _, e := range es {
		if e.ID != h.hide {
			out = append(out, e)
		}
	}
	return out, nil
}

type fakeVec struct {
	hits map[string][]Hit
}

func (f *fakeVec) Enabled() bool { return true }

func (f *fakeVec) Search(_, query string, _ int) []Hit {
	if f == nil {
		return nil
	}
	return f.hits[query]
}

func TestDeepCompleteRedactsBeforeProvider(t *testing.T) {
	v, _ := secret.New("")
	prev := secret.Default()
	secret.SetDefault(v)
	t.Cleanup(func() { secret.SetDefault(prev) })
	const raw = "fictional-secret-for-recall"
	if err := v.Register("bot", "recall", raw, "test"); err != nil {
		t.Fatal(err)
	}
	p := &recallLLM{inspect: func(params llm.GenerateParams) {
		if strings.Contains(params.System, raw) || strings.Contains(llm.TextFromParts(params.Messages[0].Content), raw) {
			t.Error("plaintext reached provider")
		}
	}}
	if _, err := deepComplete(context.Background(), DeepRecallConfig{Provider: p}, raw, "memory: "+raw); err != nil {
		t.Fatal(err)
	}
}

func TestMemorySearch_ProviderFailureClosesWithoutRawError(t *testing.T) {
	tool := deepTool(NewMemoryRepository(), &recallLLM{err: errors.New("private provider diagnostic")}, DeepRecallConfig{})
	got, err := tool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{"action": "search"})
	if err != nil {
		t.Fatalf("raw error escaped: %v", err)
	}
	m := got.(map[string]any)
	if m["count"] != 0 || m["message"] != deepMsgFailed {
		t.Fatalf("unexpected closed result: %#v", m)
	}
}

func TestCapDeepResultsHonorsSmallBudget(t *testing.T) {
	for _, cap := range []int{1, 10, 280} {
		results := capDeepResults([]Entry{{Content: strings.Repeat("字", 2000)}}, 5, cap)
		if len(results) != 1 || len([]rune(results[0].Content)) > cap {
			t.Fatalf("result exceeds %d runes", cap)
		}
	}
}
