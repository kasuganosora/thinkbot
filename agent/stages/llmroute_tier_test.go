package stages

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/route"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
)

type tierProvider struct {
	name  string
	mu    sync.Mutex
	calls int
	tools [][]string
	reply func(n int, params llm.GenerateParams) (*llm.GenerateResult, error)
}

func (p *tierProvider) Name() string { return p.name }

func (p *tierProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	names := make([]string, 0, len(params.Tools))
	for _, tool := range params.Tools {
		names = append(names, tool.Name)
	}
	p.tools = append(p.tools, names)
	p.mu.Unlock()
	return p.reply(n, params)
}

func (p *tierProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("not used")
}

func (p *tierProvider) toolNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tools) == 0 {
		return nil
	}
	return p.tools[0]
}

func TestLLMStage_LightTierNarrowsThenUpgrades(t *testing.T) {
	light := &tierProvider{
		name: "light",
		reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
			return &llm.GenerateResult{
				FinishReason: llm.FinishReasonToolCalls,
				ToolCalls: []llm.ToolCall{{
					ToolCallID: "c1", ToolName: route.ToolUpgrade, Input: map[string]any{},
				}},
			}, nil
		},
	}
	main := &tierProvider{
		name: "main",
		reply: func(int, llm.GenerateParams) (*llm.GenerateResult, error) {
			return &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: "full answer"}, nil
		},
	}
	stage := NewLLMStage("llm", main, LLMConfig{
		Model:         llm.ChatModel("main-model"),
		LightProvider: light,
		LightModel:    llm.ChatModel("light-model"),
		MaxSteps:      2,
		HardMaxSteps:  2,
		Tools: []llm.Tool{
			{Name: "memory", Execute: func(*llm.ToolExecContext, any) (any, error) { return "ok", nil }},
			{Name: "now", Execute: func(*llm.ToolExecContext, any) (any, error) { return "ok", nil }},
			{Name: "sandbox_exec", Execute: func(*llm.ToolExecContext, any) (any, error) { return "ok", nil }},
		},
	}, nil, zap.NewNop().Sugar())
	env := core.NewEnvelope(core.Message{ID: "m", BotID: "bot", Text: "fix the build"})
	route.SetTier(env, route.IntentChat, route.ModelLight)

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if light.calls != 1 {
		t.Fatalf("light calls = %d, want 1", light.calls)
	}
	names := map[string]bool{}
	for _, name := range light.toolNames() {
		names[name] = true
	}
	if !names["memory"] || !names["now"] || !names[route.ToolUpgrade] {
		t.Fatalf("light tools = %#v", names)
	}
	if names["sandbox_exec"] || names["exec"] {
		t.Fatal("light tier kept a heavy tool")
	}
	if main.calls != 1 {
		t.Fatalf("main calls = %d, want 1", main.calls)
	}
	mainNames := map[string]bool{}
	for _, name := range main.toolNames() {
		mainNames[name] = true
	}
	// The orchestrator strips the sandbox_ prefix before the provider sees the tool.
	if !mainNames["sandbox_exec"] && !mainNames["exec"] {
		t.Fatalf("upgraded turn lost the heavy tool: %#v", mainNames)
	}
	actions := out.Actions()
	if len(actions) != 1 || actions[0].Payload != "full answer" {
		t.Fatalf("actions = %#v", actions)
	}
}

func TestLLMStage_LightTierUsesLightSnapshotLimits(t *testing.T) {
	var gotMax int
	var gotTemp float64
	var gotModel string
	lightTemp := 0.2
	mainTemp := 0.9
	light := &tierProvider{
		name: "light",
		reply: func(_ int, params llm.GenerateParams) (*llm.GenerateResult, error) {
			if params.MaxTokens != nil {
				gotMax = *params.MaxTokens
			}
			if params.Temperature != nil {
				gotTemp = *params.Temperature
			}
			if params.Model != nil {
				gotModel = params.Model.ID
			}
			return &llm.GenerateResult{FinishReason: llm.FinishReasonStop, Text: "short"}, nil
		},
	}
	mainMax := 8000
	stage := NewLLMStage("llm", light, LLMConfig{
		Model:         llm.ChatModel("glm-5.2"),
		LightProvider: light,
		LightModel:    llm.ChatModel("glm-5.2"),
		Temperature:   &mainTemp,
		MaxTokens:     &mainMax,
		MaxSteps:      1,
		HardMaxSteps:  1,
	}, nil, zap.NewNop().Sugar())
	env := core.NewEnvelope(core.Message{ID: "m", BotID: "bot", Text: "hi"})
	route.SetTier(env, route.IntentChat, route.ModelLight)
	snap := &bot.LLMSnapshot{
		MainModel:  llm.ChatModel("glm-5.2"),
		MainDef:    config.ModelDef{Model: "glm-5.2", MaxTokens: 8000, Temperature: &mainTemp},
		Light:      light,
		LightModel: llm.ChatModel("glm-5.3"),
		LightDef:   config.ModelDef{Model: "glm-5.3", MaxTokens: 256, Temperature: &lightTemp},
	}
	ctx := bot.WithLLMSnapshot(context.Background(), snap)
	if _, err := stage.Process(ctx, env); err != nil {
		t.Fatal(err)
	}
	if gotModel != "glm-5.3" || gotMax != 256 || gotTemp != 0.2 {
		t.Fatalf("light call model %s max %d temp %v", gotModel, gotMax, gotTemp)
	}
}
