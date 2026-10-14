package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/dao"
)

func TestFinalizedAssistantStatusSurvivesReloadAndHistoryHTTP(t *testing.T) {
	history := newReliabilityHistory(t)
	for _, status := range []string{"error", "cancelled", "dropped", "completed"} {
		message := ""
		if status != "completed" {
			message = "terminal detail: " + status
		}
		if err := history.UpsertAssistantSnapshot("bot", "1", "partial", status, "", "", "session", true, 1); err != nil {
			t.Fatal(err)
		}
		if err := history.FinalizeAssistantSnapshot("bot", "1", "final "+status, status, "", "", "session", 2, status, message); err != nil {
			t.Fatal(err)
		}
		// Old persistence jobs and legacy fallbacks must not reopen the turn,
		// replace the final content, or erase its durable failure description.
		if err := history.UpsertAssistantSnapshot("bot", "1", "late partial", status, "", "", "session", true, 3); err != nil {
			t.Fatal(err)
		}
		if err := history.FinalizeAssistantSnapshot("bot", "1", "stale final", status, "", "", "session", 1, "completed", ""); err != nil {
			t.Fatal(err)
		}
		if err := history.UpsertAssistantByTrace("bot", "1", "fallback", status, "", "", "session", false); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh service has no in-memory turn state or replay cache.
	reloaded := NewChatHistoryService(history.db, zap.NewNop().Sugar())
	for _, status := range []string{"error", "cancelled", "dropped", "completed"} {
		msg, err := reloaded.LoadAssistantByTrace("bot", status)
		if err != nil {
			t.Fatal(err)
		}
		if msg == nil || msg.Streaming || msg.StreamStatus != status || msg.StreamSeq != 2 || msg.Content != "final "+status {
			t.Fatalf("%s lost final outcome after reload: %+v", status, msg)
		}
		if status != "completed" && msg.StreamError != "terminal detail: "+status {
			t.Fatalf("%s lost error: %+v", status, msg)
		}
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/chat/history?botId=bot&sessionId=session", nil)
	c.Set(ContextKeyUser, &dao.User{ID: 1})
	(&Server{chatHistory: reloaded}).handleChatHistory(c)
	if w.Code != http.StatusOK {
		t.Fatalf("history returned %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			Messages []struct {
				TraceID      string `json:"traceId"`
				StreamStatus string `json:"streamStatus"`
				StreamError  string `json:"streamError"`
				Streaming    bool   `json:"streaming"`
				SessionID    string `json:"sessionId"`
			} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data.Messages) != 4 {
		t.Fatalf("history messages=%d", len(response.Data.Messages))
	}
	for _, msg := range response.Data.Messages {
		if msg.StreamStatus != msg.TraceID || msg.Streaming || msg.SessionID != "session" {
			t.Fatalf("HTTP lost terminal fields: %+v", msg)
		}
		if msg.TraceID != "completed" && msg.StreamError != "terminal detail: "+msg.TraceID {
			t.Fatalf("HTTP lost error detail: %+v", msg)
		}
	}
}

func TestFinalizeAssistantSnapshotWritesOutcomeWithoutTrace(t *testing.T) {
	history := newTestChatHistory(t)
	if err := history.FinalizeAssistantSnapshot("bot", "1", "reply", "", "", "", "session", 4, "cancelled", "stopped"); err != nil {
		t.Fatal(err)
	}
	msg := loadAssistant(t, history, "")
	if msg.Streaming || msg.StreamStatus != "cancelled" || msg.StreamError != "stopped" || msg.StreamSeq != 4 {
		t.Fatalf("outcome missing from non-idempotent insert: %+v", msg)
	}
}
