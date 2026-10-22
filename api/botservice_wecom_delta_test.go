package api

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/channel/wecom"
)

// TestToWecomDelta_Text 正文增量原样转发。
func TestToWecomDelta_Text(t *testing.T) {
	d, ok := toWecomDelta(outbound.Event{
		Type: outbound.EventLLMTextDelta,
		Data: map[string]any{"text": "半截正文"},
	})
	if !ok || d.Kind != wecom.DeltaText || d.Text != "半截正文" {
		t.Fatalf("got %+v ok=%v, want text delta", d, ok)
	}
	// 空文本不该产生一帧（否则每次心跳都会推一次空帧）。
	if _, ok := toWecomDelta(outbound.Event{
		Type: outbound.EventLLMTextDelta,
		Data: map[string]any{"text": ""},
	}); ok {
		t.Error("empty text delta must be dropped")
	}
}

// TestToWecomDelta_ToolsOnlyCarryName 工具事件只带**名字**。
//
// ⚠️ 这是外泄红线：input / output 里可能有绝对路径、命令行、URL，
// 一旦带进企微聊天记录就是内部信息外泄。所以这里显式断言它们没被带出来。
func TestToWecomDelta_ToolsOnlyCarryName(t *testing.T) {
	start, ok := toWecomDelta(outbound.Event{
		Type: outbound.EventLLMToolCall,
		Data: map[string]any{
			"toolCallId": "c1",
			"tool":       "sandbox_read_file",
			"input":      map[string]any{"path": "/Users/sion/secret/token.txt"},
		},
	})
	if !ok || start.Kind != wecom.DeltaToolStart || start.Text != "sandbox_read_file" || start.ID != "c1" {
		t.Fatalf("got %+v ok=%v, want tool start", start, ok)
	}

	done, ok := toWecomDelta(outbound.Event{
		Type: outbound.EventLLMToolResult,
		Data: map[string]any{
			"toolCallId": "c1",
			"tool":       "sandbox_read_file",
			"output":     map[string]any{"stdout": "SECRET=xxx"},
		},
	})
	if !ok || done.Kind != wecom.DeltaToolDone || done.Failed {
		t.Fatalf("got %+v ok=%v, want successful tool done", done, ok)
	}

	failed, ok := toWecomDelta(outbound.Event{
		Type: outbound.EventLLMToolResult,
		Data: map[string]any{
			"toolCallId": "c1",
			"tool":       "sandbox_exec",
			"error":      "exit status 1",
		},
	})
	if !ok || !failed.Failed {
		t.Fatalf("got %+v ok=%v, want failed tool done", failed, ok)
	}
}

// TestToWecomDelta_IgnoresOtherEvents 其余事件一律不转发（否则每步都会多推一帧空内容）。
func TestToWecomDelta_IgnoresOtherEvents(t *testing.T) {
	for _, typ := range []outbound.EventType{
		outbound.EventLLMStart, outbound.EventLLMStepDone, outbound.EventLLMDone,
		outbound.EventLLMToolProgress, outbound.EventStageEnter, outbound.EventMessageDone,
	} {
		if _, ok := toWecomDelta(outbound.Event{Type: typ, Data: map[string]any{}}); ok {
			t.Errorf("event %s must not be forwarded", typ)
		}
	}
}
