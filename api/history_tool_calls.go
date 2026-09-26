package api

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/kasuganosora/thinkbot/llm"
)

// maxPersistedToolOutputRunes bounds a tool output stored with a
// Telegram/managed-channel reply (audit only; the LLM history loader never
// reads tool_calls).
const maxPersistedToolOutputRunes = 4000

// toolCallsJSONFromResult serializes the tool calls of one orchestration run
// in the same shape the web chat stores in chat_messages.tool_calls
// ({id,name,title,status,input,output}), so Telegram turns become auditable:
// before this, only the reply text was saved for Telegram, and the full
// arguments of e.g. compact_context were unrecoverable (the log only keeps a
// truncated input preview). Returns "" when there were no tool calls.
func toolCallsJSONFromResult(result *llm.GenerateResult) string {
	if result == nil {
		return ""
	}
	msgs := result.Messages
	if len(msgs) == 0 {
		for _, st := range result.Steps {
			msgs = append(msgs, st.Messages...)
		}
	}
	type entry = map[string]any
	var calls []entry
	idx := map[string]int{}
	for _, m := range msgs {
		for _, p := range m.Content {
			switch v := p.(type) {
			case llm.ToolCallPart:
				if _, dup := idx[v.ToolCallID]; dup && v.ToolCallID != "" {
					continue
				}
				idx[v.ToolCallID] = len(calls)
				calls = append(calls, entry{
					"id": v.ToolCallID, "name": v.ToolName, "title": v.ToolName,
					"status": "unknown", "input": v.Input,
				})
			case llm.ToolResultPart:
				i, ok := idx[v.ToolCallID]
				if !ok {
					continue
				}
				status := "success"
				if v.IsError {
					status = "error"
				}
				out, truncated := clipToolOutput(v.Result)
				calls[i]["status"] = status
				calls[i]["output"] = out
				if truncated {
					calls[i]["outputTruncated"] = true
				}
				if v.InvocationID != "" {
					calls[i]["invocationId"] = v.InvocationID
				}
			}
		}
	}
	if len(calls) == 0 {
		return ""
	}
	b, err := json.Marshal(calls)
	if err != nil {
		return ""
	}
	return string(b)
}

func clipToolOutput(v any) (string, bool) {
	var s string
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		s = t
	case []byte:
		s = string(t)
	case json.RawMessage:
		s = string(t)
	default:
		if b, err := json.Marshal(t); err == nil {
			s = string(b)
		} else {
			s = fmt.Sprintf("%v", t)
		}
	}
	if utf8.RuneCountInString(s) <= maxPersistedToolOutputRunes {
		return s, false
	}
	return string([]rune(s)[:maxPersistedToolOutputRunes]), true
}
