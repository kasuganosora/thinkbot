package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestResponsesRoundTrip_ToolsReasoningAndImage(t *testing.T) {
	var body []byte
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"resp_1","model":"model-sol-main","output":[
				{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},
				{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}
			]
		}`)
	}))
	defer srv.Close()
	client := New(WithBaseURL(srv.URL), WithAPIKey("dummy-token-alpha-aaaa"))
	effort := "low"
	maxTok := 128
	temp := 0.2
	res, err := client.DoGenerate(context.Background(), llm.GenerateParams{
		Model:           llm.ChatModel("model-sol-main"),
		Temperature:     &temp,
		MaxTokens:       &maxTok,
		ReasoningEffort: &effort,
		Tools:           []llm.Tool{{Name: "lookup", Description: "find", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}},
		Messages: []llm.Message{
			{Role: llm.MessageRoleUser, Content: []llm.MessagePart{
				llm.TextPart{Text: "see"},
				llm.ImagePart{Image: "https://example.test/a.png", MediaType: "image/png"},
			}},
			{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
				llm.ReasoningPart{Text: "prior"},
				llm.ToolCallPart{ToolCallID: "call_0", ToolName: "lookup", Input: map[string]any{"q": "old"}},
			}},
			{Role: llm.MessageRoleTool, Content: []llm.MessagePart{
				llm.ToolResultPart{ToolCallID: "call_0", Result: map[string]any{"ok": true}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/responses" && path != "/responses" {
		t.Fatalf("path %s", path)
	}
	raw := string(body)
	for _, want := range []string{`"type":"function"`, `"name":"lookup"`, "input_image", "function_call", "function_call_output", `"effort":"low"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("payload missing %s\n%s", want, raw)
		}
	}
	if strings.Contains(raw, `"text":"prior"`) || strings.Contains(raw, `"type":"reasoning"`) {
		t.Fatalf("text-only reasoning was replayed: %s", raw)
	}
	if res.Reasoning != "think" || len(res.ToolCalls) != 1 || res.ToolCalls[0].ToolName != "lookup" {
		t.Fatalf("result reasoning %q calls %+v", res.Reasoning, res.ToolCalls)
	}
	_, err = client.DoGenerate(context.Background(), llm.GenerateParams{
		Model: llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{{Role: llm.MessageRoleUser, Content: []llm.MessagePart{
			llm.FilePart{Data: "data:text/plain,hi", MediaType: "text/plain", Filename: "a.txt"},
		}}},
	})
	if err == nil || !strings.Contains(err.Error(), "file content is not sent") {
		t.Fatalf("file part err %v", err)
	}
}

func TestResponsesStream_ParsesReasoningAndToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		events := []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"r1"}}`,
			`{"type":"response.reasoning.delta","item_id":"r1","delta":"think"}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc1","call_id":"call_1","name":"lookup"}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"q\":\"x\"}"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}}`,
			`{"type":"response.completed","response":{"id":"resp_1","model":"model-sol-main"}}`,
		}
		for _, ev := range events {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	client := New(WithBaseURL(srv.URL), WithAPIKey("dummy-token-alpha-aaaa"))
	stream, err := client.DoStream(context.Background(), llm.GenerateParams{
		Model:    llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{llm.UserMessage("go")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var reasoning, tool string
	for part := range stream.Stream {
		switch p := part.(type) {
		case *llm.ReasoningDeltaPart:
			reasoning += p.Text
		case *llm.StreamToolCallPart:
			tool = p.ToolName
			b, _ := json.Marshal(p.Input)
			if !strings.Contains(string(b), "x") {
				t.Fatalf("tool input %s", b)
			}
		}
	}
	if reasoning != "think" || tool != "lookup" {
		t.Fatalf("reasoning %q tool %q", reasoning, tool)
	}
}
