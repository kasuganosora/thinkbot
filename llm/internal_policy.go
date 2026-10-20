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
//     by llm.internal_max_tokens.<purpose> / .default (0 = model limit) and by
//     a built-in purpose cap (memory_dedup: 4096).
//
// Values of the reasoning setting:
//   - "" / "auto": the built-in default of the purpose (judges/classifiers/
//     scorers → "none", memory_dedup → "medium", everything else → "low"),
//     sent ONLY when the provider
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
	PurposeDailyDigest     = "memory_daily_digest"
	PurposeAutoCompact     = "auto_compact"
	PurposeSummarizeHead   = "summarize_head"
	PurposeUserProfiler    = "user_profiler"
	PurposeBotProfiler     = "bot_profiler"
	PurposeWorkflowHeal    = "workflow_heal"
	PurposeVision          = "vision"
	PurposeNotify          = "notify"
)

// InternalPurposes lists every purpose with its built-in reasoning default.
var InternalPurposes = []struct {
	Name             string
	DefaultReasoning string
	Description      string
	// DefaultMaxTokens is the built-in output cap of the purpose (0 = model
	// limit). Operators override it with llm.internal_max_tokens.<purpose>.
	DefaultMaxTokens int
}{
	{PurposeLazyJudge, "none", "anti-lazy reply judge (classifier)", 0},
	{PurposeEngagementJudge, "none", "engagement Tier-2 judge (classifier)", 0},
	{PurposeDreamScore, "none", "dreaming importance scoring (scorer)", 0},
	{PurposeDreamCluster, "none", "dreaming theme tagging (classifier)", 0},
	{PurposeDreamExtract, "low", "dreaming light-phase memory extraction", 0},
	{PurposeMemoryDedup, "medium", "long-term memory cluster merge (memory_dedup); medium by default (low dropped details on 09-27, high cost ~12k output tokens/call); the cluster detail guard stays on", MemoryDedupDefaultMaxTokens},
	{PurposeDailyDigest, "medium", "daily digest of a passive listen scope (e.g. misskey:timeline): one call per scope per day", 0},
	{PurposeAutoCompact, "low", "automatic conversation compaction summary", 0},
	{PurposeSummarizeHead, "low", "sliding-window head summary (subagent, /compact)", 0},
	{PurposeUserProfiler, "low", "user profile extraction", 0},
	{PurposeBotProfiler, "low", "bot self-profile extraction", 0},
	{PurposeWorkflowHeal, "low", "workflow healing diagnosis", 0},
	{PurposeVision, "low", "vision transcription of attachments", 0},
	{PurposeNotify, "low", "notify bot mode: the bot relays an external notification to its owner", 0},
}

// MemoryDedupDefaultMaxTokens is the built-in output cap (reasoning included)
// of one memory_dedup call. Groups are 2-10 entries, so the JSON answer is
// small; the cap bounds reasoning. A cut-off answer leaves the group unmerged.
const MemoryDedupDefaultMaxTokens = 4096

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

func purposeDefaultMaxTokens(purpose string) int {
	for _, ip := range InternalPurposes {
		if ip.Name == purpose {
			return ip.DefaultMaxTokens
		}
	}
	return 0
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
		// No per-purpose setting: the built-in purpose cap, lowered further by
		// the global default cap when that one is smaller.
		opCap = purposeDefaultMaxTokens(purpose)
		if g := s.DefaultMaxTokens; g > 0 && (opCap <= 0 || g < opCap) {
			opCap = g
		}
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
	if botCheap && (want == "low" || want == "medium" || want == "high") {
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
