package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/agent/pipeline"
	"github.com/kasuganosora/thinkbot/llm"
)

type ownerLive struct {
	active bool
	note   string
	mu     sync.Mutex
}

func (l *ownerLive) IsMessageActive(string, string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

func (l *ownerLive) PublicFailure(string, string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.note
}

func (l *ownerLive) set(active bool) {
	l.mu.Lock()
	l.active = active
	l.mu.Unlock()
}

func TestStreamFailure_OnePresentation(t *testing.T) {
	const modelErr = "模型服务暂时没有返回结果。"
	cases := []struct {
		name   string
		chat   string
		kind   string
		err    error
		want   int
		absent string
	}{
		{name: "empty", chat: "42", kind: "empty", err: &llm.LLMError{Reason: llm.ErrorReasonRateLimit, Message: "slow"}, want: 1},
		{name: "pending-tool", chat: "42", kind: "pending", err: &llm.LLMError{Reason: llm.ErrorReasonRateLimit, Message: "slow"}, want: 1},
		{name: "flushed-tool", chat: "42", kind: "flushed", err: &llm.LLMError{Reason: llm.ErrorReasonRateLimit, Message: "slow"}, want: 1},
		{name: "body", chat: "42", kind: "body", err: &llm.LLMError{Reason: llm.ErrorReasonRateLimit, Message: "slow"}, want: 0, absent: modelErr},
		{name: "cancel", chat: "42", kind: "empty", err: context.Canceled, want: 0, absent: modelErr},
		{name: "nonllm", chat: "42", kind: "empty", err: errString("disk full"), want: 0, absent: modelErr},
		{name: "group", chat: "-100", kind: "group", err: &llm.LLMError{Reason: llm.ErrorReasonRateLimit, Message: "slow"}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sentence := modelErr
			if got := llm.UserFailureText(tc.err); got != "" {
				sentence = got
			}
			n := presentFailure(t, tc.chat, tc.kind, tc.err, sentence)
			if n != tc.want {
				t.Fatalf("presentations %d", n)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func presentFailure(t *testing.T, chat, kind string, cause error, sentence string) int {
	t.Helper()
	var mu sync.Mutex
	texts := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		texts = append(texts, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "sendMessage") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":50}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()

	delay := time.Hour
	if kind == "flushed" {
		delay = 15 * time.Millisecond
	}
	withFastStream(t, delay, time.Hour, time.Minute)
	ch := NewChannel("tg-synth", "bot-synth", Config{
		Token: "123456:TEST", APIBaseURL: srv.URL, StreamReply: true, PollTimeout: 1,
	})
	live := &ownerLive{active: true}
	if _, ok := cause.(*llm.LLMError); ok {
		live.note = sentence
	}
	ch.SetMessageLiveness(live)
	trace := "trace-" + kind + chat
	if kind == "pending" || kind == "flushed" || kind == "body" {
		ch.SetDeltaSource(func(id string) (<-chan Delta, func()) {
			chd := make(chan Delta, 1)
			if id == trace {
				switch kind {
				case "body":
					chd <- Delta{Kind: DeltaText, Text: "already said"}
				default:
					chd <- Delta{Kind: DeltaToolStart, ID: "tool-1", Text: "search"}
				}
			}
			close(chd)
			return chd, func() {}
		})
	}
	logger := zap.NewNop().Sugar()
	tp := nooptrace.NewTracerProvider()
	pipe, err := pipeline.New(nil, tp, noop.NewMeterProvider(), logger)
	if err != nil {
		t.Fatal(err)
	}
	b, err := bot.New(bot.BotParams{
		ID: "bot-synth", Logger: logger, TP: tp,
		Dispatcher: outbound.NewMultiDispatcher(logger, tp),
		Pipeline:   pipe,
		Channels:   []bot.Channel{ch},
	})
	if err != nil {
		t.Fatal(err)
	}
	b.RegisterSender(ch.Name(), ch)
	chatType := "private"
	if kind == "group" {
		chatType = "group"
	}
	ch.openStream(context.Background(), chatID(chat), chatType, trace, 0)
	if kind == "flushed" || kind == "body" {
		waitUntil(t, 2*time.Second, func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, text := range texts {
				if strings.Contains(text, "search") || strings.Contains(text, "already said") {
					return true
				}
			}
			return false
		})
	}
	env := &core.Envelope{Message: core.Message{
		ID: "m1", BotID: "bot-synth", UserID: "u", Source: ch.Name(), Channel: chat,
		TraceID: trace, Metadata: map[string]any{"reply_target": chat},
	}}
	b.OnPipelineError(context.Background(), env, cause)
	live.set(false)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := 0
		for _, text := range texts {
			if strings.Contains(text, sentence) {
				n++
			}
		}
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, text := range texts {
		if strings.Contains(text, sentence) {
			n++
		}
	}
	if n == 0 && (kind == "pending" || kind == "flushed") {
		t.Logf("bodies: %#v", texts)
	}
	return n
}

func chatID(s string) int64 {
	if s == "-100" {
		return -100
	}
	return 42
}

func TestTelegramEmptyStream_DistinguishesCategoriesWithoutUniformInterruption(t *testing.T) {
	const leaked = "DO_NOT_LEAK prompt=SECRET url=https://example.invalid/key token=abcd"
	errs := []*llm.LLMError{
		{Reason: llm.ErrorReasonInvalidRequest, Message: leaked, SafeDetail: "code=invalid_argument"},
		{Reason: llm.ErrorReasonRateLimit, Message: leaked, SafeDetail: "code=1302"},
		{Reason: llm.ErrorReasonQuotaExceeded, Message: leaked, SafeDetail: "code=1113"},
		{Reason: llm.ErrorReasonAuthentication, Message: "provider rejected the credentials"},
	}

	seen := map[string]bool{}
	for _, cause := range errs {
		t.Run(string(cause.Reason), func(t *testing.T) {
			var mu sync.Mutex
			var sent []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				sent = append(sent, string(body))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "sendMessage") {
					_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
			}))
			defer srv.Close()

			// Keep streamStartGrace long (1m) and placeholder delay short (10ms) to exercise:
			// 1. Pushed placeholder ("正在思考…") + fast (<1s) LLM failure while sawActive is still false,
			// 2. Replacing the placeholder immediately with the distinct category note instead of streamErrorNote.
			withFastStream(t, 10*time.Millisecond, time.Hour, time.Minute)
			streamStartGrace = time.Minute
			streamLivenessInterval = 25 * time.Millisecond

			live := &ownerLive{active: true}
			ch := NewChannel("tg-synth", "bot-synth", Config{
				Token: "123456:TEST", APIBaseURL: srv.URL, StreamReply: true, PollTimeout: 1,
			})
			ch.SetMessageLiveness(live)

			logger := zap.NewNop().Sugar()
			tp := nooptrace.NewTracerProvider()
			pipe, err := pipeline.New(nil, tp, noop.NewMeterProvider(), logger)
			if err != nil {
				t.Fatal(err)
			}
			b, err := bot.New(bot.BotParams{
				ID: "bot-synth", Logger: logger, TP: tp,
				Dispatcher: outbound.NewMultiDispatcher(logger, tp),
				Pipeline:   pipe,
				Channels:   []bot.Channel{ch},
				OnLLMFailure: func(_ string, err error) {
					live.mu.Lock()
					live.note = llm.UserFailureText(err)
					live.mu.Unlock()
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			b.RegisterSender(ch.Name(), ch)

			trace := "trace-fast-" + string(cause.Reason)
			// Turn fails BEFORE openStream's watchdog sees IsMessageActive=true on a tick:
			// openStream -> placeholder pushed -> OnPipelineError sets note & marks inactive.
			ch.openStream(context.Background(), 42, "private", trace, 0)
			waitUntil(t, time.Second, func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(sent) >= 1
			})

			live.set(false)
			env := &core.Envelope{Message: core.Message{
				ID: "m1", BotID: "bot-synth", UserID: "u", Source: ch.Name(), Channel: "42",
				TraceID: trace, Metadata: map[string]any{"reply_target": "42"},
			}}
			b.OnPipelineError(context.Background(), env, cause)

			wantSentence := llm.UserFailureText(cause)
			if wantSentence == "" || wantSentence == streamErrorNote || wantSentence == "模型服务暂时没有返回结果。" {
				t.Fatalf("expected distinct category text for %s, got %q", cause.Reason, wantSentence)
			}
			seen[wantSentence] = true

			// Must deliver within 500ms even though streamStartGrace is 1 minute!
			waitUntil(t, 600*time.Millisecond, func() bool {
				mu.Lock()
				defer mu.Unlock()
				for _, body := range sent {
					if strings.Contains(body, wantSentence) {
						return true
					}
				}
				return false
			})

			mu.Lock()
			allBodies := strings.Join(sent, "\n")
			mu.Unlock()
			if strings.Contains(allBodies, streamErrorNote) {
				t.Fatalf("Telegram fell back to uniform interruption placeholder %q: %s", streamErrorNote, allBodies)
			}
			for _, bad := range []string{"DO_NOT_LEAK", "SECRET", "https://example.invalid", "token=abcd", "invalid_argument", "1113", "1302"} {
				if strings.Contains(allBodies, bad) {
					t.Fatalf("Telegram payload leaked %q: %s", bad, allBodies)
				}
			}
		})
	}
	if len(seen) != len(errs) {
		t.Fatalf("expected %d distinct public failure texts, got %d", len(errs), len(seen))
	}
}
