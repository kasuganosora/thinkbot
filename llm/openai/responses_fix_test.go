package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestResponsesStreamUsesFinalTextWithoutDuplication(t *testing.T) {
	doneOnly := []string{
		`{"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"final only"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"final only"}]}]}}`,
	}
	if got := collectStreamText(t, doneOnly); got != "final only" {
		t.Fatalf("done/completed text %q", got)
	}
	deltas := []string{
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"hel"}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"lo"}`,
		`{"type":"response.output_item.done","item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"hello"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"hello"}]}]}}`,
	}
	if got := collectStreamText(t, deltas); got != "hello" {
		t.Fatalf("delta text duplicated or lost: %q", got)
	}
	completedOnly := []string{
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_2","content":[{"type":"output_text","text":"from completed"}]}]}}`,
	}
	if got := collectStreamText(t, completedOnly); got != "from completed" {
		t.Fatalf("completed text %q", got)
	}
}

func TestResponsesIncompleteKeepsText(t *testing.T) {
	events := []string{
		`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"partial"}]}]}}`,
	}
	text, errText := collectStream(t, events)
	if text != "partial" || errText != "" {
		t.Fatalf("text %q err %q", text, errText)
	}
}

func TestResponsesStreamAndSyncSurfaceFailures(t *testing.T) {
	const leaked = "sk-test-LEAK-9999-secret"
	errorEvent := []string{
		`{"type":"error","code":"unsupported_parameter","param":"temperature","message":"rejected ` + leaked + ` hello"}`,
	}
	_, errText := collectStream(t, errorEvent)
	assertSafeFailure(t, errText, leaked, "code=unsupported_parameter", "param=temperature")

	failed := []string{
		`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"server_error","code":"server_error","message":"echo ` + leaked + `"}}}`,
	}
	_, errText = collectStream(t, failed)
	assertSafeFailure(t, errText, leaked, "code=server_error", "type=server_error")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","status":"failed","error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"top_p","message":"nope `+leaked+`"}}`)
	}))
	defer srv.Close()
	client := New(WithAPIKey("dummy-token-not-logged"), WithBaseURL(srv.URL), WithTimeout(2*time.Second))
	_, err := client.DoGenerate(context.Background(), llm.GenerateParams{
		Model:    llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{llm.UserMessage("ping")},
	})
	assertSafeFailure(t, llm.SafeErrorText(err), leaked, "code=unsupported_parameter", "param=top_p", "type=invalid_request_error")
}

func TestResponsesReasoningRoundTrip(t *testing.T) {
	const enc = "enc-blob-aabbcc"
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{
				"id":"resp_1","status":"completed","model":"model-sol-main",
				"output":[
					{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"`+enc+`"},
					{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"ok"}]}
				]
			}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_2","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"next"}]}]}`)
	}))
	defer srv.Close()
	client := New(WithAPIKey("dummy-token-not-logged"), WithBaseURL(srv.URL), WithTimeout(2*time.Second))
	first, err := client.DoGenerate(context.Background(), llm.GenerateParams{
		Model:    llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{llm.UserMessage("ping")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Reasoning != "think" || first.ReasoningProviderMetadata["encrypted_content"] != enc || first.ReasoningProviderMetadata["id"] != "rs_1" {
		t.Fatalf("metadata %+v reasoning %q", first.ReasoningProviderMetadata, first.Reasoning)
	}
	_, err = client.DoGenerate(context.Background(), llm.GenerateParams{
		Model: llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{
			llm.UserMessage("ping"),
			{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
				llm.ReasoningPart{Text: first.Reasoning, ProviderMetadata: first.ReasoningProviderMetadata},
				llm.TextPart{Text: first.Text},
			}},
			llm.UserMessage("continue"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("calls %d", len(bodies))
	}
	for i, body := range bodies {
		if !strings.Contains(body, `"store":false`) || !strings.Contains(body, `"reasoning.encrypted_content"`) {
			t.Fatalf("call %d missing include/store: %s", i, body)
		}
	}
	second := bodies[1]
	if !strings.Contains(second, `"encrypted_content":"`+enc+`"`) || !strings.Contains(second, `"id":"rs_1"`) || !strings.Contains(second, `"summary_text"`) {
		t.Fatalf("reasoning not round-tripped: %s", second)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(second), &raw); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesReasoningReplayIsScopedToProviderAndModel(t *testing.T) {
	const enc = "enc-blob-aabbcc"
	provider := "http://same.example/v1"
	other := "http://other.example/v1"
	meta := map[string]any{"id": "rs_1", "encrypted_content": enc, "model": "gpt-6.1-synth", "provider": provider}
	history := func(extra map[string]any) []llm.Message {
		copied := map[string]any{}
		for k, v := range meta {
			copied[k] = v
		}
		for k, v := range extra {
			copied[k] = v
		}
		return []llm.Message{
			llm.UserMessage("ping"),
			{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
				llm.ReasoningPart{Text: "think", ProviderMetadata: copied},
				llm.TextPart{Text: "answer"},
				llm.ToolCallPart{ToolCallID: "call_1", ToolName: "lookup", Input: map[string]any{"q": "x"}},
			}},
		}
	}
	body := captureReplay(t, provider, "gpt-6.1-synth", history(nil))
	reasoningAt := strings.Index(body, `"type":"reasoning"`)
	messageAt := strings.Index(body, `"answer"`)
	callAt := strings.Index(body, `"type":"function_call"`)
	if reasoningAt < 0 || messageAt < 0 || callAt < 0 || !(reasoningAt < messageAt && messageAt < callAt) {
		t.Fatalf("order reasoning=%d text=%d call=%d\n%s", reasoningAt, messageAt, callAt, body)
	}
	if strings.Count(body, `"role":"assistant"`) != 1 {
		t.Fatalf("extra assistant message: %s", body)
	}
	if !strings.Contains(body, `"encrypted_content":"`+enc+`"`) {
		t.Fatalf("missing encrypted content: %s", body)
	}
	switched := captureReplay(t, provider, "gpt-6.1-synth", history(map[string]any{"model": "other-model"}))
	if strings.Contains(switched, `"type":"reasoning"`) || strings.Contains(switched, enc) {
		t.Fatalf("replayed after model switch: %s", switched)
	}
	if !strings.Contains(switched, `"answer"`) || !strings.Contains(switched, `"type":"function_call"`) {
		t.Fatalf("switched history lost output: %s", switched)
	}
	otherProvider := captureReplay(t, provider, "gpt-6.1-synth", history(map[string]any{"provider": other}))
	if strings.Contains(otherProvider, `"type":"reasoning"`) || strings.Contains(otherProvider, enc) {
		t.Fatalf("replayed after provider switch: %s", otherProvider)
	}
	textOnly := captureReplay(t, provider, "gpt-6.1-synth", []llm.Message{
		llm.UserMessage("ping"),
		{Role: llm.MessageRoleAssistant, Content: []llm.MessagePart{
			llm.ReasoningPart{Text: "only thinking"},
			llm.TextPart{Text: "answer"},
		}},
	})
	if strings.Contains(textOnly, `"type":"reasoning"`) || strings.Contains(textOnly, "only thinking") {
		t.Fatalf("text-only reasoning sent: %s", textOnly)
	}
}

func captureReplay(t *testing.T, baseURL, model string, messages []llm.Message) string {
	t.Helper()
	params := llm.GenerateParams{Model: llm.ChatModel(model), Messages: messages}
	req, err := buildResponsesRequest(strings.TrimRight(baseURL, "/"), &params)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func collectStreamText(t *testing.T, events []string) string {
	t.Helper()
	text, errText := collectStream(t, events)
	if errText != "" {
		t.Fatalf("unexpected error %s", errText)
	}
	return text
}

func collectStream(t *testing.T, events []string) (string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	client := New(WithAPIKey("dummy-token-not-logged"), WithBaseURL(srv.URL), WithTimeout(2*time.Second))
	sr, err := client.DoStream(context.Background(), llm.GenerateParams{
		Model:    llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{llm.UserMessage("ping")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text, errText string
	for part := range sr.Stream {
		switch p := part.(type) {
		case *llm.TextDeltaPart:
			text += p.Text
		case *llm.ErrorPart:
			errText = llm.SafeErrorText(p.Error)
		}
	}
	return text, errText
}

func assertSafeFailure(t *testing.T, text, leaked string, want ...string) {
	t.Helper()
	if text == "" {
		t.Fatal("missing error")
	}
	for _, part := range want {
		if !strings.Contains(text, part) {
			t.Fatalf("safe text %q missing %q", text, part)
		}
	}
	for _, secret := range []string{leaked, "hello", "echo", "rejected", "nope"} {
		if strings.Contains(text, secret) {
			t.Fatalf("leaked %q in %s", secret, text)
		}
	}
}
