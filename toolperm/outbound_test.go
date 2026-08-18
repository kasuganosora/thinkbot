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

// TestSpeakMode_ThreeStates 验证三态发言模式的闭环：
//   - active：默认，主动 + 被动都允许；
//   - passive：仅被动回复（出站放行，但主动发帖工具被禁、心跳不主动发帖）；
//   - mute：潜水（出站拦截，被 @ 也不回）。
func TestSpeakMode_ThreeStates(t *testing.T) {
	svc := newTestService(t)
	const bot = "bot-speak"

	// 默认 active
	if svc.SpeakMode(bot, "misskey") != ModeActive {
		t.Fatal("default speak mode should be active")
	}
	if !svc.AllowOutbound(bot, "misskey", "u1") {
		t.Error("active: outbound replies should be allowed")
	}
	if !svc.AllowProactivePost(bot, "misskey") {
		t.Error("active: proactive posting should be allowed")
	}
	if !svc.Evaluate(bot, "misskey_create_note", "misskey", "u1") {
		t.Error("active: proactive post tool should be allowed")
	}

	// 切到 passive
	if err := svc.SetSpeakMode(bot, "misskey", ModePassive); err != nil {
		t.Fatal(err)
	}
	if svc.SpeakMode(bot, "misskey") != ModePassive {
		t.Error("should be passive after set")
	}
	// 被动回复仍放行（被 @ 后 ActionReply 出站）
	if !svc.AllowOutbound(bot, "misskey", "u1") {
		t.Error("passive: outbound replies must still be allowed")
	}
	// 主动发帖被禁：心跳
	if svc.AllowProactivePost(bot, "misskey") {
		t.Error("passive: heartbeat proactive posting must be blocked")
	}
	// 主动发帖被禁：工具
	if svc.Evaluate(bot, "misskey_create_note", "misskey", "u1") {
		t.Error("passive: proactive post tool must be denied")
	}
	// 其它平台不受影响（单渠道只影响该渠道）
	if svc.SpeakMode(bot, "telegram") != ModeActive {
		t.Error("passive on misskey must not affect telegram")
	}
	if !svc.AllowProactivePost(bot, "telegram") {
		t.Error("telegram should still be active")
	}

	// 切到 mute
	if err := svc.SetSpeakMode(bot, "misskey", ModeMute); err != nil {
		t.Fatal(err)
	}
	if svc.SpeakMode(bot, "misskey") != ModeMute {
		t.Error("should be mute after set")
	}
	if !svc.IsReadOnly(bot, "misskey") {
		t.Error("mute implies read-only")
	}
	if svc.AllowOutbound(bot, "misskey", "u1") {
		t.Error("mute: outbound replies must be blocked")
	}

	// 切回 active：所有 auto 规则应被清理
	if err := svc.SetSpeakMode(bot, "misskey", ModeActive); err != nil {
		t.Fatal(err)
	}
	if svc.SpeakMode(bot, "misskey") != ModeActive {
		t.Error("should be active after reset")
	}
	if !svc.AllowOutbound(bot, "misskey", "u1") {
		t.Error("active: outbound should be allowed again")
	}
	if !svc.AllowProactivePost(bot, "misskey") {
		t.Error("active: proactive posting restored")
	}
	if !svc.Evaluate(bot, "misskey_create_note", "misskey", "u1") {
		t.Error("active: proactive post tool restored")
	}
}

// TestSpeakMode_WildcardCoversAll 验证 platform=* 的 passive 覆盖所有渠道。
func TestSpeakMode_WildcardCoversAll(t *testing.T) {
	svc := newTestService(t)
	const bot = "bot-speak-wild"
	if err := svc.SetSpeakMode(bot, "*", ModePassive); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"misskey", "telegram", "web"} {
		if svc.SpeakMode(bot, p) != ModePassive {
			t.Errorf("wildcard passive should cover %s", p)
		}
		if svc.AllowProactivePost(bot, p) {
			t.Errorf("wildcard passive should block proactive post on %s", p)
		}
	}
}

// TestSpeakMode_AutoRulesIsolatedFromManual 验证切回 active 只清 auto 规则，
// 不误删用户在「规则列表」手动配置的工具 deny。
func TestSpeakMode_AutoRulesIsolatedFromManual(t *testing.T) {
	svc := newTestService(t)
	const bot = "bot-speak-iso"

	// 用户手动禁掉 misskey_create_note（非 auto）
	enabled := true
	if _, err := svc.CreateRule(bot, RuleReq{
		Tool: "misskey_create_note", Platform: "misskey", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: &enabled, Sort: intp(5),
	}); err != nil {
		t.Fatal(err)
	}

	// 切到 passive（会写 auto 规则）
	if err := svc.SetSpeakMode(bot, "misskey", ModePassive); err != nil {
		t.Fatal(err)
	}
	// 切回 active（只清 auto 规则）
	if err := svc.SetSpeakMode(bot, "misskey", ModeActive); err != nil {
		t.Fatal(err)
	}

	// 用户手动的 deny 应仍在
	rules, err := svc.ListRules(bot)
	if err != nil {
		t.Fatal(err)
	}
	manualStillThere := false
	for _, r := range rules {
		if r.Tool == "misskey_create_note" && r.Platform == "misskey" && !r.Auto {
			manualStillThere = true
		}
	}
	if !manualStillThere {
		t.Error("manual deny rule must survive speak-mode switching")
	}
	// 不应残留任何 auto 规则
	for _, r := range rules {
		if r.Auto {
			t.Errorf("auto rule %q should have been cleaned up", r.Tool)
		}
	}
}
