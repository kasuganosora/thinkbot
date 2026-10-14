package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/agent/outbound"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
)

type chatStreamTestEnv struct {
	history *ChatHistoryService
	svc     *BotService
	bus     *outbound.MemoryEventBus
	channel *WebChannel
	queue   *inbound.Ingress
	http    *httptest.Server
	session string
}

func newChatStreamTestEnv(t *testing.T) *chatStreamTestEnv {
	t.Helper()
	history := newReliabilityHistory(t)
	session := dao.ChatSession{BotID: "bot", Title: "chat"}
	if err := history.db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	lg := zap.NewNop().Sugar()
	bus := outbound.NewMemoryEventBus(outbound.DefaultMemoryEventBusConfig(), lg)
	channel := NewWebChannel("web-bot", "bot")
	channel.chatHistory = history
	queue := inbound.NewIngress(inbound.DefaultIngressConfig(), lg, nooptrace.NewTracerProvider())
	if err := channel.Start(context.Background(), queue); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(nil)
	svc := &BotService{
		db: history.db, chatHistory: history, logger: lg, store: store, eventBus: bus,
		channels:        map[string]*WebChannel{"bot": channel},
		messageSessions: make(map[string]string), messageCancels: make(map[string]context.CancelFunc), messageStarts: make(map[string]time.Time),
	}
	s := &Server{botSvc: svc, chatHistory: history, logger: lg, store: store}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(ContextKeyUser, &dao.User{ID: 1}) })
	router.POST("/api/chat/send", s.handleChatSend)
	router.GET("/api/chat/resume", s.handleChatResume)
	router.GET("/api/chat/history", s.handleChatHistory)
	server := httptest.NewServer(router)
	env := &chatStreamTestEnv{history: history, svc: svc, bus: bus, channel: channel, queue: queue, http: server, session: fmt.Sprint(session.ID)}
	t.Cleanup(func() {
		server.CloseClientConnections()
		bus.Close()
		_ = channel.Stop(context.Background())
		queue.Close()
		managedChatTurns.Range(func(_, value any) bool {
			turn := value.(*chatTurn)
			if turn.svc == svc {
				select {
				case <-turn.done:
				case <-time.After(time.Second):
					t.Error("chat persister did not stop during cleanup")
				}
			}
			return true
		})
		server.Close()
	})
	return env
}

func (env *chatStreamTestEnv) send(t *testing.T, ctx context.Context) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"botId": "bot", "sessionId": env.session, "text": "hello"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.http.URL+"/api/chat/send", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readChatStream(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read SSE: %v, body=%s", err, body)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d: %s", response.StatusCode, body)
	}
	return string(body)
}

func (env *chatStreamTestEnv) publish(trace string, kind outbound.EventType, data map[string]any) {
	env.bus.Publish(context.Background(), outbound.Event{BotID: "bot", TraceID: trace, Type: kind, Data: data})
}

func (env *chatStreamTestEnv) reply(trace, content string) {
	_ = env.channel.Send(context.Background(), core.Action{Type: core.ActionReply, Payload: content,
		Metadata: map[string]any{"trace_id": trace, "chat_session_id": env.session}})
	env.publish(trace, outbound.EventMessageDone, nil)
}

func TestChatSend_PersistsFinalAndTrailingToolsBeforeDone(t *testing.T) {
	env := newChatStreamTestEnv(t)
	trace := make(chan string, 1)
	go func() {
		msg := (<-env.queue.C()).Message
		trace <- msg.TraceID
		env.publish(msg.TraceID, outbound.EventLLMTextDelta, map[string]any{"text": "unclean preview"})
		env.publish(msg.TraceID, outbound.EventLLMToolCall, map[string]any{"toolCallId": "tool", "tool": "user_choice", "input": map[string]any{"question": "Continue?"}})
		env.publish(msg.TraceID, outbound.EventLLMToolResult, map[string]any{"toolCallId": "tool", "tool": "user_choice", "output": map[string]any{"status": "timeout"}})
		env.reply(msg.TraceID, "Final cleaned answer")
	}()
	body := readChatStream(t, env.send(t, context.Background()))
	for _, wanted := range []string{"event: done", `"text":"Final cleaned answer"`, `"status":"timeout"`, "id: "} {
		if !strings.Contains(body, wanted) {
			t.Errorf("SSE missing %s: %s", wanted, body)
		}
	}
	// No polling: receipt of done must mean history is already committed.
	msg := loadAssistant(t, env.history, <-trace)
	if msg.Streaming || msg.Content != "Final cleaned answer" || !strings.Contains(msg.ToolCalls, `"timeout"`) || msg.StreamSeq == 0 {
		t.Fatalf("done preceded complete persisted snapshot: %+v", msg)
	}
}

func TestChatSend_AllLifecycleTerminalsCloseStream(t *testing.T) {
	cases := []struct {
		name, errorText, wanted string
		kind                    outbound.EventType
	}{
		{"no reply", "", "event: done", outbound.EventMessageDone},
		{"dropped", "", `"status":"dropped"`, outbound.EventMessageDropped},
		{"failure", "provider failed", "event: error", outbound.EventMessageError},
		{"dispatch failure", "sender failed", "event: error", outbound.EventDispatchError},
		{"cancelled", context.Canceled.Error(), `"status":"cancelled"`, outbound.EventMessageError},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			env := newChatStreamTestEnv(t)
			go func() {
				trace := (<-env.queue.C()).Message.TraceID
				env.publish(trace, item.kind, map[string]any{"error": item.errorText})
			}()
			body := readChatStream(t, env.send(t, context.Background()))
			if !strings.Contains(body, item.wanted) {
				t.Fatalf("terminal not propagated: %s", body)
			}
		})
	}
}

func TestChatDisconnect_FinalSnapshotKeepsCanonicalTextAndTools(t *testing.T) {
	env := newChatStreamTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	response := env.send(t, ctx)
	msg := (<-env.queue.C()).Message
	value, ok := managedChatTurns.Load(messageCancelKey("bot", msg.TraceID))
	if !ok {
		t.Fatal("missing background persister")
	}
	turn := value.(*chatTurn)
	cancel()
	response.Body.Close()
	env.publish(msg.TraceID, outbound.EventLLMTextDelta, map[string]any{"text": "unclean"})
	env.publish(msg.TraceID, outbound.EventLLMToolCall, map[string]any{"toolCallId": "tool", "tool": "exec", "input": map[string]any{"command": "pwd"}})
	env.publish(msg.TraceID, outbound.EventLLMToolResult, map[string]any{"toolCallId": "tool", "output": map[string]any{"stdout": "/work", "exitCode": 0}})
	env.reply(msg.TraceID, "Clean final answer")
	select {
	case <-turn.done:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnected turn did not finish")
	}
	saved := loadAssistant(t, env.history, msg.TraceID)
	if saved.Content != "Clean final answer" || saved.Streaming || !strings.Contains(saved.ToolCalls, `"success"`) || !strings.Contains(saved.PartsJSON, "/work") {
		t.Fatalf("disconnected final state lost content/tools: %+v", saved)
	}
	resumed, err := http.Get(env.http.URL + "/api/chat/resume?botId=bot&traceId=" + msg.TraceID + "&sessionId=" + env.session)
	if err != nil {
		t.Fatal(err)
	}
	body := readChatStream(t, resumed)
	if !strings.Contains(body, "event: snapshot") || !strings.Contains(body, "event: done") || strings.Contains(body, "event: text_delta") {
		t.Fatalf("completed resume replayed duplicate deltas instead of final snapshot: %s", body)
	}
}

func TestChatResume_SnapshotSkipsAlreadyPersistedDeltas(t *testing.T) {
	env := newChatStreamTestEnv(t)
	const trace = "resume-trace"
	env.publish(trace, outbound.EventLLMTextDelta, map[string]any{"text": "first"})
	if err := env.history.UpsertAssistantSnapshot("bot", "1", "first", trace, "", `[{"type":"text","content":"first"}]`, env.session, true, env.bus.LatestSeq()); err != nil {
		t.Fatal(err)
	}
	env.publish(trace, outbound.EventLLMTextDelta, map[string]any{"text": "second"})
	env.publish(trace, eventChatDone, map[string]any{"text": "firstsecond", "traceId": trace, "sessionId": env.session})
	response, err := http.Get(env.http.URL + "/api/chat/resume?botId=bot&traceId=" + trace + "&sessionId=" + env.session)
	if err != nil {
		t.Fatal(err)
	}
	body := readChatStream(t, response)
	if strings.Count(body, "event: text_delta") != 1 || !strings.Contains(body, `data: {"text":"second"}`) || !strings.Contains(body, "event: snapshot") {
		t.Fatalf("snapshot and replay overlapped: %s", body)
	}
}

func TestChatResume_RejectsWrongBotOrSession(t *testing.T) {
	env := newChatStreamTestEnv(t)
	if err := env.history.UpsertAssistantByTrace("bot", "1", "private to bot", "trace", "", "", env.session, false); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"botId=other&traceId=trace", "botId=bot&traceId=trace&sessionId=other"} {
		response, err := http.Get(env.http.URL + "/api/chat/resume?" + query)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode < 400 || strings.Contains(string(body), "private to bot") {
			t.Fatalf("resume accepted wrong scope: %d %s", response.StatusCode, body)
		}
	}
}

func TestChatSend_InjectFailureReleasesSubscribers(t *testing.T) {
	env := newChatStreamTestEnv(t)
	if err := env.channel.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := env.send(t, context.Background())
	response.Body.Close()
	if response.StatusCode < 400 {
		t.Fatal("unstarted channel unexpectedly accepted chat")
	}
	deadline := time.Now().Add(time.Second)
	for env.bus.ActiveSubscriptions() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("inject failure leaked %d subscribers", env.bus.ActiveSubscriptions())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestChatSend_EmptyCanonicalRemovesPreviewParts(t *testing.T) {
	env := newChatStreamTestEnv(t)
	trace := make(chan string, 1)
	go func() {
		msg := (<-env.queue.C()).Message
		trace <- msg.TraceID
		env.publish(msg.TraceID, outbound.EventLLMTextDelta, map[string]any{"text": "preview that must be removed"})
		env.reply(msg.TraceID, "")
	}()
	body := readChatStream(t, env.send(t, context.Background()))
	if !strings.Contains(body, `"text":""`) || !strings.Contains(body, "event: done") {
		t.Fatalf("empty canonical was not sent: %s", body)
	}
	snapshot := loadAssistant(t, env.history, <-trace)
	if snapshot.Content != "" || strings.Contains(snapshot.PartsJSON, "preview") {
		t.Fatalf("empty canonical retained streamed preview: %+v", snapshot)
	}
}

func TestChatResume_FailureRemainsFailureAfterCompletion(t *testing.T) {
	env := newChatStreamTestEnv(t)
	trace := make(chan string, 1)
	go func() {
		msg := (<-env.queue.C()).Message
		trace <- msg.TraceID
		env.publish(msg.TraceID, outbound.EventLLMTextDelta, map[string]any{"text": "partial result"})
		env.publish(msg.TraceID, outbound.EventMessageError, map[string]any{"error": "provider unavailable"})
	}()
	readChatStream(t, env.send(t, context.Background()))
	id := <-trace
	response, err := http.Get(env.http.URL + "/api/chat/resume?botId=bot&traceId=" + id)
	if err != nil {
		t.Fatal(err)
	}
	body := readChatStream(t, response)
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "provider unavailable") || !strings.Contains(body, "partial result") || strings.Contains(body, "event: done") {
		t.Fatalf("restored failed turn was reported as successful: %s", body)
	}
}

func TestChatTurn_FlushesTrailingDeltaWhileQuiet(t *testing.T) {
	env := newChatStreamTestEnv(t)
	const trace = "quiet-turn"
	turn := env.svc.prepareChatTurn(env.channel, "bot", "1", env.session, trace)
	if turn == nil {
		t.Fatal("missing persister")
	}
	awaitText := func(want string, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			snapshot, err := env.history.LoadAssistantByTrace("bot", trace)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot != nil && snapshot.Content == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("quiet snapshot never reached %q", want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	env.publish(trace, outbound.EventLLMTextDelta, map[string]any{"text": "first"})
	awaitText("first", time.Second)
	env.publish(trace, outbound.EventLLMTextDelta, map[string]any{"text": " tail"})
	awaitText("first tail", streamPersistInterval+time.Second)
	env.reply(trace, "first tail")
	<-turn.done
}

func TestChatResume_AbandonedTurnsDoNotWaitForever(t *testing.T) {
	for _, hasSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprint(hasSnapshot), func(t *testing.T) {
			env := newChatStreamTestEnv(t)
			const trace = "abandoned"
			if err := env.history.SaveMessage("bot", "1", dao.ChatRoleUser, "hello", trace, env.session); err != nil {
				t.Fatal(err)
			}
			if hasSnapshot {
				if err := env.history.UpsertAssistantSnapshot("bot", "1", "saved preview", trace,
					`[{"id":"tool","name":"exec","status":"running"}]`,
					`[{"type":"tool","id":"tool","name":"exec","status":"running"}]`, env.session, true, 3); err != nil {
					t.Fatal(err)
				}
			}
			response, err := (&http.Client{Timeout: time.Second}).Get(env.http.URL + "/api/chat/resume?botId=bot&traceId=" + trace)
			if err != nil {
				t.Fatal(err)
			}
			if !hasSnapshot {
				response.Body.Close()
				if response.StatusCode != http.StatusNotFound {
					t.Fatalf("expected missing saved reply, got %d", response.StatusCode)
				}
				return
			}
			body := readChatStream(t, response)
			if !strings.Contains(body, `"status":"cancelled"`) || !strings.Contains(body, "saved preview") || strings.Contains(body, `"status":"running"`) {
				t.Fatalf("abandoned snapshot not finalized: %s", body)
			}
			saved := loadAssistant(t, env.history, trace)
			if saved.Streaming || saved.StreamStatus != "cancelled" || saved.StreamError == "" {
				t.Fatalf("abandoned snapshot remains active: %+v", saved)
			}
		})
	}
}
