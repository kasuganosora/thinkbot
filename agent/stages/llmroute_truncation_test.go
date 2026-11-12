package stages

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/route"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestLightTierEffort(t *testing.T) {
	glm52 := llm.ChatModel("glm-5.2")
	glm53 := llm.ChatModel("glm-5.3")
	deepseek := llm.ChatModel("deepseek-flash")
	gpt := llm.ChatModel("gpt-4o")
	cases := []struct {
		in    string
		model *llm.Model
		want  string
	}{
		// GLM-5.2 serves low/medium as high; the floor that skips thinking is minimal.
		{"medium", glm52, "minimal"},
		{"high", glm52, "minimal"},
		{"max", glm52, "minimal"},
		{"low", glm52, "minimal"},
		{"  HIGH  ", glm52, "minimal"},
		{"minimal", glm52, "minimal"},
		// Three-step vocabularies: low is the cheapest accepted value.
		{"medium", glm53, route.LightReasoningEffort},
		{"medium", deepseek, route.LightReasoningEffort},
		{"low", deepseek, route.LightReasoningEffort},
		{"minimal", deepseek, "minimal"}, // replyEffort maps this to low
		{"medium", gpt, route.LightReasoningEffort},
		{"", glm52, ""},
		{"none", glm52, "none"},
		{"off", glm52, "off"},
		{"default", glm52, "default"},
		{llm.ReasoningProvider, glm52, llm.ReasoningProvider},
	}
	for _, c := range cases {
		if got := lightTierEffort(c.in, c.model); got != c.want {
			t.Errorf("lightTierEffort(%q, %s) = %q, want %q", c.in, c.model.ID, got, c.want)
		}
	}
}

// The bug this guards against: a "medium" bot setting was normalized through
// the main model's low/high/max vocabulary (-> "high") and then sent to the
// light model, which spent its whole 1024-token output budget on thinking.
// On GLM-5.2, capping at "low" does not help: that family serves low as high.
func TestReplyEffort_LightTierVocabulary(t *testing.T) {
	mainModel := llm.ChatModel("deepseek-flash")
	lightModel := llm.ChatModel("glm-5.2")

	if got := replyEffort("medium", mainModel); got == nil || *got != "high" {
		t.Fatalf("main model effort = %v, want high (unchanged default-tier behavior)", got)
	}
	got := replyEffort(lightTierEffort("medium", lightModel), lightModel)
	if got == nil || *got != "minimal" {
		t.Fatalf("light tier effort = %v, want minimal", got)
	}
	if got := replyEffort(lightTierEffort("none", lightModel), lightModel); got == nil || *got != "none" {
		t.Fatalf("explicit off must be preserved, got %v", got)
	}
	ds := llm.ChatModel("deepseek-flash")
	if got := replyEffort(lightTierEffort("medium", ds), ds); got == nil || *got != "low" {
		t.Fatalf("deepseek light effort = %v, want low", got)
	}
}

func TestLightTierExhausted(t *testing.T) {
	atCap := func(reason llm.FinishReason, text string, tokens int) *llm.GenerateResult {
		return &llm.GenerateResult{
			FinishReason: reason,
			Text:         text,
			Usage:        llm.Usage{OutputTokens: tokens},
		}
	}
	cases := []struct {
		name   string
		light  bool
		res    *llm.GenerateResult
		budget int
		want   bool
	}{
		{"exhausted light turn", true, &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: ""}, 1024, true},
		{"whitespace only", true, &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: " \n "}, 1024, true},
		{"partial reply is not exhaustion", true, &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: "half"}, 1024, false},
		{"clean stop under budget", true, &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: ""}, 1024, false},
		{"stop at the sent budget", true, atCap(llm.FinishReasonStop, "", 1024), 1024, true},
		{"stop over the sent budget", true, atCap(llm.FinishReasonStop, " \n", 1100), 1024, true},
		{"tool-call DSL is not a body", true, atCap(llm.FinishReasonLength, dsmlLeakFull, 100), 1024, true},
		{"stop with only tool-call DSL at cap", true, atCap(llm.FinishReasonStop, dsmlLeakTruncated, 1024), 1024, true},
		{"stop with DSL under budget stays", true, atCap(llm.FinishReasonStop, dsmlLeakFull, 20), 1024, false},
		{"full tier not eligible", false, &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: ""}, 1024, false},
		{"nil result", true, nil, 1024, false},
		{"unknown budget does not treat stop as exhaustion", true, atCap(llm.FinishReasonStop, "", 1024), 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lightTierExhausted(c.light, c.res, c.budget); got != c.want {
				t.Fatalf("lightTierExhausted = %v, want %v", got, c.want)
			}
		})
	}
}

func TestAppendTruncationNotice(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		light bool
		want  string
	}{
		{"notice is an addition", "half answer", false, "half answer\n\n" + truncationNotice},
		{"light empty body emits no bare notice", "", true, ""},
		{"full tier empty body still explains", "", false, truncationNotice},
		{"light whitespace body stays untouched", "  \n ", true, "  \n "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := appendTruncationNotice(c.text, c.light); got != c.want {
				t.Fatalf("appendTruncationNotice(%q, %v) = %q, want %q", c.text, c.light, got, c.want)
			}
		})
	}
}

func newLightTierStage(main, light llm.Provider, effort string) *LLMStage {
	return NewLLMStage("llm", main, LLMConfig{
		Model:           llm.ChatModel("deepseek-flash"),
		LightProvider:   light,
		LightModel:      llm.ChatModel("glm-5.2"),
		ReasoningEffort: effort,
		MaxSteps:        1,
	}, nil, zap.NewNop().Sugar())
}

func newLightTierEnvelope() *core.Envelope {
	env := core.NewEnvelope(core.Message{ID: "m", BotID: "bot", Text: "改好了么"})
	route.SetTier(env, route.IntentProgress, route.ModelLight)
	return env
}

func TestLLMStage_LightTierUsesLowEffortOnLightModel(t *testing.T) {
	var gotEffort *string
	var gotModelID string
	var gotMaxTokens *int
	light := &tierProvider{
		name: "light",
		reply: func(_ int, params llm.GenerateParams) (*llm.GenerateResult, error) {
			gotEffort = params.ReasoningEffort
			if params.Model != nil {
				gotModelID = params.Model.ID
			}
			gotMaxTokens = params.MaxTokens
			return &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: "short answer"}, nil
		},
	}
	main := &tierProvider{name: "main", reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: "unused"}, nil
	}}
	stage := newLightTierStage(main, light, "medium")

	if _, err := stage.Process(context.Background(), newLightTierEnvelope()); err != nil {
		t.Fatalf("process: %v", err)
	}
	if gotModelID != "glm-5.2" {
		t.Fatalf("light tier ran model %q, want glm-5.2", gotModelID)
	}
	if gotEffort == nil || *gotEffort != "minimal" {
		t.Fatalf("light tier effort = %v, want minimal (glm-5.2 serves low as high)", gotEffort)
	}
	if gotMaxTokens != nil {
		t.Fatalf("light tier max tokens = %v, want the field omitted when the model has no maxTokens", gotMaxTokens)
	}
	if main.calls != 0 {
		t.Fatalf("main model called %d times on a healthy light turn", main.calls)
	}
}

// A light turn that burns its budget on reasoning must be retried on the full
// model, not surfaced as a bare "output truncated" notice.
func TestLLMStage_LightTierEmptyLengthUpgradesWithoutNotice(t *testing.T) {
	light := &tierProvider{name: "light", reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: ""}, nil
	}}
	main := &tierProvider{name: "main", reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: "已经改好了"}, nil
	}}
	stage := newLightTierStage(main, light, "medium")

	out, err := stage.Process(context.Background(), newLightTierEnvelope())
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if light.calls != 1 || main.calls != 1 {
		t.Fatalf("light calls=%d main calls=%d, want 1/1", light.calls, main.calls)
	}
	actions := out.Actions()
	if len(actions) != 1 {
		t.Fatalf("actions = %#v, want one reply", actions)
	}
	payload, _ := actions[0].Payload.(string)
	if payload != "已经改好了" || strings.Contains(payload, truncationNotice) {
		t.Fatalf("reply = %q, want the full-model answer without a bare notice", payload)
	}
}

// Guard the last-resort behavior: on the full tier an empty, truncated reply
// still tells the user something instead of silently dropping the turn.
func TestLLMStage_FullTierEmptyLengthStillNotifies(t *testing.T) {
	main := &tierProvider{name: "main", reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: ""}, nil
	}}
	stage := NewLLMStage("llm", main, LLMConfig{
		Model:           llm.ChatModel("glm-5.2"),
		ReasoningEffort: "medium",
		MaxSteps:        1,
	}, nil, zap.NewNop().Sugar())
	env := core.NewEnvelope(core.Message{ID: "m", BotID: "bot", Text: "继续"})

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	actions := out.Actions()
	if len(actions) != 1 {
		t.Fatalf("actions = %#v, want one reply", actions)
	}
	payload, _ := actions[0].Payload.(string)
	if payload != truncationNotice {
		t.Fatalf("reply = %q, want %q", payload, truncationNotice)
	}
}

// The provider reports stop, not length, when the shared budget is used up.
// The usage fallback must compare against the model's configured max tokens,
// which is the budget that was actually sent.
func TestLLMStage_LightTierStopAtCapUpgrades(t *testing.T) {
	const budget = 64
	light := &tierProvider{name: "light", reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonStop,
			Text:         "",
			Usage:        llm.Usage{OutputTokens: budget},
		}, nil
	}}
	main := &tierProvider{name: "main", reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: "已经改好了"}, nil
	}}
	stage := newLightTierStage(main, light, "medium")
	stage.config.MaxTokens = intPtr(budget)

	out, err := stage.Process(context.Background(), newLightTierEnvelope())
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if light.calls != 1 || main.calls != 1 {
		t.Fatalf("light calls=%d main calls=%d, want 1/1", light.calls, main.calls)
	}
	actions := out.Actions()
	if len(actions) != 1 {
		t.Fatalf("actions = %#v, want one reply", actions)
	}
	payload, _ := actions[0].Payload.(string)
	if payload != "已经改好了" || strings.Contains(payload, truncationNotice) {
		t.Fatalf("reply = %q, want the full-model answer without a bare notice", payload)
	}
}

// A light turn that does produce a partial answer at the model's output cap
// still tells the user the answer was cut.
func TestLLMStage_LightTierPartialAtCapNotifies(t *testing.T) {
	const budget = 64
	light := &tierProvider{name: "light", reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonStop,
			Text:         "half answer",
			Usage:        llm.Usage{OutputTokens: budget},
		}, nil
	}}
	main := &tierProvider{name: "main", reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		t.Fatal("partial light reply must not upgrade")
		return nil, nil
	}}
	stage := newLightTierStage(main, light, "medium")
	stage.config.MaxTokens = intPtr(budget)

	out, err := stage.Process(context.Background(), newLightTierEnvelope())
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	actions := out.Actions()
	if len(actions) != 1 {
		t.Fatalf("actions = %#v, want one reply", actions)
	}
	payload, _ := actions[0].Payload.(string)
	want := "half answer\n\n" + truncationNotice
	if payload != want {
		t.Fatalf("reply = %q, want %q", payload, want)
	}
}

// Tool-call DSL is not a reply. A light turn that only emitted it and hit
// the length cap retries on the full model instead of going silent after the
// DSL is stripped.
func TestLLMStage_LightTierDSMLOnlyUpgrades(t *testing.T) {
	light := &tierProvider{name: "light", reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: dsmlLeakFull}, nil
	}}
	main := &tierProvider{name: "main", reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
		return &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: "已经改好了"}, nil
	}}
	stage := newLightTierStage(main, light, "medium")

	out, err := stage.Process(context.Background(), newLightTierEnvelope())
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if light.calls != 1 || main.calls != 1 {
		t.Fatalf("light calls=%d main calls=%d, want 1/1", light.calls, main.calls)
	}
	payload, _ := out.Actions()[0].Payload.(string)
	if payload != "已经改好了" || strings.Contains(payload, "DSML") {
		t.Fatalf("reply = %q", payload)
	}
}

func TestLLMStage_IntentJudgeUsesModelVocabulary(t *testing.T) {
	var judgeEffort *string
	var sawJudge bool
	main := &tierProvider{name: "main", reply: func(n int, params llm.GenerateParams) (*llm.GenerateResult, error) {
		if params.System != "" && len(params.Tools) == 0 {
			sawJudge = true
			judgeEffort = params.ReasoningEffort
			return &llm.GenerateResult{
				FinishReason: llm.FinishReasonStop,
				Text:         `{"verdict":"NO","reason":"query"}`,
			}, nil
		}
		if n == 1 {
			return &llm.GenerateResult{
				FinishReason: llm.FinishReasonToolCalls,
				ToolCalls: []llm.ToolCall{{
					ToolCallID: "c1", ToolName: "post_note", Input: map[string]any{},
				}},
			}, nil
		}
		return &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: "没有发出。"}, nil
	}}
	stage := NewLLMStage("llm", main, LLMConfig{
		Model:           llm.ChatModel("glm-5.3"),
		ReasoningEffort: "medium",
		MaxSteps:        2,
		Tools: []llm.Tool{{
			Name:               "post_note",
			RequiresUserIntent: true,
			Parameters:         map[string]any{"type": "object", "properties": map[string]any{}},
			Execute: func(*llm.ToolExecContext, any) (any, error) {
				return "posted", nil
			},
		}},
	}, nil, zap.NewNop().Sugar())
	env := core.NewEnvelope(core.Message{ID: "m", BotID: "bot", Text: "hello there"})

	if _, err := stage.Process(context.Background(), env); err != nil {
		t.Fatalf("process: %v", err)
	}
	if !sawJudge {
		t.Fatal("intent judge was not called")
	}
	if judgeEffort == nil || *judgeEffort != "high" {
		t.Fatalf("judge effort = %v, want high (glm-5.3 has no medium)", judgeEffort)
	}
}
