package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/inbound"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/workflow"
)

func newReliabilityHistory(t *testing.T) *ChatHistoryService {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "history.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&dao.ChatMessage{}, &dao.ChatSession{}, &dao.ContextCheckpoint{}); err != nil {
		t.Fatal(err)
	}
	return NewChatHistoryService(db, zap.NewNop().Sugar())
}

func TestChatHistoryHTTP_PreservesStreamingState(t *testing.T) {
	history := newReliabilityHistory(t)
	for _, streaming := range []bool{true, false} {
		trace := fmt.Sprintf("trace-%t", streaming)
		if err := history.UpsertAssistantByTrace("bot", "1", "reply", trace, "", "", "session", streaming); err != nil {
			t.Fatal(err)
		}
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/chat/history?botId=bot&sessionId=session", nil)
	c.Set(ContextKeyUser, &dao.User{ID: 1})
	(&Server{chatHistory: history}).handleChatHistory(c)
	if w.Code != http.StatusOK {
		t.Fatalf("history HTTP %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			Messages []struct {
				TraceID   string `json:"traceId"`
				Streaming *bool  `json:"streaming"`
			} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data.Messages) != 2 {
		t.Fatalf("received %d history messages, want 2", len(response.Data.Messages))
	}
	for _, msg := range response.Data.Messages {
		if msg.Streaming == nil {
			t.Errorf("%s: history HTTP omitted streaming; clients cannot restore running state", msg.TraceID)
		} else if *msg.Streaming != (msg.TraceID == "trace-true") {
			t.Errorf("%s: streaming=%v", msg.TraceID, *msg.Streaming)
		}
	}
}

func TestUpsertAssistantByTrace_ConcurrentFirstWriteIsUnique(t *testing.T) {
	history := newReliabilityHistory(t)
	// Widen the window between an unmatched UPDATE and INSERT without
	// requiring a particular lock strategy from the corrected implementation.
	if err := history.db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register("test:slow_empty_update", func(tx *gorm.DB) {
		if tx.Statement.Table == "chat_messages" && tx.RowsAffected == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 16)
	var writers sync.WaitGroup
	for i := 0; i < 16; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			errs <- history.UpsertAssistantByTrace("bot", "1", "partial", "same-trace", "", "", "session", true)
		}()
	}
	close(start)
	writers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := countAssistant(t, history, "same-trace"); n != 1 {
		t.Fatalf("concurrent first snapshots created %d assistant rows for one trace, want 1", n)
	}
}

func TestUpsertAssistantByTrace_LateSnapshotCannotReopenFinal(t *testing.T) {
	history := newReliabilityHistory(t)
	const trace = "snapshot-order"
	finalTools := `[{"id":"tool-1","status":"success"}]`
	if err := history.UpsertAssistantByTrace("bot", "1", "complete reply", trace, finalTools, finalTools, "session", false); err != nil {
		t.Fatal(err)
	}
	// A goroutine holding an older streaming snapshot can run after the
	// final write; a terminal row must never become streaming again.
	if err := history.UpsertAssistantByTrace("bot", "1", "partial", trace, `[{"id":"tool-1","status":"running"}]`, "", "session", true); err != nil {
		t.Fatal(err)
	}
	got := loadAssistant(t, history, trace)
	if got.Streaming || got.Content != "complete reply" || got.ToolCalls != finalTools {
		t.Fatalf("late snapshot regressed final reply: content=%q streaming=%t tools=%s", got.Content, got.Streaming, got.ToolCalls)
	}
}

func TestWebChannel_SendConcurrentWithUnregisterDoesNotPanic(t *testing.T) {
	channel := NewWebChannel("web-bot", "bot")
	const trace = "disconnecting"
	action := core.Action{Type: core.ActionReply, Payload: "done", Metadata: map[string]any{"trace_id": trace}}
	var panics atomic.Int64
	var workers sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			for j := 0; j < 5000; j++ {
				// Capture evidence in the sending goroutine so a regression is
				// a normal failing test, not an unrecoverable process crash.
				func() {
					defer func() {
						if recover() != nil {
							panics.Add(1)
						}
					}()
					_ = channel.Send(context.Background(), action)
				}()
			}
		}()
	}
	close(start)
	for i := 0; i < 5000; i++ {
		channel.RegisterResponse(trace, 16)
		runtime.Gosched()
		channel.UnregisterResponse(trace)
	}
	workers.Wait()
	if n := panics.Load(); n != 0 {
		t.Fatalf("disconnect racing with reply caused %d send-on-closed-channel panics", n)
	}
}

func TestWebWorkflowContinuation_TwoTurnsPreserveBothReplies(t *testing.T) {
	history := newReliabilityHistory(t)
	lg := zap.NewNop().Sugar()
	queue := inbound.NewIngress(inbound.DefaultIngressConfig(), lg, nooptrace.NewTracerProvider())
	t.Cleanup(queue.Close)
	channel := NewWebChannel("web-bot", "bot")
	channel.chatHistory = history
	if err := channel.Start(context.Background(), queue); err != nil {
		t.Fatal(err)
	}
	svc := &BotService{
		db: history.db, chatHistory: history, logger: lg, store: config.NewStore(nil),
		channels: map[string]*WebChannel{"bot": channel}, messageSessions: make(map[string]string),
	}
	traces := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		svc.onWorkflowCompleted(&workflow.Workflow{
			ID: fmt.Sprintf("workflow-%d", i), BotID: "bot", SessionID: "session",
			Status: workflow.WorkflowCompleted, Requirement: fmt.Sprintf("task-%d", i),
		})
		var msg core.Message
		select {
		case env := <-queue.C():
			msg = env.Message
		case <-time.After(time.Second):
			t.Fatal("continuation was not injected")
		}
		traces = append(traces, msg.TraceID)
		content := fmt.Sprintf("reply for workflow %d", i)
		if err := channel.Send(context.Background(), core.Action{
			Type: core.ActionReply, Payload: content,
			Metadata: map[string]any{"trace_id": msg.TraceID, "chat_session_id": "session"},
		}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			var count int64
			history.db.Model(&dao.ChatMessage{}).Where("role = ? AND content = ?", "assistant", content).Count(&count)
			if count > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("continuation reply was not persisted")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if traces[0] == traces[1] {
		t.Errorf("two different continuation turns reused trace %q", traces[0])
	}
	page, err := history.PaginateHistory("bot", "1", "", 20, "session")
	if err != nil {
		t.Fatal(err)
	}
	assistantCount := 0
	for _, msg := range page.Messages {
		if msg.Role == dao.ChatRoleAssistant {
			assistantCount++
		}
	}
	if assistantCount != 2 {
		t.Fatalf("two continuation turns retained %d replies in session history, want 2", assistantCount)
	}
}
