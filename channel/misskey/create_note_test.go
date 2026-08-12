package misskey

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// TestCreateNoteTool_BlockedInDirectReplyContext 验证：当处于「直接回复语境」
// （对方 @ 了 Bot 或回复了 Bot）时，misskey_create_note 必须拒绝，强制走框架
// 自动串接回复，避免「工具发孤立帖 + 框架自动回复」重复发文。
func TestCreateNoteTool_BlockedInDirectReplyContext(t *testing.T) {
	c := &MisskeyChannel{} // 阻断分支不触碰 api / cfg，零值即可
	tool := c.createNoteTool()

	// 直接回复语境：ctx 携带 IsDirectReply=true
	ctx := llm.WithDirectReply(context.Background(), true)
	out, err := tool.Execute(&llm.ToolExecContext{Context: ctx}, map[string]any{
		"text": "收到！回复来啦～",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", out)
	}
	if m["blocked"] != true {
		t.Fatalf("expected blocked=true, got %v", m["blocked"])
	}
	if m["success"] != false {
		t.Fatalf("expected success=false, got %v", m["success"])
	}
}

// TestCreateNoteTool_AllowedInTimelineContext 验证：非直接回复语境（如 timeline
// 旁听时想主动开新帖）时，misskey_create_note 不阻断，正常调用 API。
// 这里不联真实 API，仅确认阻断分支未被误触发（走到 API 调用即视为通过）。
func TestCreateNoteTool_AllowedInTimelineContext(t *testing.T) {
	c := &MisskeyChannel{} // API 为 nil：若误走到调用会 panic，从而证明阻断未触发
	tool := c.createNoteTool()

	// 非直接回复语境：ctx 不携带 IsDirectReply（或 false）
	ctx := llm.WithDirectReply(context.Background(), false)
	// 触发 API 调用会 panic（nil api），说明没有被阻断——符合预期。
	// 用 recover 捕获，确认走到了 createNoteFull（即未被阻断）。
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected to reach API call (nil api panic), but it did not")
		}
	}()
	_, _ = tool.Execute(&llm.ToolExecContext{Context: ctx}, map[string]any{
		"text": "我主动发一条新帖",
	})
}
