package llm

import (
	"context"
	"fmt"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// TestExecuteToolsEmitsEvents 验证工具执行循环把每个工具的「调用发起」与「返回」
// 追加进事件轨迹 sink（C1 深层集成）。sink 经 context 传入，对应 pipeline.Execute
// 注入的 EventSink。
func TestExecuteToolsEmitsEvents(t *testing.T) {
	sink := core.NewMemorySink(64)
	ctx := core.WithEventSink(context.Background(), sink)

	toolMap := map[string]*Tool{
		"echo": {
			Name: "echo",
			Execute: ToolExecuteFunc(func(ctx *ToolExecContext, input any) (any, error) {
				return "pong", nil
			}),
		},
	}
	calls := []ToolCall{
		{ToolCallID: "c1", ToolName: "echo", Input: map[string]any{"msg": "hi"}},
	}

	if _, err := executeTools(ctx, calls, toolMap, nil, nil, &OrchestrateConfig{}); err != nil {
		t.Fatal(err)
	}

	evs := sink.Snapshot()
	// 期望：每个工具一次 call + 一次 result = 2 条。
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(evs), evs)
	}
	if evs[0].Kind != core.EventToolCall || evs[0].Source != "tool:echo" {
		t.Errorf("ev0 = %+v, want tool/call from tool:echo", evs[0])
	}
	if evs[0].Surface {
		t.Error("tool/call should be log-only (Surface=false)")
	}
	if evs[1].Kind != core.EventToolResult || !evs[1].Surface {
		t.Errorf("ev1 = %+v, want tool/result with Surface=true", evs[1])
	}
}

// TestExecuteToolsEmitsErrorResult 验证工具执行失败时仍记录 result 事件（is_error=true）。
func TestExecuteToolsEmitsErrorResult(t *testing.T) {
	sink := core.NewMemorySink(64)
	ctx := core.WithEventSink(context.Background(), sink)

	toolMap := map[string]*Tool{
		"boom": {
			Name: "boom",
			Execute: ToolExecuteFunc(func(ctx *ToolExecContext, input any) (any, error) {
				return nil, fmt.Errorf("kaboom")
			}),
		},
	}
	calls := []ToolCall{{ToolCallID: "c1", ToolName: "boom", Input: "x"}}

	if _, err := executeTools(ctx, calls, toolMap, nil, nil, &OrchestrateConfig{}); err != nil {
		t.Fatal(err)
	}

	evs := sink.Snapshot()
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(evs), evs)
	}
	if evs[1].Kind != core.EventToolResult {
		t.Fatalf("ev1 = %+v, want tool/result", evs[1])
	}
	payload, ok := evs[1].Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T", evs[1].Payload)
	}
	if payload["is_error"] != true {
		t.Errorf("result event is_error = %v, want true", payload["is_error"])
	}
}
