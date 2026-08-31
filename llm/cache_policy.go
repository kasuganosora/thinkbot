package llm

// ============================================================================
// Prompt Caching Policy (P1)
//
// This module implements provider-aware prompt caching, following this
// strategy:
//
//   - Anthropic / Bedrock / Alibaba: explicit cache breakpoints
//     (cache_control: { type: "ephemeral" }) on system + last messages.
//     Anthropic allows up to 4 breakpoints per request.
//
//   - OpenAI / Azure / Copilot: implicit prefix caching — no explicit
//     breakpoints needed. The cache key hint (promptCacheKey) can be set
//     via GenerateParams.CacheKey (typically the session ID). store=false
//     should be set by the adapter to avoid storing conversations.
//
//   - Google / Gemini: implicit caching — handled automatically by the API.
//     No explicit markers are needed.
//
// Breakpoint placement strategy (Anthropic):
//   1. System prompt   — breakpoint on the system block
//   2. Tools block     — breakpoint on the last tool definition
//   3. Last user msg   — breakpoint on the latest user message
//   (up to MaxCacheBreakpoints total, leaving room for manual placement)
//
// This follows the default "auto" strategy:
//   { tools: true, system: true, messages: "latest-user-message" }
// which uses at most 3 breakpoints, leaving the 4th slot available.
//
// This maximises cache hits for the stable prefix while staying under the cap.
// ============================================================================

// CachePolicy controls how cache breakpoints are placed.
type CachePolicy string

const (
	// CachePolicyNone strips all existing CacheControl markers and does not
	// add new ones.
	CachePolicyNone CachePolicy = "none"
	// CachePolicyAuto automatically places breakpoints on the stable prefix
	// (system, tools, last messages), enforcing the provider's max-breakpoint
	// limit.
	CachePolicyAuto CachePolicy = "auto"
)

// MaxCacheBreakpoints is the maximum number of cache breakpoints Anthropic
// allows in a single request.
const MaxCacheBreakpoints = 4

// applyAutoCachePolicy places cache breakpoints on the stable prefix of a
// GenerateParams in-place, following the "auto" strategy.
//
// Priority order (first MaxCacheBreakpoints that qualify are kept):
//  1. System prompt   — marked via a sentinel (adapter reads it)
//  2. Tools           — last tool gets the breakpoint
//  3. Last user msg   — text part of the latest user message
//
// This matches the default auto strategy: at most 3 breakpoints,
// leaving the 4th slot available for manual placement.
func applyAutoCachePolicy(params *GenerateParams) {
	breakpoints := 0

	// 1. System prompt — set cache_control on the system text part.
	//    The Anthropic adapter checks for this sentinel to decide whether
	//    to use SystemTextWithCache.
	if params.System != "" && breakpoints < MaxCacheBreakpoints {
		params.SystemCacheControl = EphemeralCacheControl()
		breakpoints++
	}

	// 2. Tools — cache the tools block if there are tools.
	if len(params.Tools) > 0 && breakpoints < MaxCacheBreakpoints {
		params.Tools[len(params.Tools)-1].CacheControl = EphemeralCacheControl()
		breakpoints++
	}

	// 3. Last user message — set cache control on the text part of the
	//    latest user message. This is the most cache-efficient placement
	//    for tool-use loops: the user's original prompt stays put while
	//    assistant/tool messages grow below it.
	idx := findLastUserMessageIndex(params.Messages)
	if idx >= 0 && breakpoints < MaxCacheBreakpoints {
		msg := &params.Messages[idx]
		setCacheOnLastTextPart(msg)
	}
}

// ApplyCachePolicy applies the given cache policy to GenerateParams in-place.
// It first strips existing cache markers (except when policy is "none" and
// already empty), then places breakpoints per the policy.
func ApplyCachePolicy(params *GenerateParams, policy CachePolicy) {
	switch policy {
	case CachePolicyNone:
		stripAllCacheControl(params)
	case CachePolicyAuto:
		stripAllCacheControl(params)
		applyAutoCachePolicy(params)
	}
}

// ShouldApplyCacheBreakpoints returns true if the provider benefits from
// explicit cache breakpoints (Anthropic family). OpenAI and Google have
// implicit prefix caching and do not need explicit markers.
func ShouldApplyCacheBreakpoints(providerName string) bool {
	switch providerName {
	case "anthropic", "bedrock", "alibaba", "google-vertex-anthropic":
		return true
	}
	return false
}

// applyProviderCachePolicy applies the appropriate cache policy to the params
// based on the provider name. If params.CachePolicy is explicitly set, it is
// honored; otherwise a sensible default is chosen:
//   - Anthropic-family providers: CachePolicyAuto (explicit breakpoints)
//   - All other providers: no explicit breakpoints (implicit prefix caching)
func applyProviderCachePolicy(params *GenerateParams, providerName string) {
	policy := params.CachePolicy
	if policy == "" {
		if ShouldApplyCacheBreakpoints(providerName) {
			policy = CachePolicyAuto
		} else {
			policy = CachePolicyNone
		}
	}
	ApplyCachePolicy(params, policy)
}

// findLastUserMessageIndex returns the index of the last user message,
// or -1 if none exists.
func findLastUserMessageIndex(msgs []Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == MessageRoleUser {
			return i
		}
	}
	return -1
}

func setCacheOnLastTextPart(msg *Message) {
	for i := len(msg.Content) - 1; i >= 0; i-- {
		if _, ok := msg.Content[i].(TextPart); ok {
			tp := msg.Content[i].(TextPart)
			tp.CacheControl = EphemeralCacheControl()
			msg.Content[i] = tp
			return
		}
	}
}

func hasCacheControl(msg *Message) bool {
	for _, part := range msg.Content {
		switch p := part.(type) {
		case TextPart:
			if p.CacheControl != nil {
				return true
			}
		case ImagePart:
			if p.CacheControl != nil {
				return true
			}
		case FilePart:
			if p.CacheControl != nil {
				return true
			}
		}
	}
	return false
}

func stripAllCacheControl(params *GenerateParams) {
	// Strip system cache control.
	params.SystemCacheControl = nil

	// Strip from tools.
	for i := range params.Tools {
		params.Tools[i].CacheControl = nil
	}

	// Strip from messages.
	for i := range params.Messages {
		msg := &params.Messages[i]
		for j := range msg.Content {
			switch p := msg.Content[j].(type) {
			case TextPart:
				p.CacheControl = nil
				msg.Content[j] = p
			case ImagePart:
				p.CacheControl = nil
				msg.Content[j] = p
			case FilePart:
				p.CacheControl = nil
				msg.Content[j] = p
			}
		}
	}
}
