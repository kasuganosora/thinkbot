package stages

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// fakeReplyProvider 返回固定回复，用于验证 NoteCaptureMiddleware 在真实
// LLMStage 路径上把回复捕获为 ActionNote。
type fakeReplyProvider struct{}

func (fakeReplyProvider) Name() string { return "fake" }

func (fakeReplyProvider) DoGenerate(ctx context.Context, p llm.GenerateParams) (*llm.GenerateResult, error) {
	return &llm.GenerateResult{
		Text:         "hello from bot",
		FinishReason: llm.FinishReasonStop,
	}, nil
}

func (fakeReplyProvider) DoStream(ctx context.Context, p llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("stream not supported")
}

// fakeReplyStage 只发出一个 ActionReply 的假 Stage，用于隔离测试中间件逻辑。
// 注意其 text 刻意与入站 Message.Text 不同，以便区分「捕获的是用户原文还是 bot 回复」。
type fakeReplyStage struct{ text string }

func (f fakeReplyStage) Name() string { return "fake-reply" }

func (f fakeReplyStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	env.AddAction(core.Action{
		Type:    core.ActionReply,
		Channel: env.Message.Channel,
		UserID:  env.Message.UserID,
		Payload: f.text,
	})
	return env, nil
}

func TestNoteCaptureMiddleware_Isolated(t *testing.T) {
	mw := NoteCaptureMiddleware("exchange", nil)
	next := core.Stage(fakeReplyStage{text: "hi there"})
	stage := mw(next)

	msg := core.Message{ID: "m1", BotID: "bot-x", Source: "web", Channel: "ch-x", UserID: "u1", Text: "hello"}
	env := core.NewEnvelope(msg)

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var note *core.Action
	for _, a := range out.Actions() {
		if a.Type == core.ActionNote {
			note = &a
			break
		}
	}
	if note == nil {
		t.Fatalf("expected an ActionNote to be captured, got none; actions=%d", len(out.Actions()))
	}
	// 捕获的是**用户原文**（env.Message.Text="hello"），不是 bot 回复（"hi there"）。
	// 这是刻意设计：捕获 bot 回复会让 dreaming 把 bot 的发言误记成用户的事实
	// （说话人归属错误，历史 bug：把 bot 对《零之使魔》的安利错记成「用户熟悉该作」）。
	// 见 note_capture.go 中 NoteCaptureMiddleware 的注释。
	if note.Payload != "hello" {
		t.Fatalf("note payload = %q, want %q（应捕获用户原文）", note.Payload, "hello")
	}
	if strings.Contains(note.Payload.(string), "hi there") {
		t.Fatalf("note payload 混入了 bot 回复：%q", note.Payload)
	}
	if note.Metadata["category"] != "exchange" {
		t.Fatalf("note category = %v, want exchange", note.Metadata["category"])
	}
	if note.Metadata["bot_id"] != "bot-x" || note.Metadata["message_id"] != "m1" {
		t.Fatalf("note metadata = %v", note.Metadata)
	}
}

func TestNoteCaptureMiddleware_OnRealLLMStage(t *testing.T) {
	llmStage := NewLLMStage("llm", fakeReplyProvider{}, LLMConfig{
		Model:        llm.ChatModel("fake"),
		MaxSteps:     1,
		HardMaxSteps: 1,
	}, noop.NewTracerProvider(), zap.NewNop().Sugar())

	stage := NoteCaptureMiddleware("exchange", nil)(llmStage)

	msg := core.Message{ID: "m2", BotID: "bot-y", Source: "web", Channel: "ch-y", UserID: "u2", Text: "ping"}
	env := core.NewEnvelope(msg)

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("actions: %d", len(out.Actions()))
	for i, a := range out.Actions() {
		t.Logf("  action[%d] type=%s payload=%q meta=%v", i, a.Type, a.Payload, a.Metadata)
	}
	hasReply, hasNote := false, false
	for _, a := range out.Actions() {
		payload, _ := a.Payload.(string)
		if a.Type == core.ActionReply && strings.Contains(payload, "hello from bot") {
			hasReply = true
		}
		// 捕获的是用户原文 msg.Text="ping"，不是 bot 回复 "hello from bot"。
		if a.Type == core.ActionNote && strings.Contains(payload, "ping") {
			hasNote = true
		}
	}
	if !hasReply {
		t.Fatalf("expected ActionReply with bot text")
	}
	if !hasNote {
		t.Fatalf("expected ActionNote capturing the user message, got none")
	}
	// 反向断言：note 里绝不能出现 bot 回复，否则说话人归属就错了。
	for _, a := range out.Actions() {
		if a.Type != core.ActionNote {
			continue
		}
		payload, _ := a.Payload.(string)
		if strings.Contains(payload, "hello from bot") {
			t.Fatalf("ActionNote 捕获了 bot 回复而非用户原文：%q", payload)
		}
	}
}

// spyEventWriter 记录事件流写入次数与最后一次内容，用于验证「一条用户消息只写一次」。
type spyEventWriter struct {
	mu    sync.Mutex
	count int
	last  CapturedUserMessage
}

func (s *spyEventWriter) WriteUserMessageEvent(_ context.Context, msg CapturedUserMessage) error {
	s.mu.Lock()
	s.count++
	s.last = msg
	s.mu.Unlock()
	return nil
}

// twoReplyStage 一次性发出两个 ActionReply，用于验证多回复不会被重复捕获。
type twoReplyStage struct{}

func (twoReplyStage) Name() string { return "two-reply" }

func (twoReplyStage) Process(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
	env.AddAction(core.Action{Type: core.ActionReply, Channel: env.Message.Channel, UserID: env.Message.UserID, Payload: "reply-1"})
	env.AddAction(core.Action{Type: core.ActionReply, Channel: env.Message.Channel, UserID: env.Message.UserID, Payload: "reply-2"})
	return env, nil
}

func TestNoteCaptureMiddleware_OneCapturePerMessage(t *testing.T) {
	spy := &spyEventWriter{}
	mw := NoteCaptureMiddleware("exchange", spy)
	stage := mw(twoReplyStage{})

	msg := core.Message{ID: "m9", BotID: "bot-z", Source: "web", Channel: "ch-z", UserID: "u9", Text: "hello"}
	env := core.NewEnvelope(msg)

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	notes := 0
	for _, a := range out.Actions() {
		if a.Type == core.ActionNote {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("expected exactly 1 ActionNote per user message, got %d", notes)
	}
	if spy.count != 1 {
		t.Fatalf("expected exactly 1 event-stream write per user message, got %d", spy.count)
	}
	if spy.last.MessageID != "m9" || spy.last.Content != "hello" {
		t.Fatalf("event payload mismatch: %+v", spy.last)
	}
}

// TestNoteCaptureMiddleware_CrossProcessDedup 验证同一条入站消息（相同 message_id）
// 即便被多次 ingest（mention 流 + timeline 流 + 重连重放），也只落一条 exchange 记忆。
// 对应 2026-08-25 排查：aqailbnedi7a0126 同 note 7 秒内被写 4 条 exchange 记忆。
func TestNoteCaptureMiddleware_CrossProcessDedup(t *testing.T) {
	spy := &spyEventWriter{}
	mw := NoteCaptureMiddleware("exchange", spy)
	stage := mw(twoReplyStage{})

	msg := core.Message{ID: "m-dup", BotID: "bot-z", Source: "web", Channel: "ch-z", UserID: "u9", Text: "same message"}
	env := core.NewEnvelope(msg)

	if _, err := stage.Process(context.Background(), env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.count != 1 {
		t.Fatalf("first pass: expected 1 capture, got %d", spy.count)
	}

	// 第二次（同 message_id，模拟重放）：应被去重。
	if _, err := stage.Process(context.Background(), env); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.count != 1 {
		t.Fatalf("second pass (same message_id): expected still 1 capture, got %d", spy.count)
	}

	// 不同 message_id：应再捕获一条。
	msg2 := core.Message{ID: "m-other", BotID: "bot-z", Source: "web", Channel: "ch-z", UserID: "u9", Text: "other message"}
	env2 := core.NewEnvelope(msg2)
	if _, err := stage.Process(context.Background(), env2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.count != 2 {
		t.Fatalf("different message_id: expected 2 captures, got %d", spy.count)
	}
}

// TestNormalizeExchangeText 验证捕获为 L0 对话记忆前，渠道层为 LLM prompt 注入的
// 装饰噪声（[Timeline]/[DM]/[对方是 Bot 账号]/[note_id: ...]）被剥离，且用户真实
// 正文（含 Misskey 渲染的 "[Reply to ...]"）被保留。
func TestNormalizeExchangeText(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// 时间线装饰前缀 + note_id 后缀
		{"[Timeline] @luna: 来来 测试下你的记忆力\n[note_id: aq0uojwxdi7a00m]", "来来 测试下你的记忆力"},
		// DM 装饰前缀
		{"[DM] @kanna: 在吗", "在吗"},
		// Bot 账号标注前缀
		{"[对方是 Bot 账号 @ce_observe] [Timeline] @ce_observe: A 股股王长鑫科技", "A 股股王长鑫科技"},
		// 多装饰叠加
		{"[对方是 Bot 账号 @x] [Timeline] @x: hello\n[note_id: abc123]", "hello"},
		// 无装饰：原样返回（含 Misskey 渲染的正文 "[Reply to ...]" 必须保留）
		{"[Reply to 栞娜: @luna 来喵！我先下，占据中心 ✕", "[Reply to 栞娜: @luna 来喵！我先下，占据中心 ✕"},
		// 普通正文
		{"外面好像要下大雨的样子呢", "外面好像要下大雨的样子呢"},
	}
	for _, c := range cases {
		if got := normalizeExchangeText(c.in); got != c.want {
			t.Errorf("normalizeExchangeText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
