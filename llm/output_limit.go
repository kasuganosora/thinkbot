package llm

// ============================================================================
// Output limit of internal (non-chat) LLM calls
//
// The authoritative max output of a model is the per-model maxTokens setting
// of the provider config (settings UI → provider → models[].maxTokens, loaded
// as config.ModelDef.MaxTokens; explicit value > model preset > default). The
// normal chat path sends exactly that value. Internal calls (summaries,
// compaction, judges, memory jobs, …) used to send their own hardcoded caps
// instead (compact_context 4096, auto-compaction 4096, engagement judge 100,
// healing diagnosis 2000, vision 1024, …). With thinking models, whose
// reasoning tokens count toward max_tokens, such caps truncate the response
// (2026-09-26: three compact_context summaries on glm-5.3 all stopped at
// exactly 4096 output tokens although the model is configured for 128000).
//
// Rule for every internal call: use the configured model limit; an explicit
// per-feature operator setting may only LOWER it; a built-in fallback is used
// only when no model limit is known.
// ============================================================================

// DefaultMaxOutputTokens is the last-resort output limit when neither the
// model definition nor an operator setting provides one (same value as
// config.fillModelDefaults).
const DefaultMaxOutputTokens = 8192

// ResolveMaxOutputTokens returns the max output tokens for an internal call.
//
//   - modelMax: the configured model limit (ModelDef.MaxTokens); <= 0 = unknown.
//   - operatorCap: an explicit per-feature setting (e.g.
//     agent.self_compact.summary_max_tokens); <= 0 = not set. It can only
//     lower the model limit, never raise it.
//   - fallback: used only when modelMax is unknown (<= 0 → DefaultMaxOutputTokens).
func ResolveMaxOutputTokens(modelMax, operatorCap, fallback int) int {
	if modelMax > 0 {
		if operatorCap > 0 && operatorCap < modelMax {
			return operatorCap
		}
		return modelMax
	}
	if operatorCap > 0 {
		return operatorCap
	}
	if fallback > 0 {
		return fallback
	}
	return DefaultMaxOutputTokens
}

// outputContextMargin is kept free between the estimated prompt and the
// output budget (token estimates are approximate).
const outputContextMargin = 2048

// FitOutputToContext lowers maxOut so that the estimated prompt plus the
// output budget stays within the model's context window (OpenAI-compatible
// providers reject prompt + max_tokens > context). contextLength <= 0 means
// unknown (maxOut unchanged). The result never drops below minOut (if the
// prompt alone nearly fills the window the provider will report it anyway).
func FitOutputToContext(maxOut, contextLength, estInputTokens, minOut int) int {
	if contextLength <= 0 || maxOut <= 0 {
		return maxOut
	}
	room := contextLength - estInputTokens - outputContextMargin
	if room < maxOut {
		maxOut = room
	}
	if maxOut < minOut {
		maxOut = minOut
	}
	return maxOut
}

// maxTokensOf returns *p or 0.
func maxTokensOf(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
