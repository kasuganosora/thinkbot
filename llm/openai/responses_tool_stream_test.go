package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
)

// Official Responses stream order is:
// output_item.added -> function_call_arguments.delta* ->
// function_call_arguments.done -> output_item.done -> response.completed.
// arguments.done carries the final arguments string and must not suppress
// the tool-call part.

func TestResponsesStream_ToolCallSurvivesArgumentsDone(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_lookup","delta":"{\"q\":"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_lookup","arguments":"{\"q\":\"alpha\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":""}}`,
		`{"type":"response.completed","response":{"id":"resp_lookup","model":"model-sol-main","status":"completed","output":[{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":"{\"q\":\"alpha\"}"}]}}`,
	}
	got := collectToolStream(t, events, false)
	assertToolCalls(t, got, []wantToolCall{{
		id:   "call_lookup",
		name: "lookup",
		args: map[string]any{"q": "alpha"},
	}})
	assertFinishConsistent(t, got, llm.FinishReasonToolCalls)
}

func TestResponsesStream_ParallelToolCalls(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":""}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_search","call_id":"call_search","name":"search","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_lookup","delta":"{\"q\":\"alpha\"}"}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_search","delta":"{\"q\":\"beta\"}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_lookup","arguments":"{\"q\":\"alpha\"}"}`,
		`{"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_search","arguments":"{\"q\":\"beta\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":"{\"q\":\"alpha\"}"}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_search","call_id":"call_search","name":"search","arguments":"{\"q\":\"beta\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_parallel","model":"model-sol-main","status":"completed","output":[{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":"{\"q\":\"alpha\"}"},{"type":"function_call","id":"fc_search","call_id":"call_search","name":"search","arguments":"{\"q\":\"beta\"}"}]}}`,
	}
	got := collectToolStream(t, events, false)
	assertToolCalls(t, got, []wantToolCall{
		{id: "call_lookup", name: "lookup", args: map[string]any{"q": "alpha"}},
		{id: "call_search", name: "search", args: map[string]any{"q": "beta"}},
	})
	assertFinishConsistent(t, got, llm.FinishReasonToolCalls)
}

func TestResponsesStream_TextPlusToolCall(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_ack","role":"assistant"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_ack","output_index":0,"delta":"ack-token"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_ack","role":"assistant","content":[{"type":"output_text","text":"ack-token"}]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_lookup","delta":"{\"q\":"}`,
		`{"type":"response.function_call_arguments.done","output_index":1,"item_id":"fc_lookup","arguments":"{\"q\":\"alpha\"}"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":"{\"q\":\"alpha\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_mix","model":"model-sol-main","status":"completed","output":[{"type":"message","id":"msg_ack","content":[{"type":"output_text","text":"ack-token"}]},{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":"{\"q\":\"alpha\"}"}]}}`,
	}
	got := collectToolStream(t, events, false)
	if got.text != "ack-token" {
		t.Fatalf("text %q", got.text)
	}
	assertToolCalls(t, got, []wantToolCall{{
		id:   "call_lookup",
		name: "lookup",
		args: map[string]any{"q": "alpha"},
	}})
	assertFinishConsistent(t, got, llm.FinishReasonToolCalls)
}

func TestResponsesStream_ToolCallWithoutAddedEvent(t *testing.T) {
	events := []string{
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_lookup","delta":"{\"q\":\"alpha\"}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_lookup","arguments":"{\"q\":\"alpha\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":"{\"q\":\"alpha\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_no_added","model":"model-sol-main","status":"completed","output":[{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":"{\"q\":\"alpha\"}"}]}}`,
	}
	got := collectToolStream(t, events, false)
	assertToolCalls(t, got, []wantToolCall{{
		id:   "call_lookup",
		name: "lookup",
		args: map[string]any{"q": "alpha"},
	}})
	assertFinishConsistent(t, got, llm.FinishReasonToolCalls)
}

func TestResponsesStream_DuplicateDoneDoesNotDoubleEmit(t *testing.T) {
	item := `{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":"{\"q\":\"alpha\"}"}`
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_lookup","delta":"{\"q\":\"alpha\"}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_lookup","arguments":"{\"q\":\"alpha\"}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		`{"type":"response.completed","response":{"id":"resp_dup","model":"model-sol-main","status":"completed","output":[` + item + `]}}`,
		`{"type":"response.completed","response":{"id":"resp_dup","model":"model-sol-main","status":"completed","output":[` + item + `]}}`,
	}
	got := collectToolStream(t, events, false)
	assertToolCalls(t, got, []wantToolCall{{
		id:   "call_lookup",
		name: "lookup",
		args: map[string]any{"q": "alpha"},
	}})
	assertFinishConsistent(t, got, llm.FinishReasonToolCalls)
}

func TestResponsesStream_FlushUsesArgumentsDoneWhenItemDoneMissing(t *testing.T) {
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_lookup","delta":"{\"q\":"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_lookup","arguments":"{\"q\":\"alpha\"}"}`,
		`{"type":"response.completed","response":{"id":"resp_flush","model":"model-sol-main","status":"completed","output":[{"type":"message","id":"msg_empty","content":[{"type":"output_text","text":""}]}]}}`,
	}
	got := collectToolStream(t, events, false)
	assertToolCalls(t, got, []wantToolCall{{
		id:   "call_lookup",
		name: "lookup",
		args: map[string]any{"q": "alpha"},
	}})
	assertFinishConsistent(t, got, llm.FinishReasonToolCalls)
}

func TestResponsesStream_CompletedHarvestsUnemittedToolCall(t *testing.T) {
	events := []string{
		`{"type":"response.completed","response":{"id":"resp_harvest","model":"model-sol-main","status":"completed","output":[{"type":"function_call","id":"fc_lookup","call_id":"call_lookup","name":"lookup","arguments":"{\"q\":\"alpha\"}"}]}}`,
	}
	got := collectToolStream(t, events, false)
	assertToolCalls(t, got, []wantToolCall{{
		id:   "call_lookup",
		name: "lookup",
		args: map[string]any{"q": "alpha"},
	}})
	assertFinishConsistent(t, got, llm.FinishReasonToolCalls)
}

func TestChatStream_ToolCallUnaffected(t *testing.T) {
	events := []string{
		`{"id":"chat_1","model":"model-sol-main","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":""}}]}}]}`,
		`{"id":"chat_1","model":"model-sol-main","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":\"alpha\"}"}}]}}]}`,
		`{"id":"chat_1","model":"model-sol-main","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	got := collectToolStream(t, events, true)
	assertToolCalls(t, got, []wantToolCall{{
		id:   "call_lookup",
		name: "lookup",
		args: map[string]any{"q": "alpha"},
	}})
	assertFinishConsistent(t, got, llm.FinishReasonToolCalls)
}

type wantToolCall struct {
	id   string
	name string
	args map[string]any
}

type toolStreamResult struct {
	text       string
	calls      []wantToolCall
	stepReason llm.FinishReason
	finish     llm.FinishReason
	sawStep    bool
	sawFinish  bool
	errText    string
}

func collectToolStream(t *testing.T, events []string, chat bool) toolStreamResult {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	opts := []Option{WithAPIKey("dummy-token-not-logged"), WithBaseURL(srv.URL), WithTimeout(2 * time.Second)}
	if chat {
		opts = append(opts, WithChatMode(), WithChatPath("/chat/completions"))
	}
	client := New(opts...)
	sr, err := client.DoStream(context.Background(), llm.GenerateParams{
		Model:    llm.ChatModel("model-sol-main"),
		Messages: []llm.Message{llm.UserMessage("ping")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got toolStreamResult
	for part := range sr.Stream {
		switch p := part.(type) {
		case *llm.TextDeltaPart:
			got.text += p.Text
		case *llm.StreamToolCallPart:
			args, _ := p.Input.(map[string]any)
			got.calls = append(got.calls, wantToolCall{id: p.ToolCallID, name: p.ToolName, args: args})
		case *llm.FinishStepPart:
			got.sawStep = true
			got.stepReason = p.FinishReason
		case *llm.FinishPart:
			got.sawFinish = true
			got.finish = p.FinishReason
		case *llm.ErrorPart:
			got.errText = llm.SafeErrorText(p.Error)
		}
	}
	if got.errText != "" {
		t.Fatalf("stream error %s", got.errText)
	}
	return got
}

func assertToolCalls(t *testing.T, got toolStreamResult, want []wantToolCall) {
	t.Helper()
	if len(got.calls) != len(want) {
		t.Fatalf("tool calls %d, want %d: %+v", len(got.calls), len(want), got.calls)
	}
	for i := range want {
		if got.calls[i].id != want[i].id || got.calls[i].name != want[i].name || !reflect.DeepEqual(got.calls[i].args, want[i].args) {
			raw, _ := json.Marshal(got.calls[i].args)
			t.Fatalf("call[%d] id=%s name=%s args=%s, want id=%s name=%s", i, got.calls[i].id, got.calls[i].name, raw, want[i].id, want[i].name)
		}
	}
}

func assertFinishConsistent(t *testing.T, got toolStreamResult, want llm.FinishReason) {
	t.Helper()
	if !got.sawStep || !got.sawFinish {
		t.Fatalf("missing finish parts step=%v finish=%v", got.sawStep, got.sawFinish)
	}
	if got.stepReason != want || got.finish != want {
		t.Fatalf("finish step=%s final=%s, want %s", got.stepReason, got.finish, want)
	}
}
