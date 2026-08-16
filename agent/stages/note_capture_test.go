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
	if note.Payload != "hi there" {
		t.Fatalf("note payload = %q, want %q", note.Payload, "hi there")
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
		if a.Type == core.ActionNote && strings.Contains(payload, "hello from bot") {
			hasNote = true
		}
	}
	if !hasReply {
		t.Fatalf("expected ActionReply with bot text")
	}
	if !hasNote {
		t.Fatalf("expected ActionNote captured from real LLMStage reply")
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
