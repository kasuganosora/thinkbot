package api

import (
	"context"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/stages"
	"github.com/kasuganosora/thinkbot/dao"
)

// TestWebChannel_Send_FallbackPersistsWhenNoSubscriber 验证：当没有实时 SSE 订阅者时
// （responses[traceID] 不存在，例如 workflow 续跑时前端已断开 / 未 resume），bot 的
// assistant 回复会被兜底落库到 chat_messages，用户刷新或 resume 即可看到，避免
// 「bot 跑完没汇报」的体感。
//
// 这正是 2026-08-18 线上事故的根因：workflow 完成后 agent 确实生成了总结回复，但
// Send 找不到对应 traceID 的订阅 channel，回复被静默丢弃，DB 中始终没有这条消息。
func TestWebChannel_Send_FallbackPersistsWhenNoSubscriber(t *testing.T) {
	ch := NewWebChannel("web-bot-x", "bot-x")
	ch.chatHistory = newTestChatHistory(t)

	// 模拟 workflow 续跑回复：traceID 同时充当 sessionID，且无人在线订阅。
	const traceID = "5"
	reply := "全部 46 个子包 README 审查完毕 ✅，无阻塞性问题。@@REPLY_CONTROL@@{\"send\": true}"
	action := core.Action{
		Type:     core.ActionReply,
		UserID:   "system",
		Payload:  reply,
		Metadata: map[string]any{"trace_id": traceID, "source_channel": "web:system"},
	}

	if err := ch.Send(context.Background(), action); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}

	// 兜底落库走异步 goroutine，轮询等待其完成（最多 2s）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if countAssistant(t, ch.chatHistory, traceID) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fallback did not persist assistant reply within timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}

	msg := loadAssistant(t, ch.chatHistory, traceID)
	if msg.SessionID != traceID {
		t.Errorf("session_id = %q, want %q", msg.SessionID, traceID)
	}
	if msg.UserID != "system" {
		t.Errorf("user_id = %q, want %q", msg.UserID, "system")
	}
	// reply-control 协议标记必须被剥离，与正常路径 saveAssistant 一致。
	if got := stages.StripReplyControlBlock(msg.Content); got != msg.Content {
		t.Errorf("reply-control block not stripped: %q", msg.Content)
	}
	if msg.Content == "" {
		t.Errorf("persisted content is empty")
	}
}

// TestWebChannel_Send_NoFallbackForNonReply 验证：非终态 action（如工具回调）即使无人
// 订阅也不会被兜底落库，避免把内部动作写进聊天历史。
func TestWebChannel_Send_NoFallbackForNonReply(t *testing.T) {
	ch := NewWebChannel("web-bot-y", "bot-y")
	ch.chatHistory = newTestChatHistory(t)

	action := core.Action{
		Type:     core.ActionCallback, // 非 reply 类型
		Payload:  "some internal callback",
		Metadata: map[string]any{"trace_id": "7"},
	}
	if err := ch.Send(context.Background(), action); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := countAssistant(t, ch.chatHistory, "7"); n != 0 {
		t.Errorf("non-reply action should not be persisted, got %d rows", n)
	}
}

// TestPaginateHistory_IncludesSystemMessages 验证：会话视图（按当前用户分页查询）会包含
// 系统/续跑产生的 'system' 归属消息，否则 workflow 续跑的指令与 bot 总结会被 user_id 过滤
// 掉，用户刷新后看不到 bot 续跑结果。
func TestPaginateHistory_IncludesSystemMessages(t *testing.T) {
	s := newTestChatHistory(t)
	const botID, sessionID, human = "bot-z", "sess-9", "1"

	_ = s.SaveMessage(botID, human, dao.ChatRoleUser, "hi", "t1", sessionID)
	_ = s.SaveMessage(botID, "system", dao.ChatRoleUser, "系统通知：工作流已完成，请继续", "t2", sessionID)
	_ = s.UpsertAssistantByTrace(botID, "system", "总结：全部完成", "t3", "", "", sessionID, false)

	page, err := s.PaginateHistory(botID, human, "", 20, sessionID)
	if err != nil {
		t.Fatalf("PaginateHistory: %v", err)
	}
	if len(page.Messages) != 3 {
		t.Fatalf("expected 3 messages (incl 2 system-owned), got %d", len(page.Messages))
	}
}
