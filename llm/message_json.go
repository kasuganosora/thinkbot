package llm

import (
	"encoding/json"
	"fmt"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// MarshalJSON serializes a Message to JSON. When the message has exactly one
// TextPart with no metadata, the content is emitted as a plain string for
// compact wire representation.
func (m Message) MarshalJSON() ([]byte, error) {
	// Single TextPart with no metadata and no cache control → emit as plain string.
	var content any
	if len(m.Content) == 1 {
		if tp, ok := m.Content[0].(TextPart); ok && len(tp.ProviderMetadata) == 0 && tp.CacheControl == nil {
			content = tp.Text
		}
	}
	if content == nil {
		parts := make([]json.RawMessage, 0, len(m.Content))
		for _, p := range m.Content {
			raw, err := marshalPart(p)
			if err != nil {
				return nil, err
			}
			parts = append(parts, raw)
		}
		content = parts
	}
	if m.Usage != nil {
		return json.Marshal(struct {
			Role    MessageRole `json:"role"`
			Content any         `json:"content"`
			Usage   *Usage      `json:"usage,omitempty"`
		}{Role: m.Role, Content: content, Usage: m.Usage})
	}
	return json.Marshal(struct {
		Role    MessageRole `json:"role"`
		Content any         `json:"content"`
	}{Role: m.Role, Content: content})
}

// UnmarshalJSON deserializes a Message from JSON. Content can be a plain string
// or an array of typed parts.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    MessageRole     `json:"role"`
		Content json.RawMessage `json:"content"`
		Usage   *Usage          `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Usage = raw.Usage

	// content can be a string or an array of parts.
	if len(raw.Content) > 0 && raw.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(raw.Content, &s); err != nil {
			return errs.Wrap(err, "unmarshal string content")
		}
		m.Content = []MessagePart{TextPart{Text: s}}
		return nil
	}

	var parts []json.RawMessage
	if err := json.Unmarshal(raw.Content, &parts); err != nil {
		return errs.Wrap(err, "unmarshal content array")
	}
	m.Content = make([]MessagePart, 0, len(parts))
	for _, r := range parts {
		p, err := unmarshalPart(r)
		if err != nil {
			return err
		}
		m.Content = append(m.Content, p)
	}
	return nil
}

func marshalPart(p MessagePart) (json.RawMessage, error) {
	type typed struct {
		Type MessagePartType `json:"type"`
	}
	base, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	typeJSON, _ := json.Marshal(typed{Type: p.PartType()})

	// merge {"type":"..."} into the part's JSON
	merged := make(map[string]json.RawMessage)
	if err := json.Unmarshal(typeJSON, &merged); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	return json.Marshal(merged)
}

func unmarshalPart(data json.RawMessage) (MessagePart, error) {
	var probe struct {
		Type MessagePartType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, errs.Wrap(err, "unmarshal message part type")
	}
	switch probe.Type {
	case PartTypeText:
		var p TextPart
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		return p, nil
	case PartTypeReasoning:
		var p ReasoningPart
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		return p, nil
	case PartTypeImage:
		var p ImagePart
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		return p, nil
	case PartTypeFile:
		var p FilePart
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		return p, nil
	case PartTypeToolCall:
		var p ToolCallPart
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		return p, nil
	case PartTypeToolResult:
		var p ToolResultPart
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, err
		}
		return p, nil
	default:
		return nil, fmt.Errorf("unknown message part type: %q", probe.Type)
	}
}
