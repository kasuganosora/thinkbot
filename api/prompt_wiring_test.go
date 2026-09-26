package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/engagement"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/stages"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/skill"
	"github.com/kasuganosora/thinkbot/tools"
)

type promptCaptureProvider struct{ systems []string }

func (p *promptCaptureProvider) Name() string { return "capture" }
func (p *promptCaptureProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.systems = append(p.systems, params.System)
	return &llm.GenerateResult{Text: "ok", FinishReason: llm.FinishReasonStop}, nil
}
func (p *promptCaptureProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("no stream")
}

const (
	testSoul         = "# Shiina\n\nYou are Shiina, a maid. You call your owner Ojou-sama."
	testSystemPrompt = "Always answer in Japanese unless asked otherwise."
)

// promptHarness 复刻 botservice 的装配：per-bot Registry + SoulLoader + ToolManager（关闭自动描述）
// + SkillManager，PromptStage（newMainPromptStage）→ LLMStage。
type promptHarness struct {
	reg      *prompt.Registry
	soul     *prompt.SoulLoader
	soulPath string
	stage    *prompt.PromptStage
	llm      *stages.LLMStage
	prov     *promptCaptureProvider
}

func newPromptHarness(t *testing.T) *promptHarness {
	t.Helper()
	dir := t.TempDir()
	h := &promptHarness{reg: prompt.NewRegistry(), prov: &promptCaptureProvider{}, soulPath: filepath.Join(dir, "SOUL.md")}
	if err := os.WriteFile(h.soulPath, []byte(testSoul), 0o644); err != nil {
		t.Fatal(err)
	}
	h.soul = prompt.NewSoulLoader(prompt.SoulLoaderConfig{Path: h.soulPath, BotID: "bot-a", SectionName: "identity", ReloadInterval: 50 * time.Millisecond}, h.reg)
	if err := h.soul.Load(); err != nil {
		t.Fatal(err)
	}

	mgr := agenttools.NewToolManager(h.reg, nil, zap.NewNop().Sugar())
	mgr.EnableAutoDescribe(false)
	if err := tools.RegisterTools(mgr, tools.Config{}); err != nil { // common_tools 指引
		t.Fatal(err)
	}
	// 同 Order 的多个工具段落：顺序必须确定（按名称），不随 map 迭代变化
	for _, name := range []string{"zeta", "alpha", "mid"} {
		if err := mgr.Register(agenttools.ToolDef{
			Tool:          llm.Tool{Name: name + "_tool", Description: "desc of " + name},
			PromptSection: &agenttools.ToolPromptSection{Name: name + "_tools", Order: 310, Content: "## Tool guide " + name, Enabled: true},
		}); err != nil {
			t.Fatal(err)
		}
	}
	skills := skill.NewSkillManager(skill.NewPromptRegistryAdapter(
		func(name string, order int, content string, enabled bool) {
			h.reg.Register(prompt.Section{Name: name, Order: order, Content: content, Enabled: enabled})
		}, h.reg.Unregister), nil, zap.NewNop().Sugar())
	skills.Register(&skill.Skill{Name: "weather", Description: "weather lookups", Content: "SKILL BODY weather", Enabled: true})
	skills.Register(&skill.Skill{Name: "diary", Description: "keep a diary", Content: "SKILL BODY diary", Enabled: true})

	h.stage = newMainPromptStage(h.reg, noop.NewTracerProvider(), zap.NewNop().Sugar())
	h.llm = stages.NewLLMStage("llm", h.prov, stages.LLMConfig{
		Model: llm.ChatModel("capture"), MaxSteps: 1, HardMaxSteps: 1, RequireReplyControl: true,
	}, noop.NewTracerProvider(), zap.NewNop().Sugar())
	return h
}

// turn 跑一轮：模拟 Bot.OnBeforeProcess 注入的 KV + RecallStage 的记忆召回。
func (h *promptHarness) turn(t *testing.T, text, recall string, lurk bool) string {
	t.Helper()
	env := core.NewEnvelope(core.Message{ID: "m-" + text, BotID: "bot-a", Source: "telegram", Text: text, ChatType: core.ChatPrivate})
	env.Set("bot.id", "bot-a")
	env.Set("bot.config", bot.BotConfig{SystemPrompt: testSystemPrompt})
	env.Set(core.KVSoulContent, h.soul.Content())
	if recall != "" {
		env.Set(core.KVMemoryRecall, recall)
	}
	if lurk {
		env.Set(core.KVLurkMode, true)
	}
	ctx := context.Background()
	env, err := h.stage.Process(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.llm.Process(ctx, env); err != nil {
		t.Fatal(err)
	}
	return h.prov.systems[len(h.prov.systems)-1]
}

func TestMainPromptContainsIdentityOperatorToolsAndSkills(t *testing.T) {
	h := newPromptHarness(t)
	sys := h.turn(t, "hello", "[Memory] Ojou-sama likes tea.", false)

	idx := func(s string) int {
		i := strings.Index(sys, s)
		if i < 0 {
			t.Fatalf("system prompt missing %q:\n%s", s, sys)
		}
		if strings.Count(sys, s) != 1 {
			t.Fatalf("%q appears %d times:\n%s", s, strings.Count(sys, s), sys)
		}
		return i
	}
	order := []int{
		idx("You are Shiina, a maid."),       // identity(0)
		idx("# Operator Instructions"),       // operator_instructions(10)
		idx(testSystemPrompt),                //
		idx("## Skills"),                     // skill_trigger(150)
		idx("## Tool guide alpha"),           // tool_*(310)，同 Order 按名称
		idx("## Tool guide mid"),             //
		idx("## Tool guide zeta"),            //
		idx("[Memory] Ojou-sama likes tea."), // 记忆召回（逐轮）在稳定前缀之后
		idx("REPLY CONTROL PROTOCOL"),        // 回复控制协议
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("section %d out of order:\n%s", i, sys)
		}
	}
	if strings.Contains(sys, "desc of alpha") {
		t.Fatalf("auto tool descriptions must not be rendered (they duplicate the tool schema):\n%s", sys)
	}
	idx("# Common Tools") // tools.RegisterTools 的 common_tools 指引
	if strings.Contains(sys, "SKILL BODY") {
		t.Fatalf("skill bodies must not be injected (loaded on demand via use_skill):\n%s", sys)
	}
	if _, ok := h.reg.Get("skill_weather"); !ok {
		t.Fatal("test setup: skill body section should exist in the registry")
	}
}

func TestMainPromptStablePrefixAcrossTurns(t *testing.T) {
	h := newPromptHarness(t)
	var prefixes []string
	for i, text := range []string{"hello", "what's the weather in Tokyo?", "tell me a story"} {
		recall := "[Memory] turn " + text
		sys := h.turn(t, text, recall, false)
		cut := strings.Index(sys, recall)
		if cut < 0 {
			t.Fatalf("turn %d: recall missing", i)
		}
		prefixes = append(prefixes, sys[:cut])
	}
	for i := 1; i < len(prefixes); i++ {
		if prefixes[i] != prefixes[0] {
			t.Fatalf("stable prefix differs between turns:\n--- 0 ---\n%s\n--- %d ---\n%s", prefixes[0], i, prefixes[i])
		}
	}
	if !strings.HasPrefix(prefixes[0], "# Shiina") {
		t.Fatalf("prefix must start with the SOUL identity: %q", prefixes[0][:40])
	}
	// Registry.List 本身也必须确定
	for i := 0; i < 20; i++ {
		a, b := h.reg.List(), h.reg.List()
		for j := range a {
			if a[j].Name != b[j].Name {
				t.Fatalf("registry order not deterministic at %d: %s vs %s", j, a[j].Name, b[j].Name)
			}
		}
	}
}

func TestLurkPromptHasSoulOnce(t *testing.T) {
	h := newPromptHarness(t)
	sys := h.turn(t, "some public post", "", true)
	if n := strings.Count(sys, "You are Shiina, a maid."); n != 1 {
		t.Fatalf("lurk prompt must contain SOUL exactly once, got %d:\n%s", n, sys)
	}
}

func TestSoulEditPickedUpWithoutRestart(t *testing.T) {
	h := newPromptHarness(t)
	if sys := h.turn(t, "hi", "", false); !strings.Contains(sys, "You are Shiina, a maid.") {
		t.Fatal("initial SOUL missing")
	}
	// soul 工具的 rewrite：WriteRaw + Load（立即热重载）
	if err := h.soul.WriteRaw(context.Background(), []byte("# Shiina v2\n\nYou are Shiina, now a butler.")); err != nil {
		t.Fatal(err)
	}
	if err := h.soul.Load(); err != nil {
		t.Fatal(err)
	}
	sys := h.turn(t, "hi again", "", false)
	if !strings.Contains(sys, "now a butler") || strings.Contains(sys, "a maid.") {
		t.Fatalf("edited SOUL not picked up:\n%s", sys)
	}

	// 外部编辑：5s watcher 按 mtime 重载
	future := time.Now().Add(time.Minute)
	if err := os.WriteFile(h.soulPath, []byte("# Shiina v3\n\nYou are Shiina, a gardener."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(h.soulPath, future, future); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.soul.StartWatcher(ctx)
	defer h.soul.Stop()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(h.soul.Content(), "gardener") {
		time.Sleep(100 * time.Millisecond)
	}
	if sys := h.turn(t, "hi third", "", false); !strings.Contains(sys, "a gardener") {
		t.Fatalf("watcher reload not picked up:\n%s", sys)
	}
}

func TestPromptWithoutSoulUsesSystemPromptAsIdentity(t *testing.T) {
	reg := prompt.NewRegistry()
	st := newMainPromptStage(reg, nil, nil)
	env := core.NewEnvelope(core.Message{ID: "m", Text: "x"})
	env.Set("bot.config", bot.BotConfig{SystemPrompt: testSystemPrompt})
	env, err := st.Process(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := env.Get("system.prompt")
	if v != testSystemPrompt {
		t.Fatalf("got %q", v)
	}
}

func TestJudgeGetsShortPersona(t *testing.T) {
	long := testSoul + "\n\n" + strings.Repeat("More persona detail line.\n", 200)
	p := prompt.ShortPersona(prompt.ComposeIdentity(long, testSystemPrompt), judgePersonaRunes)
	if !strings.Contains(p, "You are Shiina, a maid.") || len([]rune(p)) > judgePersonaRunes || strings.Contains(p, "# ") {
		t.Fatalf("persona %q (len %d)", p, len([]rune(p)))
	}
	// 未运行的 bot：回退到 system_prompt
	if got := judgePersona(nil, testSystemPrompt); got != testSystemPrompt {
		t.Fatalf("got %q", got)
	}
	cur := "first persona"
	cfg := engagement.PromptConfig{BotName: "栞娜", PersonaFunc: func() string { return cur }}
	sys, _ := engagement.BuildJudgePrompt(cfg, &core.Message{Text: "post"})
	if !strings.Contains(sys, "Persona: first persona") {
		t.Fatalf("%s", sys)
	}
	cur = "second persona" // 热重载后下一次判定即生效
	sys, _ = engagement.BuildScoredJudgePrompt(cfg, &core.Message{Text: "post"})
	if !strings.Contains(sys, "Persona: second persona") {
		t.Fatalf("%s", sys)
	}
	sys, _ = engagement.BuildJudgePrompt(engagement.PromptConfig{BotName: "x"}, &core.Message{Text: "post"})
	if !strings.Contains(sys, "Persona: a friendly chat bot") {
		t.Fatalf("empty persona must fall back to the default: %s", sys)
	}
}
