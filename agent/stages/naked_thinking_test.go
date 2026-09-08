package stages

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// TestExtractPublicReply_NakedThinkingLeak 锁定路径 3 的裸思考泄漏兜底：
// 模型未打标签、把内部推理/规划独白作为纯文本输出时，必须 fail-closed 返回空，
// 绝不外发（实测 TG 私聊约 3% 回合命中）。正常纯文本与正常选项列表不得误伤。
func TestExtractPublicReply_NakedThinkingLeak(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"normal plain text", "你好，这是正常回复。", "你好，这是正常回复。"},
		{"naked planning with send true", "short, then send true. luna is in 1:1 chat... Options: (a) admit degeneration", ""},
		{"naked self-critique + options", "But I failed to do the review properly. Options: (a) admit degeneration to luna", ""},
		{"protocol marker leaked into body", "I should decide now @@REPLY_CONTROL@@ and think", ""},
		{"normal options list to user (no first-person inner verb)", "给你几个方案：\n(a) 方案A\n(b) 方案B", "给你几个方案：\n(a) 方案A\n(b) 方案B"},
		{"normal reply mentioning send verb", "我会把正确的结果发给你。", "我会把正确的结果发给你。"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractPublicReply(c.input); got != c.want {
				t.Fatalf("extractPublicReply(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// TestLooksLikeInternalThinking 直接验证泄漏信号判定（高置信、低误伤）。
func TestLooksLikeInternalThinking(t *testing.T) {
	leak := []string{
		"short, then send true. luna is in 1:1 chat",
		"the plan is send:false because we should stay quiet",
		"But I failed to review it. Options: (a) admit (b) hide",
		"@@REPLY_CONTROL@@ leaked",
	}
	for _, s := range leak {
		if !looksLikeInternalThinking(s) {
			t.Fatalf("expected leak=true for %q", s)
		}
	}
	safe := []string{
		"你好，这是正常回复。",
		"给你几个方案：(a) 方案A (b) 方案B",
		"我会把正确的结果发给你，send 动词不在 true/false 后。",
		"Here is my plan for the migration, step 1 is to freeze.",
	}
	for _, s := range safe {
		if looksLikeInternalThinking(s) {
			t.Fatalf("expected leak=false for %q", s)
		}
	}
}

// TestLLMStage_NakedThinkingLeakSuppressedInPrivateChat 端到端锁定：
// TG 私聊（必回复面）里模型漏掉标签、把含协议动词残片的裸思考当作正文，
// 必须 fail-closed 不出站（不发裸思考给用户）—— 宁可本回合静默，也不暴露内部推理。
// 这与「私聊必回」不冲突：裸思考本身不是有效回复。
func TestLLMStage_NakedThinkingLeakSuppressedInPrivateChat(t *testing.T) {
	stage := newRCStage("short, then send true. luna is in 1:1 chat, I should just answer briefly. Options: (a) admit degeneration (b) stay silent")
	env := core.NewEnvelope(core.Message{
		ID: "priv-naked-thinking", Text: "模型还退化吗", Source: "telegram",
		Channel: "telegram:76017910", UserID: "luna", ChatType: core.ChatPrivate,
	})
	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("naked internal thinking must NOT be posted in private chat, got %d actions: %+v", len(got), got)
	}
}

// TestLLMStage_NormalReplyStillPostedAfterFix 回归：修复不得误伤正常私聊回复。
func TestLLMStage_NormalReplyStillPostedAfterFix(t *testing.T) {
	stage := newRCStage("模型已经修好了，现在的回复质量正常。\n@@REPLY_CONTROL@@{\"send\": true}")
	env := core.NewEnvelope(core.Message{
		ID: "priv-normal", Text: "修好了吗", Source: "telegram",
		Channel: "telegram:76017910", UserID: "luna", ChatType: core.ChatPrivate,
	})
	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	replies := replyActions(out)
	if len(replies) != 1 {
		t.Fatalf("normal private reply must be posted, got %d actions", len(replies))
	}
}
