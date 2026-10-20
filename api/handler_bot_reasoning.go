package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Per-bot reasoning effort of background tasks (Web bot settings)
//
//   GET /api/bots/:id/reasoning → {tasks: {memory_dedup, heartbeat, dream, workflow_heal}, defaults: {...}}
//   PUT /api/bots/:id/reasoning   {tasks: {...}}
//
// Stored as bot.<id>.reasoning.<task> in the settings store and read live on
// every call. The main reply effort stays the bot definition's reasoningEffort.
// Values: "" inherit, provider = model default (parameter not sent), none /
// low / medium / high. Every value is mapped to the model's vocabulary and
// not sent to models that do not support the parameter.
// ============================================================================

type botReasoningResponse struct {
	Tasks    map[string]string `json:"tasks"`
	Defaults map[string]string `json:"defaults"`
}

func botReasoningDefaults() map[string]string {
	out := map[string]string{"heartbeat": "inherit_reply"}
	for _, t := range config.BotReasoningTasks {
		if t == "heartbeat" {
			continue
		}
		p := config.BotReasoningPurposes(t)[0]
		for _, ip := range llm.InternalPurposes {
			if ip.Name == p {
				out[t] = ip.DefaultReasoning
			}
		}
	}
	return out
}

func (s *Server) handleGetBotReasoning(c *gin.Context) {
	botID := c.Param("id")
	OK(c, botReasoningResponse{
		Tasks:    config.NewBuilder(s.store, s.logger).GetBotReasoning(botID),
		Defaults: botReasoningDefaults(),
	})
}

func (s *Server) handleUpdateBotReasoning(c *gin.Context) {
	botID := c.Param("id")
	var req struct {
		Tasks map[string]string `json:"tasks"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}
	known := map[string]bool{}
	for _, t := range config.BotReasoningTasks {
		known[t] = true
	}
	clean := map[string]string{}
	for t, v := range req.Tasks {
		v = strings.ToLower(strings.TrimSpace(v))
		if !known[t] {
			Fail(c, errs.BadRequest("unknown task: "+t))
			return
		}
		if !config.ValidBotReasoning(v) {
			Fail(c, errs.BadRequest("invalid reasoning effort for "+t+": "+v))
			return
		}
		clean[t] = v
	}
	for t, v := range clean {
		if err := s.store.Set(c.Request.Context(), config.BotReasoningKey(botID, t), v); err != nil {
			Fail(c, errs.Wrap(err, "save reasoning setting"))
			return
		}
	}
	s.handleGetBotReasoning(c)
}
