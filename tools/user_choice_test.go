package tools

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
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

// 非 web 平台必须**立即**降级返回，而不是注册问题后白等到超时。
// 回归的表现很隐蔽：telegram/misskey 上 bot 一调本工具就静默卡住 600s。
func TestUserChoiceUnsupportedPlatform(t *testing.T) {
	for _, platform := range []string{"telegram", "misskey"} {
		t.Run(platform, func(t *testing.T) {
			ctx := agenttools.ContextWithMessageMeta(context.Background(),
				agenttools.MessageMeta{BotID: "b1", ChatID: "c1", ChannelType: platform})
			start := time.Now()
			out, err := execUserChoice(&llm.ToolExecContext{Context: ctx},
				choiceInputJSON("选一个", 3, "single"))
			if err != nil {
				t.Fatal(err)
			}
			m, _ := out.(map[string]any)
			if m["status"] != "unsupported" {
				t.Fatalf("status = %v, want unsupported", m["status"])
			}
			if m["platform"] != platform {
				t.Fatalf("platform = %v, want %s", m["platform"], platform)
			}
			// 必须是快速失败：注册+阻塞的话这里会是秒级以上
			if el := time.Since(start); el > 200*time.Millisecond {
				t.Fatalf("降级不够快，耗时 %s —— 说明仍走了注册/阻塞路径", el)
			}
		})
	}
}

// 线上契约回归：progress payload 的选项必须带 id、必须带绝对到期时间 timeoutAt，
// 作答结果必须带 questionId/selectedIds/freeText。
// 这三项分别对应前端的：按 id 渲染可点选项、倒计时、终态锚回卡片。
// 缺任何一项都表现为「卡片出来了但用不了 / 刷新后卡片丢失」。
func TestUserChoiceWireContract(t *testing.T) {
	var mu sync.Mutex
	var got UserChoiceEventPayload
	progress := make(chan struct{}, 1)
	execCtx := &llm.ToolExecContext{
		Context: context.Background(),
		SendProgress: func(content any) {
			mu.Lock()
			if p, ok := content.(UserChoiceEventPayload); ok {
				got = p
			}
			mu.Unlock()
			select {
			case progress <- struct{}{}:
			default:
			}
		},
	}

	done := make(chan any, 1)
	go func() {
		out, err := execUserChoice(execCtx, choiceInputJSON("多选题", 3, "multi"))
		if err != nil {
			done <- err
			return
		}
		done <- out
	}()

	<-progress
	mu.Lock()
	p := got
	mu.Unlock()

	if len(p.Options) != 3 {
		t.Fatalf("options len = %d", len(p.Options))
	}
	seen := map[string]bool{}
	for i, o := range p.Options {
		if o.ID == "" {
			t.Fatalf("option[%d] 缺 id —— 前端按 id 过滤后会得到空选项列表", i)
		}
		if seen[o.ID] {
			t.Fatalf("option id 重复: %q", o.ID)
		}
		seen[o.ID] = true
	}
	if p.TimeoutAt <= time.Now().UnixMilli() {
		t.Fatalf("timeoutAt = %d，应是未来的绝对毫秒时间戳", p.TimeoutAt)
	}
	if p.Mode != "multi" {
		t.Fatalf("mode = %q，后端枚举应原样下发 multi（前端负责翻译）", p.Mode)
	}

	// 用 id→下标翻译（模拟 web 回填路径）后作答
	idx, err := interaction.Default().IndicesForOptionIDs(p.QuestionID,
		[]string{p.Options[0].ID, p.Options[2].ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := interaction.Default().Resolve(p.QuestionID, interaction.Answer{
		Selected: idx, CustomInput: "还想补一句", Via: interaction.ViaWeb,
	}); err != nil {
		t.Fatal(err)
	}

	res := <-done
	if err, ok := res.(error); ok {
		t.Fatal(err)
	}
	m, _ := res.(map[string]any)
	if m["questionId"] != p.QuestionID {
		t.Fatalf("output 缺 questionId（前端无法把终态锚回卡片）: %v", m["questionId"])
	}
	ids, _ := m["selectedIds"].([]string)
	if len(ids) != 2 || ids[0] != p.Options[0].ID || ids[1] != p.Options[2].ID {
		t.Fatalf("selectedIds = %v, want [%s %s]", ids, p.Options[0].ID, p.Options[2].ID)
	}
	if m["freeText"] != "还想补一句" || m["custom_input"] != "还想补一句" {
		t.Fatalf("freeText/custom_input 未同时喂饱: %v / %v", m["freeText"], m["custom_input"])
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
