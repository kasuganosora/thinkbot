package toolperm

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
)

// TestAllowOutbound_DefaultAllows 验证默认放行：
// 出站回复是 Bot 的核心功能，绝不能因为管理员配了无关的工具规则而意外失声。
func TestAllowOutbound_DefaultAllows(t *testing.T) {
	svc := newTestService(t)

	// 完全无规则
	if !svc.AllowOutbound("bot-a", "misskey", "u1") {
		t.Error("no rules should allow outbound")
	}

	// 有工具规则但与出站无关：即便是「禁止全部工具」也不应连带禁掉回复，
	// 因为 tool=* 是给真实工具用的，出站用保留工具名单独表达。
	if _, err := svc.CreateRule("bot-a", RuleReq{
		Tool: "*", Platform: "misskey", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}
	if !svc.AllowOutbound("bot-a", "misskey", "u1") {
		t.Error("tool-level deny(*) must NOT silence outbound replies")
	}
}

// TestSetReadOnly_TogglesOutbound 验证只读开关的写入 / 撤销闭环。
func TestSetReadOnly_TogglesOutbound(t *testing.T) {
	svc := newTestService(t)

	// 开启潜水
	if err := svc.SetReadOnly("bot-b", "misskey", true); err != nil {
		t.Fatal(err)
	}
	if svc.AllowOutbound("bot-b", "misskey", "u1") {
		t.Error("read-only channel must block outbound")
	}
	if !svc.IsReadOnly("bot-b", "misskey") {
		t.Error("IsReadOnly should report true")
	}
	// 其它渠道不受影响
	if !svc.AllowOutbound("bot-b", "telegram", "u1") {
		t.Error("read-only on misskey must not affect telegram")
	}

	// 幂等：重复开启不应报错也不应产生歧义
	if err := svc.SetReadOnly("bot-b", "misskey", true); err != nil {
		t.Fatal(err)
	}
	if svc.AllowOutbound("bot-b", "misskey", "u1") {
		t.Error("still expected read-only after idempotent set")
	}

	// 关闭潜水 → 恢复发言
	if err := svc.SetReadOnly("bot-b", "misskey", false); err != nil {
		t.Fatal(err)
	}
	if !svc.AllowOutbound("bot-b", "misskey", "u1") {
		t.Error("outbound should be restored after disabling read-only")
	}
	if svc.IsReadOnly("bot-b", "misskey") {
		t.Error("IsReadOnly should report false")
	}

	// 关闭时本来无规则 → 不应报错
	if err := svc.SetReadOnly("bot-b", "misskey", false); err != nil {
		t.Fatalf("disabling when absent should be a no-op, got %v", err)
	}
}

// TestOutboundGuard_BlocksOnlyConfiguredChannel 验证守卫按渠道类型精确拦截，
// 且 name → type 的映射被正确使用（Action 只带Channel 名称）。
func TestOutboundGuard_BlocksOnlyConfiguredChannel(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SetReadOnly("bot-c", "misskey", true); err != nil {
		t.Fatal(err)
	}

	// 模拟真实映射：Channel 实例名 →渠道类型
	chanTypes := map[string]string{
		"my-misskey-bot": "misskey",
		"my-tg-bot":      "telegram",
	}
	guard := svc.NewOutboundGuard("bot-c", func(name string) string { return chanTypes[name] })

	act := core.Action{Type: core.ActionReply, UserID: "u1", Payload: "hello"}

	if guard.AllowOutbound(context.Background(), "my-misskey-bot", act) {
		t.Error("misskey channel should be blocked")
	}
	if !guard.AllowOutbound(context.Background(), "my-tg-bot", act) {
		t.Error("telegram channel should stay open")
	}
	// 未知 Channel 名：resolve 返回空 → 退化为用 channelName 当平台，
	// 无匹配规则 → 放行（不因映射缺失而误伤）
	if !guard.AllowOutbound(context.Background(), "unknown-channel", act) {
		t.Error("unmapped channel should not be blocked by default")
	}
}

// TestAllowOutbound_WildcardPlatform 验证 platform=* 的只读规则覆盖所有渠道。
func TestAllowOutbound_WildcardPlatform(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SetReadOnly("bot-d", "*", true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"misskey", "telegram", "web"} {
		if svc.AllowOutbound("bot-d", p, "u1") {
			t.Errorf("wildcard read-only should block %s", p)
		}
	}
}

// TestAllowOutbound_PerUser 验证可按用户粒度只读：
// 只对某个捣蛋用户闭麦，对其他人正常回复。
func TestAllowOutbound_PerUser(t *testing.T) {
	svc := newTestService(t)
	enabled := true
	sortVal := -100
	if _, err := svc.CreateRule("bot-e", RuleReq{
		Tool: OutboundReplyTool, Platform: "misskey", UserIDs: []string{"troll"},
		Decision: DecisionDeny, Enabled: &enabled, Sort: &sortVal,
	}); err != nil {
		t.Fatal(err)
	}
	if svc.AllowOutbound("bot-e", "misskey", "troll") {
		t.Error("should not reply to the muted user")
	}
	if !svc.AllowOutbound("bot-e", "misskey", "someone-else") {
		t.Error("other users should still get replies")
	}
}
