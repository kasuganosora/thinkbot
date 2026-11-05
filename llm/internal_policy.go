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
//     a built-in purpose cap (memory_dedup: 12288).
//
// Values of the reasoning setting:
//   - "" / "auto": the built-in default of the purpose (judges/classifiers/
//     scorers → "none", memory_dedup → "medium", everything else → "low"),
//     sent ONLY when the provider
//     is known to accept the parameter: the bot already sends a
//     reasoning_effort for normal turns, the model is marked with the
//     "reasoning" capability in the provider config, or the model belongs to a
//     family documented to support it (GLM-5.2+, DeepSeek V4). Otherwise
//     nothing is sent, so models that reject the parameter keep working.
//     "none" is downgraded to "low" unless the model is known to accept it
//     (or the bot itself runs on none/minimal).
//   - "provider" / "off" / "default": never send the parameter.
//   - anything else: parsed into one of the canonical levels (see
//     CanonicalReasoningLevels) and then mapped into the vocabulary of the
//     target model family. Families with a reduced vocabulary (GLM-5.3+,
//     DeepSeek V4: low/high/max only) round each level onto their own
//     strings; models below GLM-5.2 do not support the parameter.
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
	PurposeMemoryRecall    = "memory_recall"
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
	{PurposeMemoryRecall, "low", "on-demand related-memory recall inside memory search (keyword expansion and candidate refine); runs only when the model calls search without a query or with deep set", 1024},
}

// MemoryDedupDefaultMaxTokens is the built-in output cap (reasoning included)
// of one memory_dedup call. Groups are 2-10 entries, so the JSON answer is
// small; the cap bounds reasoning. 12288 rather than 4096: GLM-5.3 serves the
// medium default as high and reasoning tokens count against the cap (observed
// ~11.9k output tokens/call). A cut-off answer leaves the group unmerged and is
// counted in the memory_dedup_stats log line.
const MemoryDedupDefaultMaxTokens = 12288

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

// ---- canonical reasoning levels -------------------------------------------

// reasoningLevel is the project-wide vocabulary for thinking effort. Every
// configured value (and every provider-specific alias) is first parsed into one
// of these levels; each model family then maps the level onto the strings its
// API actually accepts. Adding a provider means declaring which vocabulary
// shape it uses, not sprinkling per-provider branches through the call sites.
type reasoningLevel int

const (
	levelOff     reasoningLevel = iota // 关闭推理
	levelMinimal                       // 极低
	levelLow                           // 低
	levelMedium                        // 中
	levelHigh                          // 高
	levelMax                           // 超
)

// CanonicalReasoningLevels lists the unified levels, highest first (超/高/中/
// 低/极低, plus 关闭). Config validation reads it through IsReasoningLevel and
// the Web options mirror it, so both stay in sync with this single definition.
var CanonicalReasoningLevels = []string{"max", "high", "medium", "low", "minimal", "none"}

// IsReasoningLevel reports whether v is one of the canonical levels. Provider
// aliases (e.g. xhigh) are accepted by parseReasoningLevel but are not valid
// configuration values.
func IsReasoningLevel(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	for _, lv := range CanonicalReasoningLevels {
		if s == lv {
			return true
		}
	}
	return false
}

// parseReasoningLevel maps a configured value or a provider alias onto a
// canonical level. Aliases for the same semantic level converge here:
// max/xhigh → 超, high → 高, medium → 中, low → 低, minimal → 极低, none → 关闭.
func parseReasoningLevel(v string) (reasoningLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "none":
		return levelOff, true
	case "minimal":
		return levelMinimal, true
	case "low":
		return levelLow, true
	case "medium":
		return levelMedium, true
	case "high":
		return levelHigh, true
	case "max", "xhigh":
		return levelMax, true
	default:
		return levelLow, false
	}
}

// reasoningVocabulary maps canonical levels onto the strings a family accepts.
type reasoningVocabulary map[reasoningLevel]string

// vocabLowHighMax is the three-step vocabulary shared by GLM-5.3+ (regular
// API) and DeepSeek V4 (effort.supported_levels): 关闭/极低/低 → low, 中/高 →
// high, 超 → max. Neither family documents a "no thinking" value, so 关闭 also
// lands on low.
var vocabLowHighMax = reasoningVocabulary{
	levelOff:     "low",
	levelMinimal: "low",
	levelLow:     "low",
	levelMedium:  "high",
	levelHigh:    "high",
	levelMax:     "max",
}

// ---- model families -------------------------------------------------------

// reasoningFamily classifies a model by the shape of the reasoning_effort
// vocabulary it accepts, not by vendor. A new provider is mapped onto one of
// these shapes in reasoningFamilyOf.
//
// Known provider vocabularies, all expressed in canonical levels:
//
//	GLM-5.2       none, minimal, low, medium, high, xhigh, max → familyFull
//	GLM-5.3+ API  low, high, max                              → familyLowHighMax
//	DeepSeek V4   low, high, max                              → familyLowHighMax
//	OpenAI-style  minimal, low, medium, high                  → familyUnknown (unchanged)
type reasoningFamily int

const (
	familyUnknown    reasoningFamily = iota
	familyNoEffort                   // 不支持 reasoning_effort (GLM < 5.2)
	familyFull                       // 接受完整规范档位 (GLM-5.2)
	familyLowHighMax                 // 只接受 low / high / max (GLM-5.3+, DeepSeek V4)
)

var glmVersionRe = regexp.MustCompile(`^glm-(\d+)(?:\.(\d+))?`)

func reasoningFamilyOf(model string) reasoningFamily {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 { // e.g. "z-ai/glm-5.3"
		m = m[i+1:]
	}
	// DeepSeek V4 declares effort.supported_levels = [low, high, max].
	if strings.HasPrefix(m, "deepseek-") {
		return familyLowHighMax
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
		return familyLowHighMax
	case major == 5 && minor == 2:
		return familyFull
	default:
		return familyNoEffort
	}
}

// familyAcceptsNone reports whether the family documents a "no thinking"
// value. Only the full vocabulary has none; three-step vocabularies downgrade
// it to low.
func familyAcceptsNone(f reasoningFamily) bool { return f == familyFull }

// vocabularyOf returns the mapping a family needs, or nil when the value can
// be sent unchanged (unknown models, or the full vocabulary).
func vocabularyOf(f reasoningFamily) reasoningVocabulary {
	switch f {
	case familyLowHighMax:
		return vocabLowHighMax
	default:
		return nil
	}
}

// NormalizeReasoningEffort maps effort into the vocabulary accepted by model
// ("" = do not send). The value is first parsed into a canonical level; a
// family with a reduced vocabulary then maps that level onto its own strings.
// Unknown models and the full vocabulary keep the value unchanged.
//
// GLM (docs.z.ai "Deep Thinking", 2026-09): reasoning_effort is supported by
// GLM-5.2 and above. GLM-5.3 accepts only low/high/max on the regular API
// (other values → HTTP 400; the Coding Plan endpoint maps none/minimal/low →
// low and medium/high → high); GLM-5.2 accepts none..max (none/minimal skip
// thinking, low/medium are served as high).
// DeepSeek V4 declares effort.supported_levels = [low, high, max] on its model
// listing, so it shares the same three-step vocabulary.
func NormalizeReasoningEffort(model, effort string) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return ""
	}
	fam := reasoningFamilyOf(model)
	if fam == familyNoEffort {
		return ""
	}
	vocab := vocabularyOf(fam)
	if vocab == nil {
		return e
	}
	level, ok := parseReasoningLevel(e)
	if !ok {
		level = levelLow
	}
	return vocab[level]
}
