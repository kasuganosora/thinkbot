package misskey

import (
	"context"
	"strings"
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
	// 拦截必须以 error 返回（IsError），并明确「没有发出」，模型不能把它当成功汇报。
	if err == nil || !strings.Contains(err.Error(), "NOT POSTED") {
		t.Fatalf("expected a NOT POSTED error, got out=%v err=%v", out, err)
	}
}

func reachesAPI(t *testing.T, c *MisskeyChannel, ctx context.Context) (reached bool, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			reached = true // nil api panic = 已越过拦截、走到发帖调用
		}
	}()
	_, err = c.createNoteTool().Execute(&llm.ToolExecContext{Context: ctx}, map[string]any{"text": "测试帖"})
	return false, err
}

// 09-27 18:32：Telegram 私聊里用户让 bot 发 Misskey 帖，Mentioned=true（私聊）被当成
// 「正在回复别人的 Misskey 帖」拦截。其它渠道驱动的回合不应受 Misskey 回复护栏影响。
func TestCreateNoteTool_TelegramTurnIsNotAMisskeyReply(t *testing.T) {
	c := &MisskeyChannel{name: "misskey"}
	ctx := llm.WithDirectReply(context.Background(), true)
	ctx = llm.WithInboundReply(ctx, llm.InboundReply{Source: "telegram", HasReplyTarget: true})
	if reached, err := reachesAPI(t, c, ctx); !reached {
		t.Fatalf("a Telegram-driven turn must be allowed to post on Misskey, got err=%v", err)
	}
}

func TestCreateNoteTool_MisskeyDirectReplyStillBlocked(t *testing.T) {
	c := &MisskeyChannel{name: "misskey"}
	ctx := llm.WithDirectReply(context.Background(), true)
	ctx = llm.WithInboundReply(ctx, llm.InboundReply{Source: "misskey", HasReplyTarget: true})
	reached, err := reachesAPI(t, c, ctx)
	if reached || err == nil || !strings.Contains(err.Error(), "NOT POSTED") {
		t.Fatalf("a reply on this Misskey channel must stay blocked, reached=%v err=%v", reached, err)
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
