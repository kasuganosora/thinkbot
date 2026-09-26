package memory

import (
	"github.com/kasuganosora/thinkbot/llm"
	"go.uber.org/zap"
)

// applyInternalCall sets the output limit and reasoning_effort of an internal
// memory/dreaming LLM call from the per-purpose policy: the output limit is the
// configured model limit (modelMax; DefaultGenerationMaxTokens only when
// unknown), optionally lowered by llm.internal_max_tokens.<purpose>; the
// reasoning_effort follows llm.internal_reasoning.<purpose>. A nil policy
// sends no reasoning_effort. Returns the max tokens used.
func applyInternalCall(policy *llm.InternalPolicy, purpose string, params *llm.GenerateParams, modelMax int) int {
	mt := policy.MaxTokens(purpose, modelMax, DefaultGenerationMaxTokens)
	params.MaxTokens = &mt
	policy.Apply(purpose, params)
	return mt
}

// warnIfTruncated logs a response that stopped at the output limit (the JSON
// it carries is usually incomplete, so callers fall back). Returns true when
// truncated.
func warnIfTruncated(logger *zap.SugaredLogger, purpose string, res *llm.GenerateResult, params llm.GenerateParams) bool {
	if res == nil || res.FinishReason != llm.FinishReasonLength {
		return false
	}
	if logger != nil {
		effort := ""
		if params.ReasoningEffort != nil {
			effort = *params.ReasoningEffort
		}
		maxTok := 0
		if params.MaxTokens != nil {
			maxTok = *params.MaxTokens
		}
		logger.Warnw("internal LLM call cut off by the output limit (finish_reason=length)",
			"purpose", purpose, "max_tokens", maxTok, "output_tokens", res.Usage.OutputTokens,
			"reasoning_effort", effort)
	}
	return true
}
