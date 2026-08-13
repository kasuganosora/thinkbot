package stages

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// lurkStubProvider 与 suppressStubProvider 类似，但额外记录收到的 system prompt，
// 用于验证潜水模式是否注入了「观察者 + soul」prompt。
//
// texts 支持按调用序返回不同输出，用于验证「格式违约 → 重试 → 合规」的控制流。
// 单元素时等价于每次都返回同一段文本。
type lurkStubProvider struct {
	texts        []string
	finishReason llm.FinishReason
	called       int
	gotSystem    string
	gotTools     int
	gotSystems   []string
	gotFormat    *llm.ResponseFormat
	gotMaxTokens []int
	gotTemps     []float64
}

func (p *lurkStubProvider) Name() string { return "stub" }

func (p *lurkStubProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	idx := p.called
	p.called++
	p.gotSystem = params.System
	p.gotTools = len(params.Tools)
	p.gotSystems = append(p.gotSystems, params.System)
	p.gotFormat = params.ResponseFormat
	if params.MaxTokens != nil {
		p.gotMaxTokens = append(p.gotMaxTokens, *params.MaxTokens)
	}
	if params.Temperature != nil {
		p.gotTemps = append(p.gotTemps, *params.Temperature)
	}

	text := ""
	if len(p.texts) > 0 {
		if idx < len(p.texts) {
			text = p.texts[idx]
		} else {
			text = p.texts[len(p.texts)-1]
		}
	}
	fr := p.finishReason
	if fr == "" {
		fr = llm.FinishReasonStop
	}
	return &llm.GenerateResult{Text: text, FinishReason: fr}, nil
}

func (p *lurkStubProvider) DoStream(_ context.Context, _ llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("stream not supported in this stub")
}

func noteActions(env *core.Envelope) []core.Action {
	var out []core.Action
	for _, a := range env.Actions() {
		if a.Type == core.ActionNote {
			out = append(out, a)
		}
	}
	return out
}

// runLurk 跑一次潜水流程，返回 envelope 供断言。
func runLurk(t *testing.T, p *lurkStubProvider, soul string) *core.Envelope {
	t.Helper()
	stage := newSuppressTestStage(p)
	env := core.NewEnvelope(core.Message{
		ID: "lk", Text: "帖子正文", Source: "misskey-ch", Channel: "misskey:timeline", UserID: "u1",
	})
	env.Set(core.KVLurkMode, true)
	if soul != "" {
		env.Set(core.KVSoulContent, soul)
	}
	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}
	return out
}

// TestLLMStage_LurkModeCapturesNoteNotReply 是「潜水要学到东西」的核心回归测试。
//
// 场景：渠道只读（潜水），lurk-detect enricher 已设置 KVLurkMode。
// 期望：LLM 仍被调用（照样思考），但**不产出 ActionReply**（绝不发帖），
// 而是把契约 JSON 里的 note 作为 ActionNote（内部学习笔记）写入 L0。
func TestLLMStage_LurkModeCapturesNoteNotReply(t *testing.T) {
	p := &lurkStubProvider{texts: []string{
		`{"remember":true,"note":"这人在做 Go + misskey 集成，偏好 sqlite，值得记。","ephemeral":false,"speaker_handle":"@luna","importance":4}`,
	}}
	out := runLurk(t, p, "你是栞娜，直接有用、有自己判断的数字搭档。")

	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("lurk mode must not produce ActionReply, got %d: %+v", len(got), got)
	}
	notes := noteActions(out)
	if len(notes) != 1 {
		t.Fatalf("lurk mode should capture exactly 1 learning note, got %d", len(notes))
	}
	n := notes[0]
	if n.Payload != "这人在做 Go + misskey 集成，偏好 sqlite，值得记。" {
		t.Errorf("note payload should be the json note field, got %v", n.Payload)
	}
	if n.Metadata["category"] != "lurk" {
		t.Errorf("note category should be lurk, got %v", n.Metadata["category"])
	}
	// speaker=observer 是 dreaming 归因护栏的依据：既保留观察记忆，又不把 @handle
	// 洗成「用户/此人」，也不与「bot 自述不晋升」的护栏冲突。
	if n.Metadata["speaker"] != "observer" {
		t.Errorf("lurk note must carry speaker=observer, got %v", n.Metadata["speaker"])
	}
	if n.Metadata["ephemeral"] != false {
		t.Errorf("ephemeral should be false, got %v", n.Metadata["ephemeral"])
	}
	// importance 必须是 0.0~1.0 的 float64：note_handler 只接受 float64，
	// 传 int 会被静默忽略并落回默认 0.5。4 → 0.8。
	imp, ok := n.Metadata["importance"].(float64)
	if !ok {
		t.Fatalf("importance must be float64 for note_handler to accept it, got %T", n.Metadata["importance"])
	}
	if imp != 0.8 {
		t.Errorf("importance 4 should map to 0.8, got %v", imp)
	}
	if n.Metadata["speaker_handle"] != "@luna" {
		t.Errorf("speaker_handle should be preserved, got %v", n.Metadata["speaker_handle"])
	}
	// 潜水学习笔记必须以 bot 全局 scope 落库（Channel 为空），才能跨渠道被召回。
	if n.Channel != "" {
		t.Errorf("lurk learning note should use bot-scope (Channel empty), got %q", n.Channel)
	}
	// 合规输出必须一次过，不触发重试。
	if p.called != 1 {
		t.Errorf("valid json should need exactly 1 call, called=%d", p.called)
	}
}

// TestLLMStage_LurkModeSkipsRememberFalse 验证 remember=false 时不写噪声笔记，且不重试。
//
// 这是「语言无关」的核心：跳过判定来自 json 布尔，而非匹配某种语言的「无」。
func TestLLMStage_LurkModeSkipsRememberFalse(t *testing.T) {
	p := &lurkStubProvider{texts: []string{`{"remember": false}`}}
	out := runLurk(t, p, "")

	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("lurk mode must not produce ActionReply, got %+v", got)
	}
	if notes := noteActions(out); len(notes) != 0 {
		t.Fatalf("remember=false should skip note, got %+v", notes)
	}
	if p.called != 1 {
		t.Errorf("remember=false is a valid contract response, must not retry, called=%d", p.called)
	}
}

// TestLLMStage_LurkModeLanguageAgnostic 是本次重构的关键回归测试。
//
// 历史问题：早期用 lurkSkipMarkers 枚举 [NONE]/[无]/[なし] 等自然语言标记，
// 补掉日语后印地语等又会漏，是打地鼠。现在跳过与否只看 json 的 remember 布尔，
// 因此**任意语言**的 note 正文都能正常沉淀，任意语言的「无」都不再需要枚举。
func TestLLMStage_LurkModeLanguageAgnostic(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantNote   string
		wantStored bool
	}{
		{
			name:       "japanese_note_stored",
			text:       `{"remember":true,"note":"@blogtalk は最近 Rust を勉強していて、所有権で苦労している。","ephemeral":false,"speaker_handle":"@blogtalk","importance":3}`,
			wantNote:   "@blogtalk は最近 Rust を勉強していて、所有権で苦労している。",
			wantStored: true,
		},
		{
			name:       "hindi_note_stored",
			text:       `{"remember":true,"note":"@dev को Python पसंद है और वे डेटा साइंस पर काम कर रहे हैं।","ephemeral":false,"speaker_handle":"@dev","importance":3}`,
			wantNote:   "@dev को Python पसंद है और वे डेटा साइंस पर काम कर रहे हैं।",
			wantStored: true,
		},
		{
			name:       "korean_note_stored",
			text:       `{"remember":true,"note":"@kim은 Kubernetes 운영을 담당하고 있다.","ephemeral":false,"speaker_handle":"@kim","importance":3}`,
			wantNote:   "@kim은 Kubernetes 운영을 담당하고 있다.",
			wantStored: true,
		},
		{
			// 日语「无」不再靠枚举识别：模型按契约给出 remember=false 即跳过。
			name:       "japanese_skip_via_boolean",
			text:       `{"remember":false}`,
			wantStored: false,
		},
		{
			// 印地语场景同理 —— 无需为新语言加任何代码。
			name:       "hindi_skip_via_boolean",
			text:       `{"remember": false}`,
			wantStored: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &lurkStubProvider{texts: []string{tc.text}}
			out := runLurk(t, p, "")
			notes := noteActions(out)
			if tc.wantStored {
				if len(notes) != 1 {
					t.Fatalf("expected note stored, got %d", len(notes))
				}
				if notes[0].Payload != tc.wantNote {
					t.Errorf("note payload mismatch:\n got: %v\nwant: %v", notes[0].Payload, tc.wantNote)
				}
			} else if len(notes) != 0 {
				t.Fatalf("expected skip, got %+v", notes)
			}
		})
	}
}

// TestLLMStage_LurkModeEphemeralFlagged 验证时效性内容被结构化标记（ephemeral=true），
// 由 dreaming 晋升阶段据此拒绝进 L1 —— 替代过去按「开播/放送」等短语正则判定（语言枚举）。
func TestLLMStage_LurkModeEphemeralFlagged(t *testing.T) {
	p := &lurkStubProvider{texts: []string{
		`{"remember":true,"note":"TANK CHAIR というアニメが4月7日から放送開始。","ephemeral":true,"speaker_handle":"@blogtalk","importance":1}`,
	}}
	out := runLurk(t, p, "")
	notes := noteActions(out)
	if len(notes) != 1 {
		t.Fatalf("ephemeral note should still be captured to L0, got %d", len(notes))
	}
	if notes[0].Metadata["ephemeral"] != true {
		t.Errorf("ephemeral must be true so dreaming can refuse promotion, got %v", notes[0].Metadata["ephemeral"])
	}
}

// TestLLMStage_LurkModeRetriesOnFormatViolation 验证格式违约会重试，且重试成功后正常落库。
// 重试提示里必须回贴上次的非法输出，帮助模型自我纠正。
func TestLLMStage_LurkModeRetriesOnFormatViolation(t *testing.T) {
	p := &lurkStubProvider{texts: []string{
		"[なし]", // 第 1 次：非法（旧世界的空占位标记，现在一律按违约处理）
		`{"remember":true,"note":"@blogtalk 在学 Rust。","ephemeral":false,"speaker_handle":"@blogtalk","importance":3}`,
	}}
	out := runLurk(t, p, "")

	if p.called != 2 {
		t.Fatalf("expected 1 retry after format violation, called=%d", p.called)
	}
	notes := noteActions(out)
	if len(notes) != 1 {
		t.Fatalf("retry should succeed and store note, got %d", len(notes))
	}
	if notes[0].Payload != "@blogtalk 在学 Rust。" {
		t.Errorf("unexpected note: %v", notes[0].Payload)
	}
	// 重试的 system prompt 必须包含纠正指令与上次的非法输出回贴。
	if len(p.gotSystems) < 2 {
		t.Fatalf("expected 2 recorded system prompts, got %d", len(p.gotSystems))
	}
	retrySys := p.gotSystems[1]
	if !strings.Contains(retrySys, "FORMAT VIOLATION") {
		t.Errorf("retry prompt must contain the correction directive, got: %q", retrySys)
	}
	if !strings.Contains(retrySys, "[なし]") {
		t.Errorf("retry prompt must echo back the offending output, got: %q", retrySys)
	}
}

// TestLLMStage_LurkModeAbandonsAfterRetries 验证重试耗尽后放弃落库（安全失败）。
//
// 宁可丢一条观察，也不把无法解析的半成品写进记忆 —— 这是本方案刻意选择的失败方向。
func TestLLMStage_LurkModeAbandonsAfterRetries(t *testing.T) {
	p := &lurkStubProvider{texts: []string{"这不是 json，纯自然语言输出。"}}
	out := runLurk(t, p, "")

	if notes := noteActions(out); len(notes) != 0 {
		t.Fatalf("must abandon (store nothing) after retries exhausted, got %+v", notes)
	}
	// 首次 + lurkMaxRetries 次重试
	if want := 1 + lurkMaxRetries; p.called != want {
		t.Errorf("expected %d total calls, called=%d", want, p.called)
	}
}

// TestLLMStage_LurkModeRetriesOnEmptyNote 验证 remember=true 但 note 为空时视为违约去重试，
// 而不是当成 skip 静默丢弃 —— 否则模型漏一个字段就等于悄悄吞掉一条真实观察。
func TestLLMStage_LurkModeRetriesOnEmptyNote(t *testing.T) {
	p := &lurkStubProvider{texts: []string{
		`{"remember":true,"note":"   "}`,
		`{"remember":true,"note":"补上了：对方在用 fedora。","ephemeral":false,"speaker_handle":"@a","importance":2}`,
	}}
	out := runLurk(t, p, "")
	if p.called != 2 {
		t.Fatalf("empty note must trigger retry, called=%d", p.called)
	}
	if notes := noteActions(out); len(notes) != 1 {
		t.Fatalf("expected note stored after retry, got %d", len(notes))
	}
}

// TestLLMStage_LurkModeMissingRememberIsViolation 验证缺 remember 字段按违约处理。
// 不可默认成 false —— 那会让模型漏字段变成静默丢数据。
// 同时也钉住「json_schema 不可依赖」的结论：字段校验必须在 Go 侧做。
func TestLLMStage_LurkModeMissingRememberIsViolation(t *testing.T) {
	p := &lurkStubProvider{texts: []string{`{"note":"漏了 remember 字段","importance":3}`}}
	out := runLurk(t, p, "")
	if notes := noteActions(out); len(notes) != 0 {
		t.Fatalf("missing remember must not be stored, got %+v", notes)
	}
	if want := 1 + lurkMaxRetries; p.called != want {
		t.Errorf("missing remember must be treated as violation and retried, called=%d want=%d", p.called, want)
	}
}

// TestLLMStage_LurkModeTruncationBoostsBudget 验证截断（finish_reason=length）时
// 重试会**放大 max_tokens**，而不是原样重推。
//
// 实测 glm-5.2：max_tokens 过小时思考会吃光预算导致 content 为空，
// 盲重试必然复现同样结果并白烧 token。这是「区分失败类型」的核心价值。
func TestLLMStage_LurkModeTruncationBoostsBudget(t *testing.T) {
	base := 64
	p := &lurkStubProvider{texts: []string{""}, finishReason: llm.FinishReasonLength}
	stage := NewLLMStage("llm", p, LLMConfig{
		MaxTokens: &base,
		MessageBuilder: func(msg core.Message) []llm.Message {
			return []llm.Message{llm.UserMessage(msg.Text)}
		},
	}, nil, nil)
	env := core.NewEnvelope(core.Message{
		ID: "lk-trunc", Text: "帖子", Source: "misskey-ch", Channel: "misskey:timeline", UserID: "u1",
	})
	env.Set(core.KVLurkMode, true)
	if _, err := stage.Process(context.Background(), env); err != nil {
		t.Fatalf("process err: %v", err)
	}
	if len(p.gotMaxTokens) < 2 {
		t.Fatalf("expected at least 2 calls recording max_tokens, got %v", p.gotMaxTokens)
	}
	if p.gotMaxTokens[0] != base {
		t.Errorf("first call should use configured budget %d, got %d", base, p.gotMaxTokens[0])
	}
	if p.gotMaxTokens[1] <= base {
		t.Errorf("truncation retry must boost budget above %d, got %d", base, p.gotMaxTokens[1])
	}
}

// TestLLMStage_LurkModeRequestsJSONFormat 验证潜水请求带上 json_object 响应格式，
// 且采样温度被压低（结构化输出不需要创造性，低温提升合规率）。
func TestLLMStage_LurkModeRequestsJSONFormat(t *testing.T) {
	p := &lurkStubProvider{texts: []string{`{"remember":false}`}}
	runLurk(t, p, "")

	if p.gotFormat == nil {
		t.Fatal("lurk request must set ResponseFormat")
	}
	if p.gotFormat.Type != llm.ResponseFormatJSONObject {
		t.Errorf("lurk must use json_object (json_schema is accepted but NOT enforced by bigmodel), got %v", p.gotFormat.Type)
	}
	if len(p.gotTemps) == 0 || p.gotTemps[0] > 0.2 {
		t.Errorf("lurk should use low temperature for format compliance, got %v", p.gotTemps)
	}
}

// TestLurkPromptMentionsJSON 钉住 OpenAI 兼容 json_object 模式的硬约束：
// 提示词内必须出现 "json" 字样，否则部分供应商直接报错。
// 防止后人改写文案时无意破坏该功能。
func TestLurkPromptMentionsJSON(t *testing.T) {
	if !strings.Contains(strings.ToLower(lurkObserverInstruction), "json") {
		t.Error("lurkObserverInstruction must mention 'json' — required by OpenAI-compatible json_object mode")
	}
}

// TestParseLurkOutput 直接覆盖解析器的格式噪音容错与边界。
// 容错只允许与语言无关的格式处理（围栏、前后缀文本），不得引入自然语言枚举。
func TestParseLurkOutput(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    lurkParseOutcome
		wantAll string
	}{
		{"plain_skip", `{"remember":false}`, lurkParseSkip, ""},
		{"plain_ok", `{"remember":true,"note":"abc"}`, lurkParseOK, "abc"},
		{"code_fence", "```json\n{\"remember\":true,\"note\":\"fenced\"}\n```", lurkParseOK, "fenced"},
		{"surrounding_text", `Here you go: {"remember":true,"note":"noisy"} hope that helps`, lurkParseOK, "noisy"},
		{"thinking_prefix", "<think>考虑一下</think>{\"remember\":true,\"note\":\"thought\"}", lurkParseOK, "thought"},
		{"brace_in_string", `{"remember":true,"note":"has } brace"}`, lurkParseOK, "has } brace"},
		{"not_json", "なし", lurkParseInvalid, ""},
		{"empty", "", lurkParseInvalid, ""},
		{"missing_remember", `{"note":"x"}`, lurkParseInvalid, ""},
		{"empty_note", `{"remember":true,"note":""}`, lurkParseInvalid, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, outcome := parseLurkOutput(tc.raw)
			if outcome != tc.want {
				t.Fatalf("outcome=%v want=%v (raw=%q)", outcome, tc.want, tc.raw)
			}
			if tc.wantAll != "" && out.Note != tc.wantAll {
				t.Errorf("note=%q want=%q", out.Note, tc.wantAll)
			}
		})
	}
}

// TestParseLurkOutputClampsImportance 验证 importance 越界被收敛，避免脏值流入 dreaming 打分。
func TestParseLurkOutputClampsImportance(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{`{"remember":true,"note":"x","importance":0}`, 1},
		{`{"remember":true,"note":"x","importance":99}`, 5},
		{`{"remember":true,"note":"x","importance":3}`, 3},
	} {
		out, outcome := parseLurkOutput(tc.raw)
		if outcome != lurkParseOK {
			t.Fatalf("expected OK for %q", tc.raw)
		}
		if out.Importance != tc.want {
			t.Errorf("importance=%d want=%d (raw=%q)", out.Importance, tc.want, tc.raw)
		}
	}
}

// TestLurkImportanceToScore 验证 1~5 整数重要度到 0.0~1.0 的量纲转换。
// 这层转换是必需的：note_handler 只接受 float64，传 int 会被静默忽略。
func TestLurkImportanceToScore(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want float64
	}{
		{1, 0.2}, {2, 0.4}, {3, 0.6}, {4, 0.8}, {5, 1.0},
		{0, 0.2},  // 越界收敛
		{99, 1.0}, // 越界收敛
	} {
		if got := lurkImportanceToScore(tc.in); got != tc.want {
			t.Errorf("lurkImportanceToScore(%d)=%v want=%v", tc.in, got, tc.want)
		}
	}
}

// TestLLMStage_LurkModeUsesSoulObserverPrompt 验证潜水 prompt 结合了 soul + 观察者指令，
// 且工具被清空（潜水观察者不调用工具，杜绝经工具发帖的副作用）。
func TestLLMStage_LurkModeUsesSoulObserverPrompt(t *testing.T) {
	soul := "你是栞娜，直接有用、有自己判断的数字搭档。"
	p := &lurkStubProvider{texts: []string{`{"remember":true,"note":"对方在用 fedora。","importance":2}`}}
	runLurk(t, p, soul)

	if !strings.Contains(p.gotSystem, soul) {
		t.Errorf("lurk system prompt must embed soul content, got: %q", p.gotSystem)
	}
	if !strings.Contains(p.gotSystem, "OBSERVER MODE") {
		t.Errorf("lurk system prompt must contain observer instruction, got: %q", p.gotSystem)
	}
	if p.gotTools != 0 {
		t.Errorf("lurk mode must disable tools, got %d tools", p.gotTools)
	}
}
