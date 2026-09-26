package api

import (
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
)

// newInternalPolicy builds the per-purpose policy (reasoning_effort, output
// cap) for a bot's internal LLM calls: judges, compaction summaries,
// memory_dedup, profilers, dreaming, workflow healing and vision.
//
// Settings (llm.internal_reasoning.* / llm.internal_max_tokens.*) are read
// live from the config store on every call. botEffort is the bot's
// reasoning_effort for normal turns: when set, the provider is known to accept
// the parameter. Models whose provider config lists the "reasoning" capability
// are allowed as well.
func newInternalPolicy(store *config.Store, logger *zap.SugaredLogger, botEffort string, bundle *bot.LLMBundle) *llm.InternalPolicy {
	var reasoningModels []string
	if bundle != nil {
		for _, d := range []config.ModelDef{bundle.MainDef, bundle.LightDef, bundle.VisionDef} {
			if d.Reasoning && d.Model != "" {
				reasoningModels = append(reasoningModels, d.Model)
			}
		}
	}
	var settings func() llm.InternalSettings
	if store != nil {
		settings = func() llm.InternalSettings {
			return config.NewBuilder(store, logger).GetInternalLLMSettings()
		}
	}
	return llm.NewInternalPolicy(settings, botEffort, reasoningModels...)
}
