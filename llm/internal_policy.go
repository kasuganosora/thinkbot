package llm

import (
	"regexp"
	"strconv"
	"strings"
)

// ============================================================================
// Per-purpose policy for internal (non-chat) LLM calls
//
// Internal calls (judges, summaries, memory jobs, profilers, dreaming, workflow
// healing, vision) used to send no reasoning_effort at all. GLM-5.3 reasons at
// "max" when the parameter is absent, and since the output limit now follows
// the model's configured maxTokens (128000), those calls had unbounded
// reasoning (2026-09-26: memory_dedup 32 calls, 242K output tokens).
//
// InternalPolicy decides, per call purpose:
//   - reasoning_effort: configurable per purpose (llm.internal_reasoning.<purpose>)
//     with a global default (llm.internal_reasoning.default, default "auto");
//   - max output tokens: the model's configured maxTokens, optionally lowered
//     by llm.internal_max_tokens.<purpose> / .default (0 = model limit).
//
// Values of the reasoning setting:
//   - "" / "auto": the built-in default of the purpose (judges/classifiers/
//     scorers → "none", everything else → "low"), sent ONLY when the provider
//     is known to accept the parameter: the bot already sends a
//     reasoning_effort for normal turns, the model is marked with the
//     "reasoning" capability in the provider config, or the model belongs to a
//     family documented to support it (GLM-5.2+). Otherwise nothing is sent,
//     so models that reject the parameter keep working. "none" is downgraded
//     to "low" unless the model is known to accept it (or the bot itself runs
//     on none/minimal).
//   - "provider" / "off" / "default": never send the parameter.
//   - anything else (e.g. "low", "high", "none"): sent as configured.
// Every value is finally mapped into the vocabulary of known model families
// (GLM-5.3 accepts only low/high/max on the regular API: none/minimal → low,
// medium → high; models below GLM-5.2 do not support the parameter).
// ============================================================================

// Internal call purposes (also the config key suffixes).
const (
	PurposeLazyJudge       = "lazy_judge"
	PurposeEngagementJudge = "engagement_judge"
	PurposeDreamScore      = "dream_score"
	PurposeDreamCluster    = "dream_cluster"
	PurposeDreamExtract    = "dream_extract"
	PurposeMemoryDedup     = "memory_dedup"
	PurposeAutoCompact     = "auto_compact"
	PurposeSummarizeHead   = "summarize_head"
	PurposeUserProfiler    = "user_profiler"
	PurposeBotProfiler     = "bot_profiler"
	PurposeWorkflowHeal    = "workflow_heal"
	PurposeVision          = "vision"
)

// InternalPurposes lists every purpose with its built-in reasoning default.
var InternalPurposes = []struct {
	Name             string
	DefaultReasoning string
	Description      string
}{
	{PurposeLazyJudge, "none", "anti-lazy reply judge (classifier)"},
	{PurposeEngagementJudge, "none", "engagement Tier-2 judge (classifier)"},
	{PurposeDreamScore, "none", "dreaming importance scoring (scorer)"},
	{PurposeDreamCluster, "none", "dreaming theme tagging (classifier)"},
	{PurposeDreamExtract, "low", "dreaming light-phase memory extraction"},
	{PurposeMemoryDedup, "low", "long-term memory cluster merge (memory_dedup)"},
	{PurposeAutoCompact, "low", "automatic conversation compaction summary"},
	{PurposeSummarizeHead, "low", "sliding-window head summary (subagent, /compact)"},
	{PurposeUserProfiler, "low", "user profile extraction"},
	{PurposeBotProfiler, "low", "bot self-profile extraction"},
	{PurposeWorkflowHeal, "low", "workflow healing diagnosis"},
	{PurposeVision, "low", "vision transcription of attachments"},
}

// Reasoning setting values.
const (
	ReasoningAuto     = "auto"
	ReasoningProvider = "provider"
)

// InternalSettings is the operator configuration (read live from the config
// store by the caller). Map keys are purposes; "" / 0 values inherit the
// defaults.
type InternalSettings struct {
	DefaultReasoning string
	Reasoning        map[string]string
	DefaultMaxTokens int
	MaxTokens        map[string]int
}

// InternalPolicy applies InternalSettings for one bot. A nil *InternalPolicy
// is valid: no reasoning_effort is sent and the output limit is the model's.
type InternalPolicy struct {
	settings  func() InternalSettings
	botEffort string
	// reasoningModels: model IDs marked with the "reasoning" capability.
	reasoningModels map[string]bool
}

// NewInternalPolicy creates a policy. settings may be nil (built-in defaults);
// botEffort is the reasoning_effort the bot sends for normal turns ("" = none);
// reasoningModels are model IDs whose provider config lists the "reasoning"
// capability.
func NewInternalPolicy(settings func() InternalSettings, botEffort string, reasoningModels ...string) *InternalPolicy {
	p := &InternalPolicy{settings: settings, botEffort: strings.ToLower(strings.TrimSpace(botEffort))}
	for _, m := range reasoningModels {
		if m = strings.TrimSpace(m); m != "" {
			if p.reasoningModels == nil {
				p.reasoningModels = map[string]bool{}
			}
			p.reasoningModels[m] = true
		}
	}
	return p
}

func (p *InternalPolicy) current() InternalSettings {
	if p == nil || p.settings == nil {
		return InternalSettings{}
	}
	return p.settings()
}

func purposeDefault(purpose string) string {
	for _, ip := range InternalPurposes {
		if ip.Name == purpose {
			return ip.DefaultReasoning
		}
	}
	return "low"
}

// MaxTokens returns the output limit for purpose: the configured model limit
// (modelMax), lowered by an operator cap if one is set; fallback applies only
// when the model limit is unknown.
func (p *InternalPolicy) MaxTokens(purpose string, modelMax, fallback int) int {
	s := p.current()
	opCap := s.MaxTokens[purpose]
	if opCap <= 0 {
		opCap = s.DefaultMaxTokens
	}
	return ResolveMaxOutputTokens(modelMax, opCap, fallback)
}

// ReasoningEffort returns the reasoning_effort to send for purpose on model
// ("" = do not send).
func (p *InternalPolicy) ReasoningEffort(purpose, model string) string {
	if p == nil {
		return ""
	}
	s := p.current()
	configured := strings.ToLower(strings.TrimSpace(s.Reasoning[purpose]))
	if configured == "" {
		configured = strings.ToLower(strings.TrimSpace(s.DefaultReasoning))
	}
	switch configured {
	case ReasoningProvider, "off", "default":
		return ""
	case "", ReasoningAuto:
		return p.autoEffort(purpose, model)
	default:
		return NormalizeReasoningEffort(model, configured)
	}
}

func (p *InternalPolicy) botSendsEffort() bool {
	switch p.botEffort {
	case "", ReasoningProvider, "off", "default", ReasoningAuto:
		return false
	}
	return true
}

func (p *InternalPolicy) autoEffort(purpose, model string) string {
	fam := reasoningFamilyOf(model)
	if fam == familyNoEffort {
		return ""
	}
	if !p.botSendsEffort() && !p.reasoningModels[model] && fam == familyUnknown {
		// Nothing says this provider accepts the parameter: don't send it.
		return ""
	}
	want := purposeDefault(purpose)
	botCheap := p.botEffort == "none" || p.botEffort == "minimal"
	if botCheap && want == "low" {
		want = p.botEffort // the bot already runs cheaper than "low"
	}
	if want == "none" && !familyAcceptsNone(fam) {
		if botCheap {
			want = p.botEffort
		} else {
			want = "low"
		}
	}
	return NormalizeReasoningEffort(model, want)
}

// Apply sets params.ReasoningEffort for purpose (clears it when nothing
// should be sent) and returns the effort ("" = not sent).
func (p *InternalPolicy) Apply(purpose string, params *GenerateParams) string {
	model := ""
	if params.Model != nil {
		model = params.Model.ID
	}
	e := p.ReasoningEffort(purpose, model)
	if e == "" {
		params.ReasoningEffort = nil
	} else {
		params.ReasoningEffort = &e
	}
	return e
}

// ---- model families -------------------------------------------------------

type reasoningFamily int

const (
	familyUnknown  reasoningFamily = iota
	familyNoEffort                 // documented not to support reasoning_effort (GLM < 5.2)
	familyGLM52                    // GLM-5.2: max, xhigh, high, medium, low, minimal, none
	familyGLM53                    // GLM-5.3+: only low, high, max on the regular API
)

var glmVersionRe = regexp.MustCompile(`^glm-(\d+)(?:\.(\d+))?`)

func reasoningFamilyOf(model string) reasoningFamily {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 { // e.g. "z-ai/glm-5.3"
		m = m[i+1:]
	}
	sub := glmVersionRe.FindStringSubmatch(m)
	if sub == nil {
		return familyUnknown
	}
	major, _ := strconv.Atoi(sub[1])
	minor := 0
	if sub[2] != "" {
		minor, _ = strconv.Atoi(sub[2])
	}
	switch {
	case major > 5 || (major == 5 && minor >= 3):
		return familyGLM53
	case major == 5 && minor == 2:
		return familyGLM52
	default:
		return familyNoEffort
	}
}

func familyAcceptsNone(f reasoningFamily) bool { return f == familyGLM52 }

// NormalizeReasoningEffort maps effort into the vocabulary accepted by model
// ("" = do not send). Unknown models get the value unchanged.
//
// GLM (docs.z.ai "Deep Thinking", 2026-09): reasoning_effort is supported by
// GLM-5.2 and above. GLM-5.3 accepts only low/high/max on the regular API
// (other values → HTTP 400; the Coding Plan endpoint maps none/minimal/low →
// low and medium/high → high); GLM-5.2 accepts none..max (none/minimal skip
// thinking, low/medium are served as high).
func NormalizeReasoningEffort(model, effort string) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return ""
	}
	switch reasoningFamilyOf(model) {
	case familyNoEffort:
		return ""
	case familyGLM53:
		switch e {
		case "none", "minimal", "low":
			return "low"
		case "medium", "high":
			return "high"
		case "xhigh", "max":
			return "max"
		default:
			return "low"
		}
	}
	return e
}
