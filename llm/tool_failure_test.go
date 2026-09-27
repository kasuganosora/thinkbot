package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 工具失败必须不含糊：OpenAI 兼容接口没有 is_error，结果文本本身要说清「没有执行」。
func TestRunTool_ErrorResultIsUnambiguous(t *testing.T) {
	tool := &Tool{
		Name: "misskey_create_note",
		Execute: func(*ToolExecContext, any) (any, error) {
			return nil, errors.New("create note failed: 500")
		},
	}
	tc := ToolCall{ToolCallID: "c1", ToolName: "misskey_create_note", Input: map[string]any{"text": "hi"}}
	res := runTool(context.Background(), tc, tool, nil, &OrchestrateConfig{})
	msg, _ := res.Result.(string)
	if !res.IsError || !strings.HasPrefix(msg, ToolErrorPrefix) || !strings.Contains(msg, "create note failed: 500") {
		t.Fatalf("error result must carry the failure prefix and details, got IsError=%v %q", res.IsError, msg)
	}
}

func TestRunTool_IntentRefusalSaysNothingHappened(t *testing.T) {
	tool := &Tool{Name: "misskey_create_note", RequiresUserIntent: true,
		Execute: func(*ToolExecContext, any) (any, error) { return "ok", nil }}
	tc := ToolCall{ToolCallID: "c1", ToolName: "misskey_create_note", Input: map[string]any{}}
	res := runTool(context.Background(), tc, tool, nil, &OrchestrateConfig{UserRequest: "今天天气怎么样"})
	msg, _ := res.Result.(string)
	if !res.IsError || !strings.HasPrefix(msg, "NOT EXECUTED") || !strings.Contains(msg, "不要告诉用户已经完成") {
		t.Fatalf("refusal must state that nothing was executed, got %q", msg)
	}
}

func TestIsDirectReplyFrom(t *testing.T) {
	ctx := WithDirectReply(context.Background(), true)
	if !IsDirectReplyFrom(ctx, "misskey") {
		t.Fatal("without inbound source info the old conservative behaviour applies")
	}
	tg := WithInboundReply(ctx, InboundReply{Source: "telegram", HasReplyTarget: true})
	if IsDirectReplyFrom(tg, "misskey") {
		t.Fatal("a Telegram mention is not a direct reply on Misskey")
	}
	if !IsDirectReplyFrom(tg, "telegram") {
		t.Fatal("same channel must count")
	}
	if IsDirectReplyFrom(WithDirectReply(context.Background(), false), "misskey") {
		t.Fatal("not a direct reply")
	}
}

func TestTruncateForJudgeKeepsUTF8(t *testing.T) {
	s := strings.Repeat("发", 400)
	got := truncateForJudge(s)
	if !strings.HasPrefix(got, strings.Repeat("发", 300)) || strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncation broke UTF-8: %q", got[:20])
	}
}
