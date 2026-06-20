package bot

import (
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestDefaultAgentConfig(t *testing.T) {
	cfg := DefaultAgentConfig()
	if cfg.MaxSteps != 10 {
		t.Errorf("expected MaxSteps=10, got %d", cfg.MaxSteps)
	}
}

func TestAgentConfig_Merge(t *testing.T) {
	base := DefaultAgentConfig()

	temp := 0.5
	other := AgentConfig{
		MaxSteps:    20,
		Temperature: &temp,
	}

	merged := base.Merge(other)
	if merged.MaxSteps != 20 {
		t.Errorf("expected MaxSteps=20, got %d", merged.MaxSteps)
	}
	if merged.Temperature == nil || *merged.Temperature != 0.5 {
		t.Error("expected temperature=0.5")
	}
}

func TestAgentConfig_EffectiveTemperature(t *testing.T) {
	cfg := AgentConfig{}
	botCfg := BotConfig{Temperature: 0.7}

	if cfg.EffectiveTemperature(botCfg) != 0.7 {
		t.Error("expected 0.7 from bot config")
	}

	temp := 0.3
	cfg.Temperature = &temp
	if cfg.EffectiveTemperature(botCfg) != 0.3 {
		t.Error("expected 0.3 from agent config")
	}
}

func TestAgentConfig_EffectiveSystemPrompt(t *testing.T) {
	cfg := AgentConfig{}
	botCfg := BotConfig{SystemPrompt: "default prompt"}

	if cfg.EffectiveSystemPrompt(botCfg) != "default prompt" {
		t.Error("expected default prompt")
	}

	cfg.SystemPromptOverride = "custom prompt"
	if cfg.EffectiveSystemPrompt(botCfg) != "custom prompt" {
		t.Error("expected custom prompt")
	}
}

func TestAgentConfig_FilterTools(t *testing.T) {
	tools := []llm.Tool{
		{Name: "web_search"},
		{Name: "sandbox_exec"},
		{Name: "calculate"},
	}

	// Test blocklist
	cfg := AgentConfig{
		ToolBlocklist: []string{"sandbox_exec"},
	}
	filtered := cfg.FilterTools(tools)
	if len(filtered) != 2 {
		t.Errorf("expected 2 tools, got %d", len(filtered))
	}

	// Test allowlist
	cfg2 := AgentConfig{
		ToolAllowlist: []string{"web_search", "calculate"},
	}
	filtered2 := cfg2.FilterTools(tools)
	if len(filtered2) != 2 {
		t.Errorf("expected 2 tools, got %d", len(filtered2))
	}

	// Test both
	cfg3 := AgentConfig{
		ToolAllowlist: []string{"web_search", "calculate", "sandbox_exec"},
		ToolBlocklist: []string{"sandbox_exec"},
	}
	filtered3 := cfg3.FilterTools(tools)
	if len(filtered3) != 2 {
		t.Errorf("expected 2 tools, got %d", len(filtered3))
	}
	for _, tool := range filtered3 {
		if tool.Name == "sandbox_exec" {
			t.Error("sandbox_exec should be blocked")
		}
	}

	// Test no filters
	cfg4 := AgentConfig{}
	filtered4 := cfg4.FilterTools(tools)
	if len(filtered4) != 3 {
		t.Errorf("expected 3 tools with no filter, got %d", len(filtered4))
	}
}

func TestAgentConfig_ApplyToParams(t *testing.T) {
	temp := 0.5
	topP := 0.9
	effort := "high"
	cfg := AgentConfig{
		Temperature:     &temp,
		TopP:            &topP,
		ReasoningEffort: &effort,
		StopSequences:   []string{"STOP"},
	}

	params := &llm.GenerateParams{}
	cfg.ApplyToParams(params)

	if params.Temperature == nil || *params.Temperature != 0.5 {
		t.Error("expected temperature applied")
	}
	if params.TopP == nil || *params.TopP != 0.9 {
		t.Error("expected topP applied")
	}
	if params.ReasoningEffort == nil || *params.ReasoningEffort != "high" {
		t.Error("expected reasoningEffort applied")
	}
	if len(params.StopSequences) != 1 || params.StopSequences[0] != "STOP" {
		t.Error("expected stopSequences applied")
	}
}
