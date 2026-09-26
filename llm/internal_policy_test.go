package llm

import "testing"

func TestNormalizeReasoningEffort(t *testing.T) {
	cases := []struct{ model, in, want string }{
		{"glm-5.3", "none", "low"}, // GLM-5.3 regular API: only low/high/max
		{"glm-5.3", "minimal", "low"},
		{"glm-5.3", "low", "low"},
		{"glm-5.3", "medium", "high"},
		{"glm-5.3", "max", "max"},
		{"glm-5.3-flash", "none", "low"},
		{"z-ai/glm-5.3", "none", "low"},
		{"glm-5.2", "none", "none"}, // GLM-5.2 accepts none (skips thinking)
		{"glm-5.2", "low", "low"},
		{"glm-5.1", "low", ""}, // below 5.2: parameter not supported
		{"glm-4.7", "low", ""},
		{"glm-5", "low", ""},
		{"gpt-4o", "none", "none"}, // unknown: unchanged
		{"glm-5.3", "", ""},
	}
	for _, c := range cases {
		if got := NormalizeReasoningEffort(c.model, c.in); got != c.want {
			t.Errorf("Normalize(%q,%q)=%q, want %q", c.model, c.in, got, c.want)
		}
	}
}

func TestInternalPolicy_AutoReasoningEffort(t *testing.T) {
	cases := []struct {
		name, botEffort, model, purpose string
		reasoningModels                 []string
		want                            string
	}{
		// prod: bot 栞娜 on reasoning_effort=medium, main glm-5.3, light glm-5.2
		{"prod dedup", "medium", "glm-5.3", PurposeMemoryDedup, nil, "low"},
		{"prod auto-compact", "medium", "glm-5.3", PurposeAutoCompact, nil, "low"},
		{"prod dream_score (none unsupported on 5.3)", "medium", "glm-5.3", PurposeDreamScore, nil, "low"},
		{"prod lazy judge on light glm-5.2", "medium", "glm-5.2", PurposeLazyJudge, nil, "none"},
		{"prod engagement judge on light glm-5.2", "medium", "glm-5.2", PurposeEngagementJudge, nil, "none"},
		{"prod vision", "medium", "glm-5.3", PurposeVision, nil, "low"},
		{"prod heal", "medium", "glm-5.3", PurposeWorkflowHeal, nil, "low"},
		// bot does not use reasoning_effort, model unknown → never send
		{"unknown model, bot off", "", "gpt-4o", PurposeMemoryDedup, nil, ""},
		{"unknown model, bot off, judge", "", "gpt-4o", PurposeLazyJudge, nil, ""},
		// model def marks the reasoning capability → send; none downgraded to low
		{"reasoning capability", "", "o4-mini", PurposeMemoryDedup, []string{"o4-mini"}, "low"},
		{"reasoning capability judge", "", "o4-mini", PurposeLazyJudge, []string{"o4-mini"}, "low"},
		// documented family (GLM-5.2+) → send even when the bot sends nothing
		{"glm-5.3 bot off", "", "glm-5.3", PurposeMemoryDedup, nil, "low"},
		{"glm-5.2 bot off judge", "", "glm-5.2", PurposeDreamScore, nil, "none"},
		// GLM below 5.2 does not support the parameter
		{"glm-5.1", "medium", "glm-5.1", PurposeMemoryDedup, nil, ""},
		// unknown model, bot cheaper than low → keep the bot's value
		{"bot minimal", "minimal", "gpt-5", PurposeMemoryDedup, nil, "minimal"},
		{"bot minimal judge", "minimal", "gpt-5", PurposeLazyJudge, nil, "minimal"},
		{"bot high unknown judge", "high", "gpt-5", PurposeLazyJudge, nil, "low"},
		{"bot provider", "provider", "gpt-4o", PurposeMemoryDedup, nil, ""},
	}
	for _, c := range cases {
		p := NewInternalPolicy(nil, c.botEffort, c.reasoningModels...)
		if got := p.ReasoningEffort(c.purpose, c.model); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	var nilPol *InternalPolicy
	if nilPol.ReasoningEffort(PurposeMemoryDedup, "glm-5.3") != "" {
		t.Error("nil policy must not send reasoning_effort")
	}
}

func TestInternalPolicy_ConfiguredValues(t *testing.T) {
	set := InternalSettings{
		DefaultReasoning: "auto",
		Reasoning: map[string]string{
			PurposeMemoryDedup: "provider", // never send
			PurposeDreamScore:  "none",     // explicit
			PurposeBotProfiler: "high",
		},
		DefaultMaxTokens: 0,
		MaxTokens:        map[string]int{PurposeMemoryDedup: 16000},
	}
	p := NewInternalPolicy(func() InternalSettings { return set }, "medium")
	if got := p.ReasoningEffort(PurposeMemoryDedup, "glm-5.3"); got != "" {
		t.Errorf("provider → not sent, got %q", got)
	}
	if got := p.ReasoningEffort(PurposeDreamScore, "glm-5.3"); got != "low" {
		t.Errorf("explicit none on glm-5.3 → low, got %q", got)
	}
	if got := p.ReasoningEffort(PurposeDreamScore, "gpt-5"); got != "none" {
		t.Errorf("explicit none on unknown model → none, got %q", got)
	}
	if got := p.ReasoningEffort(PurposeBotProfiler, "glm-5.3"); got != "high" {
		t.Errorf("explicit high, got %q", got)
	}
	if got := p.ReasoningEffort(PurposeUserProfiler, "glm-5.3"); got != "low" {
		t.Errorf("inherits auto default, got %q", got)
	}
	// global default = provider → nothing sent unless a purpose overrides
	set.DefaultReasoning = "provider"
	if got := p.ReasoningEffort(PurposeUserProfiler, "glm-5.3"); got != "" {
		t.Errorf("global provider → not sent, got %q", got)
	}
	if got := p.ReasoningEffort(PurposeBotProfiler, "glm-5.3"); got != "high" {
		t.Errorf("purpose override wins, got %q", got)
	}

	// output caps: per purpose > default > model limit; cap only lowers
	if got := p.MaxTokens(PurposeMemoryDedup, 128000, 8192); got != 16000 {
		t.Errorf("purpose cap: %d", got)
	}
	if got := p.MaxTokens(PurposeUserProfiler, 128000, 8192); got != 128000 {
		t.Errorf("no cap → model limit: %d", got)
	}
	set.DefaultMaxTokens = 32000
	if got := p.MaxTokens(PurposeUserProfiler, 128000, 8192); got != 32000 {
		t.Errorf("default cap: %d", got)
	}
	if got := p.MaxTokens(PurposeUserProfiler, 8192, 8192); got != 8192 {
		t.Errorf("cap never raises: %d", got)
	}
	var nilPol *InternalPolicy
	if got := nilPol.MaxTokens(PurposeVision, 0, 1024); got != 1024 {
		t.Errorf("nil policy fallback: %d", got)
	}
}

func TestInternalPolicy_ApplyClearsWhenNotSent(t *testing.T) {
	stale := "max"
	params := GenerateParams{Model: ChatModel("gpt-4o"), ReasoningEffort: &stale}
	NewInternalPolicy(nil, "").Apply(PurposeMemoryDedup, &params)
	if params.ReasoningEffort != nil {
		t.Fatalf("not sent → must be cleared, got %v", *params.ReasoningEffort)
	}
	params.Model = ChatModel("glm-5.3")
	if e := NewInternalPolicy(nil, "medium").Apply(PurposeMemoryDedup, &params); e != "low" || *params.ReasoningEffort != "low" {
		t.Fatalf("got %q", e)
	}
}

func TestInternalPurposes_Defaults(t *testing.T) {
	want := map[string]string{
		PurposeLazyJudge: "none", PurposeEngagementJudge: "none", PurposeDreamScore: "none", PurposeDreamCluster: "none",
		PurposeDreamExtract: "low", PurposeMemoryDedup: "low", PurposeAutoCompact: "low", PurposeSummarizeHead: "low",
		PurposeUserProfiler: "low", PurposeBotProfiler: "low", PurposeWorkflowHeal: "low", PurposeVision: "low",
		PurposeNotify: "low",
	}
	if len(InternalPurposes) != len(want) {
		t.Fatalf("purposes: %d, want %d", len(InternalPurposes), len(want))
	}
	for _, p := range InternalPurposes {
		if want[p.Name] != p.DefaultReasoning {
			t.Errorf("%s default %q, want %q", p.Name, p.DefaultReasoning, want[p.Name])
		}
	}
}
