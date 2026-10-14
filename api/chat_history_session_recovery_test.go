package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
)

func assertRecoveredAssistant(t *testing.T, msg dao.ChatMessage, sessionID, content, toolID string) {
	t.Helper()
	if msg.SessionID != sessionID || msg.Content != content || msg.Streaming {
		t.Fatalf("wrong session snapshot: %+v", msg)
	}
	var tools []map[string]any
	if err := json.Unmarshal([]byte(msg.ToolCalls), &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0]["id"] != toolID {
		t.Fatalf("tools crossed sessions: %s", msg.ToolCalls)
	}
	var parts []map[string]any
	if err := json.Unmarshal([]byte(msg.PartsJSON), &parts); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	toolCount := 0
	for _, part := range parts {
		if part["type"] == "text" {
			value, _ := part["content"].(string)
			text.WriteString(value)
		}
		if part["type"] == "tool" {
			toolCount++
			if part["id"] != toolID {
				t.Fatalf("ordered tools crossed sessions: %s", msg.PartsJSON)
			}
		}
	}
	if text.String() != content || toolCount != 1 {
		t.Fatalf("ordered parts show draft or lose tools: content=%q parts=%s", content, msg.PartsJSON)
	}
}

func TestLegacySharedTraceRemainsIsolatedAcrossSessions(t *testing.T) {
	history := newTestChatHistory(t)
	base := time.Now().Add(-time.Minute)
	for _, sessionID := range []string{"session-a", "session-b"} {
		tool := "tool-" + sessionID
		rows := []dao.ChatMessage{
			{BotID: "bot", UserID: "1", SessionID: sessionID, Role: dao.ChatRoleAssistant, TraceID: "accidentally-shared", Content: "草稿", Streaming: true, StreamSeq: 1,
				ToolCalls: `[{"id":"` + tool + `","status":"success"}]`, PartsJSON: `[{"type":"text","content":"草稿"},{"type":"tool","id":"` + tool + `"}]`, CreatedAt: base},
			{BotID: "bot", UserID: "1", SessionID: sessionID, Role: dao.ChatRoleAssistant, TraceID: "accidentally-shared", Content: "最终文本 " + sessionID, StreamSeq: 2, CreatedAt: base.Add(time.Second)},
		}
		if err := history.db.Create(&rows).Error; err != nil {
			t.Fatal(err)
		}
	}
	check := func(sessionID, content string) {
		t.Helper()
		page, err := history.PaginateHistory("bot", "1", "", 20, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		context, err := history.LoadContext("bot", "1", 20, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		continued, err := history.LoadContextBySession("bot", sessionID, 20)
		if err != nil {
			t.Fatal(err)
		}
		for source, messages := range map[string][]dao.ChatMessage{"history": page.Messages, "context": context, "continuation": continued} {
			if len(messages) != 1 {
				t.Fatalf("%s %s has %d rows; legacy trace collision hid its reply", source, sessionID, len(messages))
			}
			assertRecoveredAssistant(t, messages[0], sessionID, content, "tool-"+sessionID)
		}
	}
	check("session-a", "最终文本 session-a")
	check("session-b", "最终文本 session-b")
	// A legacy trace collision must not cause an explicitly scoped update to
	// overwrite the other session's more recently inserted final row.
	if err := history.UpsertAssistantSnapshot("bot", "1", "修改后的最终文本", "accidentally-shared", "", "", "session-a", false, 3); err != nil {
		t.Fatal(err)
	}
	check("session-a", "修改后的最终文本")
	check("session-b", "最终文本 session-b")
}

func TestLegacyFinalPartsUseCanonicalContentIncludingEmptyReply(t *testing.T) {
	for _, content := range []string{"最终文本", ""} {
		t.Run("content="+content, func(t *testing.T) {
			history := newTestChatHistory(t)
			rows := []dao.ChatMessage{
				{BotID: "bot", UserID: "1", SessionID: "session", Role: dao.ChatRoleAssistant, TraceID: "trace", Content: "草稿", Streaming: true, ToolCalls: `[{"id":"tool"}]`, PartsJSON: `[{"type":"text","content":"草稿"},{"type":"tool","id":"tool"}]`, CreatedAt: time.Now()},
				{BotID: "bot", UserID: "1", SessionID: "session", Role: dao.ChatRoleAssistant, TraceID: "trace", Content: content, CreatedAt: time.Now()},
			}
			if err := history.db.Create(&rows).Error; err != nil {
				t.Fatal(err)
			}
			msg, err := history.LoadAssistantByTrace("bot", "trace")
			if err != nil {
				t.Fatal(err)
			}
			assertRecoveredAssistant(t, *msg, "session", content, "tool")
			page, err := history.PaginateHistory("bot", "1", "", 20, "session")
			if err != nil || len(page.Messages) != 1 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			assertRecoveredAssistant(t, page.Messages[0], "session", content, "tool")
		})
	}
}
