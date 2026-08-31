package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fcResp 构建一个包含函数调用的 GenerateContentResponse。
func fcResp(fcName, fcID string, args map[string]any) *GenerateContentResponse {
	return &GenerateContentResponse{Candidates: []Candidate{{
		Content: Content{Role: RoleModel, Parts: []Part{
			FunctionCallPartWithID(fcName, fcID, args),
		}},
		FinishReason: FinishReasonStop,
	}}}
}

// textResp 构建一个包含纯文本的 GenerateContentResponse。
func textResp(text string) *GenerateContentResponse {
	return &GenerateContentResponse{Candidates: []Candidate{{
		Content:      Content{Role: RoleModel, Parts: []Part{TextPart(text)}},
		FinishReason: FinishReasonStop,
	}}}
}

// ============================================================================
// SchemaBuilder 测试
// ============================================================================

func TestSchemaBuilderBasic(t *testing.T) {
	schema := NewSchema().
		PropString("location", "City name", true).
		PropInteger("days", "Number of days", false).
		Build()

	var raw map[string]any
	_ = json.Unmarshal(schema, &raw)
	if raw["type"] != "object" {
		t.Errorf("type = %v", raw["type"])
	}
	props := raw["properties"].(map[string]any)
	if props["location"].(map[string]any)["type"] != "string" {
		t.Error("location type wrong")
	}
	reqd := raw["required"].([]any)
	if len(reqd) != 1 || reqd[0] != "location" {
		t.Errorf("required = %v", raw["required"])
	}
}

func TestSchemaBuilderEnum(t *testing.T) {
	schema := NewSchema().
		PropStringEnum("unit", "Temp unit", false, "celsius", "fahrenheit").
		Build()

	var raw map[string]any
	_ = json.Unmarshal(schema, &raw)
	unit := raw["properties"].(map[string]any)["unit"].(map[string]any)
	enum := unit["enum"].([]any)
	if len(enum) != 2 || enum[0] != "celsius" || enum[1] != "fahrenheit" {
		t.Errorf("enum = %v", enum)
	}
}

func TestSchemaBuilderTypes(t *testing.T) {
	schema := NewSchema().
		PropNumber("price", "Price", true).
		PropBoolean("active", "Active", false).
		PropArray("tags", "Tags", "string", true).
		Build()

	var raw map[string]any
	_ = json.Unmarshal(schema, &raw)
	props := raw["properties"].(map[string]any)
	if props["price"].(map[string]any)["type"] != "number" {
		t.Error("price type wrong")
	}
	if props["active"].(map[string]any)["type"] != "boolean" {
		t.Error("active type wrong")
	}
	tags := props["tags"].(map[string]any)
	if tags["type"] != "array" {
		t.Error("tags type wrong")
	}
}

// ============================================================================
// 构建器测试
// ============================================================================

func TestNewFunctionDeclaration(t *testing.T) {
	schema := NewSchema().PropString("q", "query", true).Build()
	fd := NewFunctionDeclaration("search", "Search", schema)
	if fd.Name != "search" || len(fd.Parameters) == 0 {
		t.Errorf("fd = %+v", fd)
	}
}

func TestNewSimpleFunctionDeclaration(t *testing.T) {
	fd := NewSimpleFunctionDeclaration("ping", "Check")
	if len(fd.Parameters) != 0 {
		t.Error("parameters should be empty")
	}
}

func TestNewFunctionTool(t *testing.T) {
	fd := NewSimpleFunctionDeclaration("fn1", "d1")
	tool := NewFunctionTool(fd)
	if len(tool.FunctionDeclarations) != 1 {
		t.Fatalf("expected 1 decl, got %d", len(tool.FunctionDeclarations))
	}
}

func TestNewFunctionToolFromDecls(t *testing.T) {
	tool := NewFunctionToolFromDecls(
		NewSimpleFunctionDeclaration("fn1", "d1"),
		NewSimpleFunctionDeclaration("fn2", "d2"),
	)
	if len(tool.FunctionDeclarations) != 2 {
		t.Fatalf("expected 2 decls")
	}
}

func TestNewToolConfig(t *testing.T) {
	tc := NewToolConfig(FunctionCallingModeAny, "fn1", "fn2")
	if tc.FunctionCallingConfig.Mode != FunctionCallingModeAny {
		t.Errorf("mode = %q", tc.FunctionCallingConfig.Mode)
	}
	if len(tc.FunctionCallingConfig.AllowedFunctionNames) != 2 {
		t.Error("expected 2 allowed names")
	}
}

func TestFunctionCallingModeValidated(t *testing.T) {
	if FunctionCallingModeValidated != "VALIDATED" {
		t.Errorf("VALIDATED = %q", FunctionCallingModeValidated)
	}
}

// ============================================================================
// 响应检查辅助测试
// ============================================================================

func TestHasFunctionCalls(t *testing.T) {
	if HasFunctionCalls(nil) {
		t.Error("nil should be false")
	}
	respFC := fcResp("fn", "id", nil)
	if !HasFunctionCalls(respFC) {
		t.Error("expected true")
	}
	if HasFunctionCalls(textResp("hi")) {
		t.Error("expected false")
	}
}

func TestExtractFunctionCalls(t *testing.T) {
	resp := &GenerateContentResponse{Candidates: []Candidate{{
		Content: Content{Role: RoleModel, Parts: []Part{
			FunctionCallPartWithID("get_weather", "fc-1", map[string]any{"city": "Paris"}),
			FunctionCallPartWithID("get_weather", "fc-2", map[string]any{"city": "London"}),
			TextPart("text"),
		}},
	}}}
	calls := ExtractFunctionCalls(resp)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].ID != "fc-1" || calls[1].ID != "fc-2" {
		t.Errorf("IDs = %q, %q", calls[0].ID, calls[1].ID)
	}
	if ExtractFunctionCalls(nil) != nil {
		t.Error("nil should return nil")
	}
}

func TestGetFirstFunctionCall(t *testing.T) {
	resp := fcResp("fn1", "id1", nil)
	fc := GetFirstFunctionCall(resp)
	if fc == nil || fc.Name != "fn1" {
		t.Errorf("fc = %+v", fc)
	}
	if GetFirstFunctionCall(textResp("x")) != nil {
		t.Error("expected nil")
	}
}

func TestExtractText(t *testing.T) {
	resp := &GenerateContentResponse{Candidates: []Candidate{{
		Content: Content{Role: RoleModel, Parts: []Part{
			{Text: "thinking", Thought: true},
			{Text: "World!", Thought: false},
			{Text: " Good!", Thought: false},
		}},
	}}}
	text := ExtractText(resp)
	if text != "World! Good!" {
		t.Errorf("text = %q", text)
	}
}

// ============================================================================
// ToolRegistry 测试
// ============================================================================

func TestToolRegistry(t *testing.T) {
	r := NewToolRegistry()
	r.Register("get_weather", "Weather", func(args map[string]any) (any, error) {
		return map[string]any{"temp": "25C"}, nil
	}, NewSchema().PropString("city", "City", true).Build())
	r.RegisterSimple("ping", "Health", func(args map[string]any) (any, error) {
		return "pong", nil
	})

	if entry, ok := r.Get("get_weather"); !ok || entry.Description != "Weather" {
		t.Errorf("get_weather entry = %+v, ok=%v", entry, ok)
	}
	if _, ok := r.Get("nonexistent"); ok {
		t.Error("expected not found")
	}
	if len(r.Names()) != 2 {
		t.Errorf("expected 2 names")
	}

	tool := r.BuildTool()
	if len(tool.FunctionDeclarations) != 2 {
		t.Fatalf("expected 2 declarations, got %d", len(tool.FunctionDeclarations))
	}
}

// ============================================================================
// ExecuteFunctionCalls 测试
// ============================================================================

func TestExecuteFunctionCalls(t *testing.T) {
	r := NewToolRegistry()
	r.Register("get_weather", "Weather", func(args map[string]any) (any, error) {
		return map[string]any{"temp": 25, "city": args["city"]}, nil
	}, NewSchema().PropString("city", "City", true).Build())

	parts := r.ExecuteFunctionCalls(fcResp("get_weather", "fc-1", map[string]any{"city": "Paris"}))
	if len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	fr := parts[0].FunctionResponse
	if fr.Name != "get_weather" || fr.ID != "fc-1" {
		t.Errorf("fr = %+v", fr)
	}
	if fr.Response["temp"] != 25 || fr.Response["city"] != "Paris" {
		t.Errorf("response = %v", fr.Response)
	}
}

func TestExecuteFunctionCallsUnknown(t *testing.T) {
	r := NewToolRegistry()
	parts := r.ExecuteFunctionCalls(fcResp("unknown", "fc-1", nil))
	if _, ok := parts[0].FunctionResponse.Response["error"]; !ok {
		t.Errorf("expected error, got %v", parts[0].FunctionResponse.Response)
	}
}

func TestExecuteFunctionCallsHandlerError(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterSimple("fail", "Fail", func(args map[string]any) (any, error) {
		return nil, errors.New("boom")
	})
	parts := r.ExecuteFunctionCalls(fcResp("fail", "fc-1", nil))
	errMsg, _ := parts[0].FunctionResponse.Response["error"].(string)
	if !strings.Contains(errMsg, "boom") {
		t.Errorf("error = %q", errMsg)
	}
}

func TestExecuteFunctionCallsNonMapResult(t *testing.T) {
	r := NewToolRegistry()
	r.RegisterSimple("count", "Count", func(args map[string]any) (any, error) { return 42, nil })
	parts := r.ExecuteFunctionCalls(fcResp("count", "fc-1", nil))
	if parts[0].FunctionResponse.Response["result"] != 42 {
		t.Errorf("result = %v", parts[0].FunctionResponse.Response["result"])
	}
}

func TestNormalizeResponse(t *testing.T) {
	if _, ok := normalizeResponse(nil)["result"]; !ok {
		t.Error("nil should wrap as result")
	}
	m := map[string]any{"a": 1}
	if normalizeResponse(m)["a"] != 1 {
		t.Error("map should pass through")
	}
	if normalizeResponse("x")["result"] != "x" {
		t.Error("string should wrap as result")
	}
}

// ============================================================================
// RunFunctionCallLoop 测试
// ============================================================================

func TestRunFunctionCallLoopSingleCall(t *testing.T) {
	var reqCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")

		if reqCount == 1 {
			_ = json.NewEncoder(w).Encode(fcResp("get_weather", "fc-1", map[string]any{"city": "Paris"}))
		} else {
			for _, c := range req.Contents {
				for _, p := range c.Parts {
					if p.FunctionResponse != nil && p.FunctionResponse.ID != "fc-1" {
						t.Errorf("FR ID = %q", p.FunctionResponse.ID)
					}
				}
			}
			_ = json.NewEncoder(w).Encode(textResp("Paris is sunny."))
		}
	}))
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	registry := NewToolRegistry()
	registry.Register("get_weather", "Weather", func(args map[string]any) (any, error) {
		return map[string]any{"weather": "sunny"}, nil
	}, NewSchema().PropString("city", "City", true).Build())

	resp, err := RunFunctionCallLoop(context.Background(), client, "gemini-2.5-flash",
		GenerateContentRequest{Contents: []Content{{Role: RoleUser, Parts: []Part{TextPart("Weather in Paris?")}}}},
		registry, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(ExtractText(resp), "Paris") {
		t.Errorf("text = %q", ExtractText(resp))
	}
	if reqCount != 2 {
		t.Errorf("expected 2 requests, got %d", reqCount)
	}
}

func TestRunFunctionCallLoopNoFC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(textResp("Hello!"))
	}))
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	resp, err := RunFunctionCallLoop(context.Background(), client, "gemini-2.5-flash",
		GenerateContentRequest{Contents: []Content{{Role: RoleUser, Parts: []Part{TextPart("Hi")}}}},
		NewToolRegistry(), nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if ExtractText(resp) != "Hello!" {
		t.Errorf("text = %q", ExtractText(resp))
	}
}

func TestRunFunctionCallLoopMaxRounds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fcResp("loop", fmt.Sprintf("fc-%d", time.Now().UnixNano()), map[string]any{}))
	}))
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	registry := NewToolRegistry()
	registry.RegisterSimple("loop", "Loop", func(args map[string]any) (any, error) { return nil, nil })

	_, err := RunFunctionCallLoop(context.Background(), client, "gemini-2.5-flash",
		GenerateContentRequest{Contents: []Content{{Role: RoleUser, Parts: []Part{TextPart("loop")}}}},
		registry, &FunctionCallLoopOptions{MaxRounds: 3})

	if !errors.Is(err, ErrMaxRoundsExceeded) {
		t.Errorf("expected ErrMaxRoundsExceeded, got %v", err)
	}
}

func TestRunFunctionCallLoopCallbacks(t *testing.T) {
	reqCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		w.Header().Set("Content-Type", "application/json")
		if reqCount == 1 {
			_ = json.NewEncoder(w).Encode(fcResp("get_count", "fc-1", map[string]any{}))
		} else {
			_ = json.NewEncoder(w).Encode(textResp("Count is 42."))
		}
	}))
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	registry := NewToolRegistry()
	registry.RegisterSimple("get_count", "Count", func(args map[string]any) (any, error) { return 42, nil })

	callCount, respCount := 0, 0
	_, err := RunFunctionCallLoop(context.Background(), client, "gemini-2.5-flash",
		GenerateContentRequest{Contents: []Content{{Role: RoleUser, Parts: []Part{TextPart("count?")}}}},
		registry, &FunctionCallLoopOptions{
			OnFunctionCall: func(fc *FunctionCall) error {
				callCount++
				return nil
			},
			OnFunctionResponse: func(fc *FunctionCall, response map[string]any) {
				respCount++
				if response["result"] != 42 {
					t.Errorf("result = %v", response["result"])
				}
			},
		})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if callCount != 1 || respCount != 1 {
		t.Errorf("callbacks: call=%d resp=%d, want 1,1", callCount, respCount)
	}
}

func TestRunFunctionCallLoopOnCallAbort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fcResp("dangerous", "fc-1", map[string]any{}))
	}))
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	registry := NewToolRegistry()
	registry.RegisterSimple("dangerous", "Dangerous", func(args map[string]any) (any, error) { return nil, nil })

	abortErr := errors.New("denied")
	_, err := RunFunctionCallLoop(context.Background(), client, "gemini-2.5-flash",
		GenerateContentRequest{Contents: []Content{{Role: RoleUser, Parts: []Part{TextPart("danger")}}}},
		registry, &FunctionCallLoopOptions{
			OnFunctionCall: func(fc *FunctionCall) error { return abortErr },
		})
	if !errors.Is(err, abortErr) {
		t.Errorf("expected abortErr, got %v", err)
	}
}

func TestRunFunctionCallLoopAutoTools(t *testing.T) {
	var sawTools bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req GenerateContentRequest
		_ = json.Unmarshal(body, &req)
		if len(req.Tools) > 0 && len(req.Tools[0].FunctionDeclarations) > 0 {
			sawTools = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(textResp("done"))
	}))
	defer srv.Close()

	client := New(WithAPIKey("test-key"), WithBaseURL(srv.URL))
	registry := NewToolRegistry()
	registry.RegisterSimple("fn1", "Function 1", func(args map[string]any) (any, error) { return nil, nil })

	_, err := RunFunctionCallLoop(context.Background(), client, "gemini-2.5-flash",
		GenerateContentRequest{Contents: []Content{{Role: RoleUser, Parts: []Part{TextPart("hi")}}}},
		registry, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !sawTools {
		t.Error("expected auto-tools from registry")
	}
}

// ============================================================================
// 并行函数调用辅助测试
// ============================================================================

func TestBuildParallelFunctionResponses(t *testing.T) {
	calls := []*FunctionCall{
		{Name: "get_temp", ID: "fc-1", Args: nil},
		{Name: "get_temp", ID: "fc-2", Args: nil},
	}
	results := map[string]any{
		"fc-1": map[string]any{"temp": "15C"},
		"fc-2": map[string]any{"temp": "12C"},
	}
	parts := BuildParallelFunctionResponses(calls, results)
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].FunctionResponse.Response["temp"] != "15C" {
		t.Errorf("parts[0] temp wrong")
	}
	if parts[1].FunctionResponse.Response["temp"] != "12C" {
		t.Errorf("parts[1] temp wrong")
	}
}

func TestBuildParallelFunctionResponsesMissing(t *testing.T) {
	calls := []*FunctionCall{{Name: "fn", ID: "fc-1", Args: nil}}
	results := map[string]any{} // fc-1 missing
	parts := BuildParallelFunctionResponses(calls, results)
	if _, ok := parts[0].FunctionResponse.Response["error"]; !ok {
		t.Error("expected error for missing result")
	}
}

func TestBuildParallelFunctionResponsesWithErrors(t *testing.T) {
	calls := []*FunctionCall{
		{Name: "fn", ID: "fc-1", Args: nil},
		{Name: "fn", ID: "fc-2", Args: nil},
	}
	results := map[string]any{
		"fc-1": map[string]any{"ok": true},
		"fc-2": errors.New("network failure"),
	}
	parts := BuildParallelFunctionResponsesWithErrors(calls, results)
	if parts[0].FunctionResponse.Response["ok"] != true {
		t.Errorf("fc-1 response wrong")
	}
	errMsg, _ := parts[1].FunctionResponse.Response["error"].(string)
	if !strings.Contains(errMsg, "network failure") {
		t.Errorf("fc-2 error = %q", errMsg)
	}
}
