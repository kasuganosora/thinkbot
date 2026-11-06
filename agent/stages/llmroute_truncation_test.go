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
	cases := []struct {
		in   string
		want string
	}{
		{"medium", route.LightReasoningEffort},
		{"high", route.LightReasoningEffort},
		{"max", route.LightReasoningEffort},
		{"low", route.LightReasoningEffort},
		{"minimal", route.LightReasoningEffort},
		{"  HIGH  ", route.LightReasoningEffort},
		{"", ""},
		{"none", "none"},
		{"off", "off"},
		{"default", "default"},
		{llm.ReasoningProvider, llm.ReasoningProvider},
	}
	for _, c := range cases {
		if got := lightTierEffort(c.in); got != c.want {
			t.Errorf("lightTierEffort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The bug this guards against: a "medium" bot setting was normalized through
// the main model's low/high/max vocabulary (-> "high") and then sent to the
// light model, which spent its whole 1024-token output budget on thinking.
func TestReplyEffort_LightTierVocabulary(t *testing.T) {
	mainModel := llm.ChatModel("deepseek-flash")
	lightModel := llm.ChatModel("glm-5.2")

	if got := replyEffort("medium", mainModel); got == nil || *got != "high" {
		t.Fatalf("main model effort = %v, want high (unchanged default-tier behavior)", got)
	}
	got := replyEffort(lightTierEffort("medium"), lightModel)
	if got == nil || *got != route.LightReasoningEffort {
		t.Fatalf("light tier effort = %v, want %q", got, route.LightReasoningEffort)
	}
	if got := replyEffort(lightTierEffort("none"), lightModel); got == nil || *got != "none" {
		t.Fatalf("explicit off must be preserved, got %v", got)
	}
}

func TestLightTierExhausted(t *testing.T) {
	cases := []struct {
		name  string
		light bool
		res   *llm.GenerateResult
		want  bool
	}{
		{"exhausted light turn", true, &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: ""}, true},
		{"whitespace only", true, &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: " \n "}, true},
		{"partial reply is not exhaustion", true, &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: "half"}, false},
		{"clean stop with empty text", true, &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: ""}, false},
		{"full tier not eligible", false, &llm.GenerateResult{FinishReason: llm.FinishReasonLength, Text: ""}, false},
		{"nil result", true, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lightTierExhausted(c.light, c.res); got != c.want {
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
	if gotEffort == nil || *gotEffort != route.LightReasoningEffort {
		t.Fatalf("light tier effort = %v, want %q", gotEffort, route.LightReasoningEffort)
	}
	if gotMaxTokens == nil || *gotMaxTokens != route.LightMaxTokens {
		t.Fatalf("light tier max tokens = %v, want %d", gotMaxTokens, route.LightMaxTokens)
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
