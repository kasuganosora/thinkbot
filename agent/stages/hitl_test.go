package stages

import (
	"context"
	"encoding/json"
	"testing"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

func newTestHITLStore(t *testing.T) DeferredApprovalStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewDeferredApprovalStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestDeferredApprovalStore_PersistLoadResolve(t *testing.T) {
	store := newTestHITLStore(t)
	rec := &DeferredApproval{
		ApprovalID: "apv-1",
		BotID:      "bot-1",
		ToolName:   "shell_exec",
		ToolCallID: "call-1",
		MessageJSON: `{"id":"m1","botId":"bot-1","text":"run it","metadata":{"reply_target":"ch-1"}}`,
		Decision:    "deferred",
		Status:      "pending",
	}
	if err := store.Persist(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(context.Background(), "apv-1")
	if err != nil || got == nil {
		t.Fatalf("load failed: err=%v got=%v", err, got)
	}
	if got.Status != "pending" || got.ToolName != "shell_exec" {
		t.Fatalf("unexpected record: %+v", got)
	}
	if err := store.MarkResolved(context.Background(), "apv-1", "approved", "ok"); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Load(context.Background(), "apv-1")
	if got.Status != "resolved" || got.ResolvedDecision != "approved" {
		t.Fatalf("resolve not persisted: %+v", got)
	}
	// 不存在的 ID 返回 (nil, nil)
	none, err := store.Load(context.Background(), "nope")
	if err != nil || none != nil {
		t.Fatalf("expected (nil,nil) for missing id, got (%v,%v)", none, err)
	}
}

func TestBuildDeferredApproval(t *testing.T) {
	da := &llm.ToolApprovalResult{
		Decision:   llm.ToolApprovalDeferred,
		ApprovalID: "apv-2",
		ToolName:   "fs_write",
		ToolCallID: "call-2",
		Input:      map[string]any{"path": "/tmp/x", "content": "hi"},
	}
	msg := core.Message{ID: "m2", BotID: "bot-1", Text: "write file", Metadata: map[string]any{"reply_target": "ch-2"}}
	rec, err := BuildDeferredApproval(da, msg)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ApprovalID != "apv-2" || rec.ToolName != "fs_write" || rec.MessageID != "m2" {
		t.Fatalf("unexpected rec: %+v", rec)
	}
	// MessageJSON 应可反序列化回原始消息
	var back core.Message
	if err := json.Unmarshal([]byte(rec.MessageJSON), &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != "m2" || back.Metadata["reply_target"] != "ch-2" {
		t.Fatalf("message round-trip mismatch: %+v", back)
	}
}

// TestResumeDeferredApproval 验证续跑入口：加载记录→标记 resolved→按工具名注入预批准→
// 通过 ResumeDispatch 重跑，且重跑 ctx 中携带的预批准可被编排层识别。
func TestResumeDeferredApproval(t *testing.T) {
	store := newTestHITLStore(t)
	rec := &DeferredApproval{
		ApprovalID:  "apv-3",
		BotID:       "bot-1",
		ToolName:    "dangerous_tool",
		ToolCallID:  "call-3",
		MessageJSON: `{"id":"m3","botId":"bot-1","text":"do it"}`,
		Decision:    "deferred",
		Status:      "pending",
	}
	if err := store.Persist(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	var capturedCtx context.Context
	var dispatchedMsg core.Message
	stage := NewLLMStage("llm", nil, LLMConfig{
		DeferredApprovalStore: store,
		ResumeDispatch: func(ctx context.Context, msg core.Message) (*core.Envelope, error) {
			capturedCtx = ctx
			dispatchedMsg = msg
			return &core.Envelope{Message: msg}, nil
		},
	}, nil, zap.NewNop().Sugar())

	if err := stage.ResumeDeferredApproval(context.Background(), "apv-3", "approved", "go ahead"); err != nil {
		t.Fatal(err)
	}

	// 1) resolved 已落库
	got, _ := store.Load(context.Background(), "apv-3")
	if got.Status != "resolved" || got.ResolvedDecision != "approved" {
		t.Fatalf("resume did not resolve record: %+v", got)
	}
	// 2) 重跑消息即原始消息
	if dispatchedMsg.ID != "m3" {
		t.Fatalf("dispatched wrong message: %+v", dispatchedMsg)
	}
	// 3) 预批准按工具名注入 ctx，且编排层可识别
	pre := llm.PreApprovalFromContext(capturedCtx)
	if pre == nil {
		t.Fatal("pre-approval not injected into resume ctx")
	}
	pa, ok := pre["dangerous_tool"]
	if !ok || pa.Decision != llm.ToolApprovalApproved {
		t.Fatalf("pre-approval for dangerous_tool missing/wrong: %+v", pa)
	}

	// 重复续跑应报错（已 resolved）
	if err := stage.ResumeDeferredApproval(context.Background(), "apv-3", "approved", ""); err == nil {
		t.Fatal("expected error on double resume, got nil")
	}
	// 不存在的 ID 应报错
	if err := stage.ResumeDeferredApproval(context.Background(), "ghost", "approved", ""); err == nil {
		t.Fatal("expected error on unknown approval id, got nil")
	}
}
