package stages

import (
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
)

// TestChatSessionIDFromEnvelope 验证前端会话 ID 能从消息 metadata 取出。
//
// 这是「刷新页面后工作流卡片消失」修复链路的一环：
// web 侧把会话 ID 写进 metadata → LLMStage 注入 CallOrigin → task 工具落库 →
// 前端刷新后按会话查回工作流。链路任一环断掉，卡片就恢复不出来。
func TestChatSessionIDFromEnvelope(t *testing.T) {
	env := &core.Envelope{Message: core.Message{
		Metadata: map[string]any{agenttools.ExtraKeyChatSessionID: "sess-7"},
	}}
	if got := chatSessionIDFromEnvelope(env); got != "sess-7" {
		t.Errorf("want sess-7, got %q", got)
	}
}

// TestChatSessionIDFromEnvelope_Absent 验证非 web渠道（无该 metadata）安全返回空串，
// 且nil envelope / nil metadata 都不 panic。
func TestChatSessionIDFromEnvelope_Absent(t *testing.T) {
	if got := chatSessionIDFromEnvelope(nil); got != "" {
		t.Errorf("nil envelope 应返回空串，实际 %q", got)
	}
	if got := chatSessionIDFromEnvelope(&core.Envelope{}); got != "" {
		t.Errorf("nil metadata 应返回空串，实际 %q", got)
	}
	env := &core.Envelope{Message: core.Message{
		Metadata: map[string]any{"other": "x"},
	}}
	if got := chatSessionIDFromEnvelope(env); got != "" {
		t.Errorf("无该 key 应返回空串，实际 %q", got)
	}
	// 类型不符（非 string）也不能 panic
	bad := &core.Envelope{Message: core.Message{
		Metadata: map[string]any{agenttools.ExtraKeyChatSessionID: 123},
	}}
	if got := chatSessionIDFromEnvelope(bad); got != "" {
		t.Errorf("类型不符应返回空串，实际 %q", got)
	}
}
