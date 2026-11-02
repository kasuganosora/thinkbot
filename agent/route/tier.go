package route

import (
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

const (
	// IntentChat is a casual turn. It is an execution tier on an opened
	// pipeline, not a third interaction mode.
	IntentChat     = "chat"
	IntentProgress = "progress"
	IntentTask     = "task"
	IntentFollowup = "followup"

	ModelLight   = "light"
	ModelDefault = "default"

	KVLevel      = "route.level"
	KVIntent     = "route.intent"
	KVModel      = "route.model"
	KVStatus     = "route.status"
	KVUpgrade    = "route.upgrade"
	KVUpgraded   = "route.upgraded"
	KVDropReason = "route.drop_reason"

	LevelL1 = "l1"
	LevelL2 = "l2"

	DropReasonPreempt = "l1-preempt"
	DropReasonCancel  = "l1-cancel"
	DropReasonRefuse  = "l1-refuse"

	ToolUpgrade = "route_upgrade"

	// LightMaxTokens caps a light-tier execution so a casual turn cannot
	// inherit the main model's output budget.
	LightMaxTokens = 1024
)

// lightAllow is the tool surface of a light-tier execution pipeline.
// Heavy tools stay on the default tier; the model can ask to upgrade.
var lightAllow = map[string]struct{}{
	"memory":        {},
	"now":           {},
	"datetime_calc": {},
	"calculate":     {},
	"random":        {},
	"uuid":          {},
	"text_hash":     {},
	"text_encode":   {},
	"text_diff":     {},
	"text_stats":    {},
}

// NarrowTools keeps the light-tier allowlist and drops everything else.
func NarrowTools(tools []llm.Tool) []llm.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]llm.Tool, 0, len(tools))
	for _, tool := range tools {
		if _, ok := lightAllow[tool.Name]; ok {
			out = append(out, tool)
		}
	}
	return out
}

// SetTier records the execution tier chosen by L1. Empty values fall back
// to a normal task on the default model.
func SetTier(env *core.Envelope, intent, model string) {
	if env == nil {
		return
	}
	intent, model = normalizeTier(intent, model)
	env.Set(KVIntent, intent)
	env.Set(KVModel, model)
}

func normalizeTier(intent, model string) (string, string) {
	switch intent {
	case IntentChat, IntentProgress, IntentTask, IntentFollowup:
	default:
		intent = IntentTask
	}
	switch model {
	case ModelLight, ModelDefault:
	default:
		if intent == IntentChat || intent == IntentProgress {
			model = ModelLight
		} else {
			model = ModelDefault
		}
	}
	return intent, model
}

// IntentOf returns the execution intent stamped on env, or task.
func IntentOf(env *core.Envelope) string {
	if env == nil {
		return IntentTask
	}
	if v, ok := env.Get(KVIntent); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return IntentTask
}

// ModelOf returns the execution model tier stamped on env, or default.
func ModelOf(env *core.Envelope) string {
	if env == nil {
		return ModelDefault
	}
	if v, ok := env.Get(KVModel); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ModelDefault
}

// SetLevel marks the envelope as an L1 or L2 execution.
func SetLevel(env *core.Envelope, level string) {
	if env == nil {
		return
	}
	env.Set(KVLevel, level)
}

// LevelOf returns the pipeline level stamped on env.
func LevelOf(env *core.Envelope) string {
	if env == nil {
		return ""
	}
	v, _ := env.Get(KVLevel)
	s, _ := v.(string)
	return s
}

// WantsLight reports whether this execution should use the light tier.
// A turn that already upgraded stays on the full tier.
func WantsLight(env *core.Envelope) bool {
	return ModelOf(env) == ModelLight && !Upgraded(env)
}

// UpgradeRequested reports that the light-tier model asked for the full tier.
func UpgradeRequested(env *core.Envelope) bool {
	if env == nil {
		return false
	}
	v, ok := env.Get(KVUpgrade)
	b, ok2 := v.(bool)
	return ok && ok2 && b
}

// MarkUpgraded records that this turn already left the light tier.
func MarkUpgraded(env *core.Envelope) {
	if env == nil {
		return
	}
	env.Set(KVUpgraded, true)
}

// Upgraded reports MarkUpgraded.
func Upgraded(env *core.Envelope) bool {
	if env == nil {
		return false
	}
	v, ok := env.Get(KVUpgraded)
	b, ok2 := v.(bool)
	return ok && ok2 && b
}

// StatusNote is the running-pipeline snapshot for a progress execution.
// It is pipeline state, not recalled notes.
func StatusNote(env *core.Envelope) string {
	if IntentOf(env) != IntentProgress || env == nil {
		return ""
	}
	v, ok := env.Get(KVStatus)
	s, _ := v.(string)
	if !ok || s == "" {
		return ""
	}
	return "Other pipelines already running:\n" + s
}

// NewUpgradeTool lets a light-tier execution ask for the full model and tool
// surface. L1 does not expose this tool.
func NewUpgradeTool(env *core.Envelope) llm.Tool {
	return llm.Tool{
		Name:        ToolUpgrade,
		Description: "Leave the light tier and continue this turn with the full model and tool surface. Call this instead of answering when the user needs files, the network, a workflow, or a stronger model.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Execute: func(ctx *llm.ToolExecContext, _ any) (any, error) {
			if env != nil {
				env.Set(KVUpgrade, true)
			}
			if ctx != nil {
				ctx.RequestHalt()
			}
			return map[string]any{"status": "upgrade"}, nil
		},
	}
}
