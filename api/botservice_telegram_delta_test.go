package api

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/channel/telegram"
)

func TestToTelegramDelta_TextAndTools(t *testing.T) {
	d, ok := toTelegramDelta(outbound.Event{
		Type: outbound.EventLLMTextDelta,
		Data: map[string]any{"text": "半截"},
	})
	if !ok || d.Kind != telegram.DeltaText || d.Text != "半截" {
		t.Fatalf("got %+v ok=%v", d, ok)
	}
	if _, ok := toTelegramDelta(outbound.Event{
		Type: outbound.EventLLMTextDelta,
		Data: map[string]any{"text": ""},
	}); ok {
		t.Fatal("empty text must drop")
	}

	start, ok := toTelegramDelta(outbound.Event{
		Type: outbound.EventLLMToolCall,
		Data: map[string]any{
			"toolCallId": "c1",
			"tool":       "sandbox_read_file",
			"input":      map[string]any{"path": "/secret"},
		},
	})
	if !ok || start.Kind != telegram.DeltaToolStart || start.Text != "sandbox_read_file" {
		t.Fatalf("got %+v ok=%v", start, ok)
	}
	// Name only — no path leakage via Text.
	if start.Text != "sandbox_read_file" {
		t.Fatal("tool delta must carry name only")
	}

	if _, ok := toTelegramDelta(outbound.Event{
		Type: outbound.EventLLMStart,
		Data: map[string]any{},
	}); ok {
		t.Fatal("non-delta events must drop")
	}
}
