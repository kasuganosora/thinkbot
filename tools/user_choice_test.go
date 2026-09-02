package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/llm"
)

// choiceInputJSON 构造工具入参（map → any，模拟 LLM 下发的 JSON 参数）。
func choiceInputJSON(question string, nOptions int, mode string) map[string]any {
	opts := make([]map[string]any, nOptions)
	for i := range opts {
		opts[i] = map[string]any{"label": string(rune('A' + i)), "description": "desc"}
	}
	return map[string]any{
		"question": question,
		"options":  opts,
		"mode":     mode,
	}
}

func TestUserChoiceParamsValidation(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
	}{
		{"空 question", choiceInputJSON("", 3, "single")},
		{"零选项", choiceInputJSON("q", 0, "single")},
		{"九选项", choiceInputJSON("q", 9, "single")},
		{"非法 mode", choiceInputJSON("q", 3, "both")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execUserChoice(&llm.ToolExecContext{Context: context.Background()}, tc.in)
			if err == nil {
				t.Fatal("want validation error, got nil")
			}
		})
	}
}

func TestUserChoiceAnswered(t *testing.T) {
	var gotPayload any
	progress := make(chan struct{}, 1)
	execCtx := &llm.ToolExecContext{
		Context: context.Background(),
		SendProgress: func(content any) {
			gotPayload = content
			select {
			case progress <- struct{}{}:
			default:
			}
		},
	}

	done := make(chan any, 1)
	go func() {
		out, err := execUserChoice(execCtx, choiceInputJSON("选一个", 3, "single"))
		if err != nil {
			done <- err
			return
		}
		done <- out
	}()

	// 等 progress 事件，拿到 questionID 后回填。
	<-progress
	payload, ok := gotPayload.(UserChoiceEventPayload)
	if !ok {
		t.Fatalf("progress payload type = %T, want UserChoiceEventPayload", gotPayload)
	}
	if payload.Type != "user_choice" || payload.QuestionID == "" || payload.Mode != "single" {
		t.Fatalf("bad payload: %+v", payload)
	}
	if payload.Timeout != interaction.DefaultTimeoutSecs {
		t.Fatalf("timeout not defaulted: %d", payload.Timeout)
	}
	if err := interaction.Default().Resolve(payload.QuestionID, interaction.Answer{
		Selected: []int{1}, Via: interaction.ViaWeb,
	}); err != nil {
		t.Fatal(err)
	}

	res := <-done
	if err, ok := res.(error); ok {
		t.Fatal(err)
	}
	m, _ := res.(map[string]any)
	if m["status"] != "answered" {
		t.Fatalf("status = %v", m["status"])
	}
	if s, _ := m["selected"].([]int); len(s) != 1 || s[0] != 1 {
		t.Fatalf("selected = %v", m["selected"])
	}
	if labels, _ := m["selected_labels"].([]string); len(labels) != 1 || labels[0] != "B" {
		t.Fatalf("selected_labels = %v", m["selected_labels"])
	}
}

func TestUserChoiceTimeout(t *testing.T) {
	in := choiceInputJSON("等超时", 2, "single")
	in["timeout_secs"] = 1
	start := time.Now()
	out, err := execUserChoice(&llm.ToolExecContext{Context: context.Background()}, in)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := out.(map[string]any)
	if m["status"] != "timeout" {
		t.Fatalf("status = %v, want timeout", m["status"])
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("timeout fired too early")
	}
}

// 验证 progress payload 的 JSON 可序列化（web 前端消费路径）。
func TestUserChoicePayloadJSON(t *testing.T) {
	p := UserChoiceEventPayload{
		Type: "user_choice", QuestionID: "uc-x", Question: "q",
		Options: []interaction.Option{{Label: "a"}}, Mode: "multi",
		Via: "web", Timeout: 600,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back["questionId"] != "uc-x" || back["mode"] != "multi" {
		t.Fatalf("json roundtrip mismatch: %s", b)
	}
}
