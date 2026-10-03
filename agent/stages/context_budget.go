package stages

import (
	"fmt"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

const (
	contextRemainingToolName = "get_context_remaining"
	searchContextToolName    = "search_context_history"
)

// contextBudgetNote 把当前窗口余量写进系统提示，让模型在触发线之前主动收束。
// 没有压缩器（自动压缩关闭）时不注入。
func (s *LLMStage) contextBudgetNote(env *core.Envelope, systemPrompt string, messages []llm.Message) string {
	compactor, ok := s.getCompactor(conversationKey(env))
	if !ok || compactor == nil {
		return ""
	}
	b := compactor.Budget()
	if b.Window <= 0 || b.Trigger <= 0 {
		return ""
	}
	used := llm.EstimateMessagesTokens(messages) + llm.EstimateSystemTokens(systemPrompt)
	remaining := b.Trigger - used
	if remaining < 0 {
		remaining = 0
	}
	pct := 100
	if b.Trigger > 0 {
		pct = used * 100 / b.Trigger
	}
	note := fmt.Sprintf(`Context budget: window %d tokens, compact trigger at %d (%.0f%% of the model window), about %d used (messages plus system prompt), %d left before automatic compaction. Raw history is kept; compact_context folds older turns into a note and search_context_history can recover details afterwards.`,
		b.Window, b.Trigger, b.Ratio*100, used, remaining)
	if pct >= 75 {
		note += " Budget is low: finish the current step, then call compact_context with a focus listing open tasks, decisions, paths and rejected approaches before starting more large tool calls."
	}
	return note
}

func (s *LLMStage) newContextRemainingTool(env *core.Envelope, messages []llm.Message) llm.Tool {
	return llm.Tool{
		Name: contextRemainingToolName,
		Description: "Read the context-window oil gauge: model window, compact trigger, estimated tokens used, and tokens left before automatic compaction. Call this before a long tool run if you are unsure whether to compact first.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
			compactor, ok := s.getCompactor(conversationKey(env))
			if !ok || compactor == nil {
				return map[string]any{"status": "unavailable"}, nil
			}
			b := compactor.Budget()
			used := llm.EstimateMessagesTokens(messages)
			if live := llm.LiveContextFromContext(ctx); live != nil {
				used = llm.EstimateMessagesTokens(live.Snapshot())
			}
			remaining := b.Trigger - used
			if remaining < 0 {
				remaining = 0
			}
			return map[string]any{
				"window_tokens":  b.Window,
				"trigger_tokens": b.Trigger,
				"trigger_ratio":  b.Ratio,
				"reserved_tokens": b.Reserved,
				"used_tokens":    used,
				"remaining_tokens": remaining,
				"low":            used*100 >= b.Trigger*75,
			}, nil
		},
	}
}

func (s *LLMStage) newSearchContextTool(env *core.Envelope) llm.Tool {
	cfg := s.config.SelfCompact
	return llm.Tool{
		Name: searchContextToolName,
		Description: "Search raw chat history that compaction folded out of the live window. Use this when a summary is missing a path, error, decision or rejected approach. Returns short excerpts, not the whole row.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Substring to find in stored messages."},
				"limit": map[string]any{"type": "integer", "description": "Max hits, 1-8. Default 5."},
			},
			"required": []string{"query"},
		},
		Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
			if cfg == nil || cfg.History == nil {
				return map[string]any{"status": "unavailable"}, nil
			}
			m, _ := input.(map[string]any)
			query, _ := m["query"].(string)
			limit := 5
			switch n := m["limit"].(type) {
			case float64:
				limit = int(n)
			case int:
				limit = n
			}
			sessionID := chatSessionIDFromEnvelope(env)
			var before uint64
			if cfg.Store != nil && sessionID != "" {
				if b, err := cfg.Store.LatestContextCheckpointBoundary(env.Message.BotID, sessionID); err == nil {
					before = b
				}
			}
			hits, err := cfg.History.SearchContextHistory(env.Message.BotID, sessionID, strings.TrimSpace(query), before, limit)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(hits))
			for _, h := range hits {
				out = append(out, map[string]any{
					"id":         h.ID,
					"role":       h.Role,
					"excerpt":    h.Excerpt,
					"created_at": h.CreatedAt.UTC().Format(time.RFC3339),
				})
			}
			return map[string]any{"hits": out, "before_id": before}, nil
		},
	}
}
