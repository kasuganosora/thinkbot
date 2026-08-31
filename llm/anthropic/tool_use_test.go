package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ============================================================================
// SchemaBuilder
// ============================================================================

func TestSchemaBuilderBasic(t *testing.T) {
	schema := NewSchema().
		PropString("location", "City name", true).
		PropStringEnum("unit", "Unit", false, "celsius", "fahrenheit").
		Build()

	if schema["type"] != "object" {
		t.Errorf("expected type=object")
	}
	props := schema["properties"].(map[string]SchemaProperty)
	if props["location"].Type != "string" {
		t.Errorf("expected location=string")
	}
	if len(props["unit"].Enum) != 2 {
		t.Errorf("expected 2 enum values")
	}
	r := schema["required"].([]string)
	if len(r) != 1 || r[0] != "location" {
		t.Errorf("expected required=[location], got %v", r)
	}
}

func TestSchemaBuilderAllTypes(t *testing.T) {
	schema := NewSchema().
		PropString("s", "str", true).
		PropStringFormat("dt", "datetime", "date-time", false).
		PropInteger("i", "int", true).
		PropNumber("n", "num", false).
		PropBoolean("b", "bool", false).
		PropArray("arr", "array", "string", false).
		Build()
	props := schema["properties"].(map[string]SchemaProperty)
	if props["dt"].Format != "date-time" {
		t.Errorf("expected format=date-time")
	}
	if props["arr"].Items == nil || props["arr"].Items.Type != "string" {
		t.Errorf("expected items.type=string")
	}
	r := schema["required"].([]string)
	if len(r) != 2 {
		t.Errorf("expected 2 required, got %d", len(r))
	}
}

// ============================================================================
// Tool 构造器
// ============================================================================

func TestNewTool(t *testing.T) {
	schema := NewSchema().PropString("city", "City", true).Build()
	tool := NewTool("get_weather", "Get weather", schema)
	if tool.Name != "get_weather" || tool.Strict != nil {
		t.Errorf("unexpected tool: %+v", tool)
	}
}

func TestNewSimpleTool(t *testing.T) {
	tool := NewSimpleTool("ping", "Health check")
	if tool.Strict != nil {
		t.Error("expected nil strict")
	}
	s := tool.InputSchema.(map[string]any)
	if s["type"] != "object" {
		t.Errorf("expected schema type=object")
	}
}

func TestNewStrictTool(t *testing.T) {
	tool := NewStrictTool("search", "Search", NewSchema().PropString("q", "q", true).Build())
	if tool.Strict == nil || !*tool.Strict {
		t.Error("expected strict=true")
	}
}

func TestToolWithExamples(t *testing.T) {
	tool := NewTool("fn", "desc", map[string]any{"type": "object"}).
		WithExamples(map[string]any{"x": 1}, map[string]any{"x": 2})
	if len(tool.InputExamples) != 2 {
		t.Fatalf("expected 2 examples, got %d", len(tool.InputExamples))
	}
}

// ============================================================================
// ToolChoice 构造器
// ============================================================================

func TestToolChoiceConstructors(t *testing.T) {
	if ChoiceAuto().Type != ToolChoiceAuto {
		t.Error("auto failed")
	}
	if ChoiceAny().Type != ToolChoiceAny {
		t.Error("any failed")
	}
	if tc := ChoiceTool("fn"); tc.Type != ToolChoiceTool || tc.Name != "fn" {
		t.Errorf("tool failed: %+v", tc)
	}
	if ChoiceNone().Type != ToolChoiceNone {
		t.Error("none failed")
	}
}

func TestToolChoiceDisableParallel(t *testing.T) {
	tc := ChoiceAuto().WithDisableParallel(true)
	if !tc.DisableParallel {
		t.Error("expected disable_parallel=true")
	}
}

// ============================================================================
// 内容块构造器
// ============================================================================

func TestToolUseBlock(t *testing.T) {
	block := ToolUseBlock("toolu_1", "get_weather", map[string]any{"location": "SF"})
	if block.Type != ContentTypeToolUse || block.ID != "toolu_1" || block.Name != "get_weather" {
		t.Errorf("unexpected block: %+v", block)
	}
	var input map[string]any
	_ = json.Unmarshal(block.Input, &input)
	if input["location"] != "SF" {
		t.Errorf("expected location=SF")
	}
}

func TestToolResultBlock(t *testing.T) {
	block := ToolResultBlock("toolu_1", map[string]any{"temp": "25C"})
	if block.Type != ContentTypeToolResult || block.ToolUseID != "toolu_1" || block.IsError {
		t.Errorf("unexpected block: %+v", block)
	}
	var r map[string]any
	_ = json.Unmarshal(block.ResultContent, &r)
	if r["temp"] != "25C" {
		t.Errorf("expected temp=25C")
	}
}

func TestToolResultStringBlock(t *testing.T) {
	block := ToolResultStringBlock("toolu_1", "hello")
	var s string
	_ = json.Unmarshal(block.ResultContent, &s)
	if s != "hello" {
		t.Errorf("expected 'hello', got %s", s)
	}
}

func TestToolResultErrorBlock(t *testing.T) {
	block := ToolResultErrorBlock("toolu_1", "failed")
	if !block.IsError {
		t.Error("expected is_error=true")
	}
	var s string
	_ = json.Unmarshal(block.ResultContent, &s)
	if s != "failed" {
		t.Errorf("expected 'failed'")
	}
}

// ============================================================================
// 响应检查辅助
// ============================================================================

func TestExtractToolUse(t *testing.T) {
	resp := &MessageResponse{
		Content: []ContentBlock{
			{Type: ContentTypeText, Text: "hi"},
			ToolUseBlock("toolu_1", "fn1", nil),
			ToolUseBlock("toolu_2", "fn2", nil),
		},
	}
	entries := ExtractToolUse(resp)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].ID != "toolu_1" || entries[0].Index != 1 {
		t.Errorf("unexpected first entry: %+v", entries[0])
	}
}

func TestExtractToolUseParsedInput(t *testing.T) {
	resp := &MessageResponse{
		Content: []ContentBlock{
			ToolUseBlock("t1", "fn", map[string]any{"x": "y"}),
		},
	}
	entries := ExtractToolUse(resp)
	var input map[string]string
	if err := entries[0].ParsedInput(&input); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if input["x"] != "y" {
		t.Errorf("expected x=y, got %s", input["x"])
	}
}

func TestHasToolUse(t *testing.T) {
	if !HasToolUse(&MessageResponse{Content: []ContentBlock{ToolUseBlock("t", "f", nil)}}) {
		t.Error("expected true")
	}
	if HasToolUse(&MessageResponse{Content: []ContentBlock{{Type: ContentTypeText, Text: "x"}}}) {
		t.Error("expected false")
	}
	if HasToolUse(nil) {
		t.Error("expected false for nil")
	}
}

func TestGetFirstToolUse(t *testing.T) {
	resp := &MessageResponse{
		Content: []ContentBlock{
			{Type: ContentTypeText, Text: "hi"},
			ToolUseBlock("t1", "fn", nil),
		},
	}
	first := GetFirstToolUse(resp)
	if first == nil || first.ID != "t1" {
		t.Errorf("expected t1, got %+v", first)
	}
	if GetFirstToolUse(&MessageResponse{Content: []ContentBlock{{Type: ContentTypeText}}}) != nil {
		t.Error("expected nil")
	}
}

func TestExtractText(t *testing.T) {
	resp := &MessageResponse{
		Content: []ContentBlock{
			{Type: ContentTypeText, Text: "Hello "},
			ToolUseBlock("t1", "fn", nil),
			{Type: ContentTypeText, Text: "world"},
		},
	}
	if ExtractText(resp) != "Hello world" {
		t.Errorf("got %q", ExtractText(resp))
	}
}

// ============================================================================
// ToolRegistry
// ============================================================================

func TestToolRegistryRegisterAndGet(t *testing.T) {
	r := NewToolRegistry()
	r.Register("search", "Search", func(map[string]any) (any, error) { return nil, nil },
		NewSchema().PropString("q", "query", true).Build())

	if _, ok := r.Get("search"); !ok {
		t.Error("expected to find search")
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("expected false for nonexistent")
	}
}

func TestToolRegistryNames(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterSimple("a", "A", func(map[string]any) (any, error) { return nil, nil })
	r.RegisterSimple("b", "B", func(map[string]any) (any, error) { return nil, nil })
	if len(r.Names()) != 2 {
		t.Errorf("expected 2 names")
	}
}

func TestToolRegistryBuildTools(t *testing.T) {
	r := NewToolRegistry()
	r.Register("get_weather", "Weather", func(map[string]any) (any, error) { return nil, nil },
		NewSchema().PropString("city", "City", true).Build())
	r.RegisterSimple("ping", "Ping", func(map[string]any) (any, error) { return nil, nil })

	tools := r.BuildTools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	// Simple tool should have default schema
	for _, tool := range tools {
		if tool.Name == "ping" {
			s := tool.InputSchema.(map[string]any)
			if s["type"] != "object" {
				t.Error("expected default object schema for simple tool")
			}
		}
	}
}

// ============================================================================
// ExecuteToolCalls
// ============================================================================

func TestExecuteToolCallsSuccess(t *testing.T) {
	r := NewToolRegistry()
	r.Register("get_weather", "Weather", func(input map[string]any) (any, error) {
		return map[string]any{"temp": "25C"}, nil
	}, nil)

	resp := &MessageResponse{
		Content: []ContentBlock{
			ToolUseBlock("toolu_1", "get_weather", map[string]any{"location": "SF"}),
		},
	}

	blocks := r.ExecuteToolCalls(resp)
	if len(blocks) != 1 || blocks[0].Type != ContentTypeToolResult {
		t.Fatalf("unexpected blocks: %+v", blocks)
	}
	if blocks[0].ToolUseID != "toolu_1" {
		t.Errorf("expected tool_use_id=toolu_1")
	}
	var r2 map[string]any
	_ = json.Unmarshal(blocks[0].ResultContent, &r2)
	if r2["temp"] != "25C" {
		t.Errorf("expected temp=25C")
	}
}

func TestExecuteToolCallsUnknownTool(t *testing.T) {
	r := NewToolRegistry()
	resp := &MessageResponse{
		Content: []ContentBlock{ToolUseBlock("t1", "nope", map[string]any{})},
	}
	blocks := r.ExecuteToolCalls(resp)
	if len(blocks) != 1 || !blocks[0].IsError {
		t.Error("expected error block for unknown tool")
	}
}

func TestExecuteToolCallsHandlerError(t *testing.T) {
	r := NewToolRegistry()
	r.Register("fail", "Fails", func(map[string]any) (any, error) {
		return nil, context.DeadlineExceeded
	}, nil)
	resp := &MessageResponse{
		Content: []ContentBlock{ToolUseBlock("t1", "fail", map[string]any{})},
	}
	blocks := r.ExecuteToolCalls(resp)
	if len(blocks) != 1 || !blocks[0].IsError {
		t.Error("expected error block")
	}
}

func TestExecuteToolCallsMultiple(t *testing.T) {
	r := NewToolRegistry()
	r.Register("a", "A", func(map[string]any) (any, error) { return "ra", nil }, nil)
	r.Register("b", "B", func(map[string]any) (any, error) { return "rb", nil }, nil)
	resp := &MessageResponse{
		Content: []ContentBlock{
			ToolUseBlock("t1", "a", nil),
			ToolUseBlock("t2", "b", nil),
		},
	}
	blocks := r.ExecuteToolCalls(resp)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].ToolUseID != "t1" || blocks[1].ToolUseID != "t2" {
		t.Errorf("unexpected order")
	}
}

func TestExecuteToolCallsEmpty(t *testing.T) {
	r := NewToolRegistry()
	if r.ExecuteToolCalls(nil) != nil {
		t.Error("expected nil for nil resp")
	}
	if r.ExecuteToolCalls(&MessageResponse{Content: []ContentBlock{{Type: ContentTypeText}}}) != nil {
		t.Error("expected nil for text-only")
	}
}

// ============================================================================
// RunToolLoop
// ============================================================================

func TestRunToolLoopSingleCall(t *testing.T) {
	callCount := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant", Model: req.Model,
				StopReason: StopReasonToolUse,
				Content:    []ContentBlock{ToolUseBlock("toolu_1", "get_weather", map[string]any{"location": "SF"})},
				Usage:      Usage{InputTokens: 10, OutputTokens: 5},
			})
		} else {
			// Verify tool_result in last message
			last := req.Messages[len(req.Messages)-1]
			found := false
			for _, b := range last.Content {
				if b.Type == ContentTypeToolResult {
					found = true
				}
			}
			if !found {
				t.Error("expected tool_result in messages")
			}
			_ = json.NewEncoder(w).Encode(MessageResponse{
				ID: "msg_2", Type: "message", Role: "assistant", Model: req.Model,
				StopReason: StopReasonEndTurn,
				Content:    []ContentBlock{{Type: ContentTypeText, Text: "Sunny"}},
				Usage:      Usage{InputTokens: 20, OutputTokens: 10},
			})
		}
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	registry := NewToolRegistry()
	registry.Register("get_weather", "Weather", func(map[string]any) (any, error) {
		return map[string]any{"w": "sunny"}, nil
	}, nil)

	resp, err := RunToolLoop(context.Background(), client, MessageRequest{
		Model: "claude-sonnet-4-6", MaxTokens: 1024,
		Messages: []Message{{Role: "user", Content: TextContent("Weather in SF?")}},
	}, registry, nil)
	if err != nil {
		t.Fatalf("RunToolLoop failed: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 calls, got %d", callCount)
	}
	if ExtractText(resp) != "Sunny" {
		t.Errorf("got %q", ExtractText(resp))
	}
}

func TestRunToolLoopAutoBuildTools(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tools) == 0 {
			t.Error("expected auto-populated tools")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MessageResponse{
			ID: "msg_1", Type: "message", Role: "assistant", Model: req.Model,
			StopReason: StopReasonEndTurn,
			Content:    []ContentBlock{{Type: ContentTypeText, Text: "ok"}},
			Usage:      Usage{InputTokens: 5, OutputTokens: 1},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	registry := NewToolRegistry()
	registry.RegisterSimple("ping", "Ping", func(map[string]any) (any, error) { return "pong", nil })

	_, err := RunToolLoop(context.Background(), client, MessageRequest{
		Model: "claude-sonnet-4-6", MaxTokens: 1024,
		Messages: []Message{{Role: "user", Content: TextContent("ping")}},
	}, registry, nil)
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
}

func TestRunToolLoopMaxRounds(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MessageResponse{
			ID: "msg", Type: "message", Role: "assistant", Model: req.Model,
			StopReason: StopReasonToolUse,
			Content:    []ContentBlock{ToolUseBlock("t", "echo", map[string]any{})},
			Usage:      Usage{InputTokens: 10, OutputTokens: 5},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	registry := NewToolRegistry()
	registry.RegisterSimple("echo", "Echo", func(i map[string]any) (any, error) { return i, nil })

	resp, err := RunToolLoop(context.Background(), client, MessageRequest{
		Model: "claude-sonnet-4-6", MaxTokens: 1024,
		Messages: []Message{{Role: "user", Content: TextContent("loop")}},
	}, registry, &ToolLoopOptions{MaxRounds: 3})
	if err != ErrMaxRoundsExceeded {
		t.Errorf("expected ErrMaxRoundsExceeded, got %v", err)
	}
	if resp == nil {
		t.Error("expected non-nil last response")
	}
}

func TestRunToolLoopCallbacks(t *testing.T) {
	callCount := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(MessageResponse{
				ID: "msg_1", Type: "message", Role: "assistant", Model: req.Model,
				StopReason: StopReasonToolUse,
				Content:    []ContentBlock{ToolUseBlock("t1", "calc", map[string]any{"x": 2})},
				Usage:      Usage{InputTokens: 10, OutputTokens: 5},
			})
		} else {
			_ = json.NewEncoder(w).Encode(MessageResponse{
				ID: "msg_2", Type: "message", Role: "assistant", Model: req.Model,
				StopReason: StopReasonEndTurn,
				Content:    []ContentBlock{{Type: ContentTypeText, Text: "done"}},
				Usage:      Usage{InputTokens: 15, OutputTokens: 5},
			})
		}
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	registry := NewToolRegistry()
	registry.Register("calc", "Calc", func(i map[string]any) (any, error) {
		return map[string]any{"r": i["x"]}, nil
	}, nil)

	var useCalled, resultCalled bool
	_, err := RunToolLoop(context.Background(), client, MessageRequest{
		Model: "claude-sonnet-4-6", MaxTokens: 1024,
		Messages: []Message{{Role: "user", Content: TextContent("calc")}},
	}, registry, &ToolLoopOptions{
		OnToolUse: func(e *ToolUseEntry) error {
			useCalled = true
			if e.Name != "calc" {
				t.Errorf("expected name=calc, got %s", e.Name)
			}
			return nil
		},
		OnToolResult: func(e *ToolUseEntry, b ContentBlock) {
			resultCalled = true
		},
	})
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	if !useCalled || !resultCalled {
		t.Error("callbacks not called")
	}
}

func TestRunToolLoopOnToolUseAbort(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MessageResponse{
			ID: "msg_1", Type: "message", Role: "assistant", Model: req.Model,
			StopReason: StopReasonToolUse,
			Content:    []ContentBlock{ToolUseBlock("t1", "dangerous", map[string]any{})},
			Usage:      Usage{InputTokens: 10, OutputTokens: 5},
		})
	})
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	registry := NewToolRegistry()
	registry.RegisterSimple("dangerous", "Danger", func(map[string]any) (any, error) { return nil, nil })

	_, err := RunToolLoop(context.Background(), client, MessageRequest{
		Model: "claude-sonnet-4-6", MaxTokens: 1024,
		Messages: []Message{{Role: "user", Content: TextContent("go")}},
	}, registry, &ToolLoopOptions{
		OnToolUse: func(e *ToolUseEntry) error { return context.Canceled },
	})
	if err == nil || !strings.Contains(err.Error(), "on_tool_use") {
		t.Errorf("expected on_tool_use error, got %v", err)
	}
}

// ============================================================================
// 并行辅助
// ============================================================================

func TestBuildParallelToolResults(t *testing.T) {
	entries := []ToolUseEntry{{ID: "t1", Name: "a"}, {ID: "t2", Name: "b"}}
	results := map[string]any{"t1": map[string]any{"ok": true}}
	blocks := BuildParallelToolResults(entries, results)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks")
	}
	if blocks[1].IsError != true {
		t.Error("expected error for missing result")
	}
}

func TestBuildParallelToolResultsWithErrors(t *testing.T) {
	entries := []ToolUseEntry{{ID: "t1", Name: "a"}, {ID: "t2", Name: "b"}}
	results := map[string]any{"t1": "ok", "t2": context.DeadlineExceeded}
	blocks := BuildParallelToolResultsWithErrors(entries, results)
	if blocks[0].IsError {
		t.Error("expected success for t1")
	}
	if !blocks[1].IsError {
		t.Error("expected error for t2")
	}
}

// ============================================================================
// 序列化
// ============================================================================

func TestToolSerialization(t *testing.T) {
	tool := NewTool("get_weather", "Get weather",
		NewSchema().PropString("location", "City", true).Build())
	data, _ := json.Marshal(tool)
	s := string(data)
	if !strings.Contains(s, `"name":"get_weather"`) {
		t.Errorf("expected name: %s", s)
	}
	if !strings.Contains(s, `"input_schema"`) {
		t.Errorf("expected input_schema: %s", s)
	}
}

func TestToolResultErrorBlockSerialization(t *testing.T) {
	block := ToolResultErrorBlock("t1", "fail")
	data, _ := json.Marshal(block)
	if !strings.Contains(string(data), `"is_error":true`) {
		t.Errorf("expected is_error in JSON: %s", data)
	}
}
