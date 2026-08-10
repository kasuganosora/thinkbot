package toolperm

import (
	"context"
	"testing"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// TestIsBroadcastTool_Classification 覆盖「对外发言」级别的判定边界。
func TestIsBroadcastTool_Classification(t *testing.T) {
	broadcast := []string{
		"misskey_create_note", "misskey_create_renote", "misskey_delete_note",
		"misskey_react_to_note", "misskey_unreact_to_note",
		"misskey_follow_user", "misskey_unfollow_user",
		"telegram_pin_message", "telegram_delete_message",
		"telegram_ban_member", "telegram_unban_member",
		// 前缀兜底：未登记的新 Channel 写工具也应判为对外发言
		"misskey_quote_note", "telegram_promote_member",
	}
	for _, name := range broadcast {
		if !IsBroadcastTool(name) {
			t.Errorf("expected %q to be broadcast", name)
		}
		if ToolRisk(name) != RiskBroadcast {
			t.Errorf("expectedToolRisk(%q)=broadcast, got %s", name, ToolRisk(name))
		}
		// 对外发言工具绝不能同时被判为基础工具
		if IsBasicTool(name) {
			t.Errorf("%q must not be classified as basic", name)
		}
	}

	// Channel 只读查询不算对外发言，且应归入基础工具
	readOnly := []string{
		"misskey_search_user", "misskey_list_following",
		"telegram_get_chat_info", "telegram_get_chat_member_count",
		"telegram_get_chat_administrators",
	}
	for _, name := range readOnly {
		if IsBroadcastTool(name) {
			t.Errorf("read-only %q must not be broadcast", name)
		}
		if !IsBasicTool(name) {
			t.Errorf("read-only %q should be basic", name)
		}
	}

	// 非 Channel 工具不受影响
	for _, name := range []string{"sandbox_exec", "web_search", "calculate", ""} {
		if IsBroadcastTool(name) {
			t.Errorf("%q should not be broadcast", name)
		}
	}
}

// TestEvaluate_BroadcastDeniedInWhitelistMode 验证只读 bot 的核心场景：
// misskey 上配一条「禁止发帖」规则后，发帖类工具被拦，只读查询与基础工具照常可用。
func TestEvaluate_BroadcastDeniedInWhitelistMode(t *testing.T) {
	svc := newTestService(t)
	//潜水配置：禁掉 misskey 上所有对外写操作
	if _, err := svc.CreateRule("bot-lurk", RuleReq{
		Tool: "misskey_create_*", Platform: "misskey", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}

	// 发帖/转发被禁（命中 deny）
	for _, tool := range []string{"misskey_create_note", "misskey_create_renote"} {
		if svc.Evaluate("bot-lurk", tool, "misskey", "u1") {
			t.Errorf("%q should be denied", tool)
		}
	}
	// 其它对外动作：平台已有规则 + 非基础工具 → 默认禁止
	for _, tool := range []string{"misskey_react_to_note", "misskey_follow_user"} {
		if svc.Evaluate("bot-lurk", tool, "misskey", "u1") {
			t.Errorf("%q should be denied by whitelist default", tool)
		}
	}
	// 只读查询与基础工具仍可用 —— 潜水 bot 依然能看能想
	for _, tool := range []string{"misskey_search_user", "misskey_list_following", "memory", "calculate"} {
		if !svc.Evaluate("bot-lurk", tool, "misskey", "u1") {
			t.Errorf("%q should stay allowed for a read-only bot", tool)
		}
	}
}

// TestEvaluator_SystemSessionDoesNotBypassBroadcast 是本次的关键安全约束：
// 系统会话（cron / 心跳 / 梦境巩固）豁免普通工具，但**不豁免对外发言工具**——
// 否则定时任务能在无人监督的独立 session 里偷偷发帖，绕过整个权限配置。
func TestEvaluator_SystemSessionDoesNotBypassBroadcast(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateRule("bot-sys", RuleReq{
		Tool: "*", Platform: "misskey", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}
	ev := svc.NewEvaluator()

	toolList := []llm.Tool{
		{Name: "misskey_create_note", Execute: func(*llm.ToolExecContext, any) (any, error) { return "posted", nil }},
		{Name: "sandbox_exec", Execute: func(*llm.ToolExecContext, any) (any, error) { return "ran", nil }},
		{Name: "calculate", Execute: func(*llm.ToolExecContext, any) (any, error) { return 42, nil }},
	}
	sysCtx := &agenttools.ToolSessionContext{
		BotID: "bot-sys", SourceChannelType: "misskey", UserID: "cron", IsSystem: true,
	}
	out, err := ev.FilterTools(context.Background(), toolList, sysCtx)
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, tl := range out {
		names[tl.Name] = true
	}
	if names["misskey_create_note"] {
		t.Error("system session must NOT be allowed to post (broadcast tool bypassed)")
	}
	// 非发言工具仍被豁免（cron 需要它们干活）
	if !names["sandbox_exec"] || !names["calculate"] {
		t.Errorf("system session should still get non-broadcast tools, got %v", names)
	}
}

// TestEvaluator_SystemSessionCanPostWithExplicitAllow 验证豁免收紧后仍留有出口：
// 想让定时任务发帖，配一条显式 allow 规则即可。
func TestEvaluator_SystemSessionCanPostWithExplicitAllow(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateRule("bot-sys2", RuleReq{
		Tool: "misskey_create_note", Platform: "misskey", UserIDs: []string{"*"},
		Decision: DecisionAllow, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}
	ev := svc.NewEvaluator()
	toolList := []llm.Tool{
		{Name: "misskey_create_note", Execute: func(*llm.ToolExecContext, any) (any, error) { return "posted", nil }},
	}
	out, err := ev.FilterTools(context.Background(), toolList, &agenttools.ToolSessionContext{
		BotID: "bot-sys2", SourceChannelType: "misskey", UserID: "cron", IsSystem: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("explicit allow should let system session post, got %d tools", len(out))
	}
}

// TestEvaluator_UnknownPlatformBlocksBroadcast 覆盖子智能体绕过通道。
//
// 背景（实测确认的真实漏洞）：`botservice.go` 里
// `saMgr.SetToolResolver(toolMgr, ToolSessionContext{BotID: id})` 只传了 BotID，
// **不带 SourceChannelType**。子智能体解析工具时 platform=""，会落进
// Evaluate 的「该平台无任何规则 → 开放基线」分支 —— 主 Agent 在 misskey 上
// 被禁发帖，派生的子智能体却能拿到 misskey_create_note 照发。
// 修复：平台未知时对外发言工具一律拒绝（非发言工具不受影响，避免误伤工作空间操作）。
func TestEvaluator_UnknownPlatformBlocksBroadcast(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateRule("bot-sub", RuleReq{
		Tool: "*", Platform: "misskey", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}
	ev := svc.NewEvaluator()
	toolList := []llm.Tool{
		{Name: "misskey_create_note", Execute: func(*llm.ToolExecContext, any) (any, error) { return "posted", nil }},
		{Name: "sandbox_exec", Execute: func(*llm.ToolExecContext, any) (any, error) { return "ran", nil }},
	}

	// 子智能体上下文：只有 BotID，platform 为空
	out, err := ev.FilterTools(context.Background(), toolList, &agenttools.ToolSessionContext{
		BotID: "bot-sub", IsSubagent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var hasExec bool
	for _, tl := range out {
		if tl.Name == "misskey_create_note" {
			t.Error("subagent with unknown platform must not receive broadcast tools")
		}
		if tl.Name == "sandbox_exec" {
			hasExec = true
		}
	}
	// 工作空间工具仍可用：子智能体的正常用途（读写文件、跑命令）不该被误伤
	if !hasExec {
		t.Error("non-broadcast tools should still resolve for subagents")
	}
}

// TestIsBasicTool_Classification 覆盖风险分级的判定边界。
func TestIsBasicTool_Classification(t *testing.T) {
	basic := []string{
		"calculate", "datetime_calc", "now", "random", "uuid",
		"text_diff", "text_encode", "text_hash", "text_stats",
		"memory", "memory_snapshot", "memory_tools",
		"task_detail", "sandbox_health",
	}
	for _, name := range basic {
		if !IsBasicTool(name) {
			t.Errorf("expected %q to be basic", name)
		}
		if ToolRisk(name) != RiskBasic {
			t.Errorf("expected ToolRisk(%q)=basic", name)
		}
	}

	sensitive := []string{
		// 沙箱：执行命令 / 读写文件
		"sandbox_exec", "sandbox_read_file", "sandbox_write_file",
		"sandbox_delete_file", "sandbox_move_file", "sandbox_list_dir",
		"sandbox_search_content", "sandbox_replace_in_file",
		// 联网
		"web_search", "web_fetch", "http_get",
		// 派生执行体
		"spawn", "task", "task_control",
		// 未知 / 外部工具（安全默认）
		"mcp_do_something", "some_unknown_tool", "",
	}
	for _, name := range sensitive {
		if IsBasicTool(name) {
			t.Errorf("expected %q to be sensitive", name)
		}
	}
}

// TestIsBasicTool_PrefixGuardWins 验证敏感前缀是兜底护栏：
// 即便某工具被误列进 basicTools，只要命中敏感前缀（且非显式例外）仍判为敏感。
func TestIsBasicTool_PrefixGuardWins(t *testing.T) {
	basicTools["sandbox_wipe_disk"] = struct{}{} // 模拟误加
	defer delete(basicTools, "sandbox_wipe_disk")

	if IsBasicTool("sandbox_wipe_disk") {
		t.Fatal("sensitive prefix guard should override basicTools entry")
	}
	// 显式例外仍然生效
	if !IsBasicTool("sandbox_health") {
		t.Fatal("sandbox_health should remain basic via exception list")
	}
}

// TestEvaluate_BasicToolsSurviveWhitelistMode 是本次修复的核心回归测试。
//
// 场景：管理员只想在 telegram 上禁掉 sandbox_exec，于是配了一条 deny 规则。
// 修复前该平台会整体进入白名单模式，calculate / memory / now 等无害工具
// 被连带禁止，Bot 失去基本能力。修复后基础工具不受牵连。
func TestEvaluate_BasicToolsSurviveWhitelistMode(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateRule("bot-x", RuleReq{
		Tool: "sandbox_exec", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}

	// 敏感工具：命中 deny 或落入白名单默认禁止
	for _, tool := range []string{"sandbox_exec", "web_search", "web_fetch", "spawn", "task"} {
		if svc.Evaluate("bot-x", tool, "telegram", "u1") {
			t.Errorf("sensitive tool %q should be denied", tool)
		}
	}
	// 基础工具：不受白名单模式牵连
	for _, tool := range []string{"calculate", "now", "memory", "task_detail", "text_stats", "uuid"} {
		if !svc.Evaluate("bot-x", tool, "telegram", "u1") {
			t.Errorf("basic tool %q should stay allowed", tool)
		}
	}
}

// TestEvaluate_ExplicitDenyBeatsBasicDefault 验证管理员显式规则优先级高于风险分级：
// 想连基础工具一起锁死，配一条 tool=* 的 deny 即可，分级不应成为绕过手段。
func TestEvaluate_ExplicitDenyBeatsBasicDefault(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateRule("bot-x", RuleReq{
		Tool: "*", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"calculate", "memory", "now", "sandbox_exec"} {
		if svc.Evaluate("bot-x", tool, "telegram", "u1") {
			t.Errorf("explicit deny(*) should block %q including basic tools", tool)
		}
	}

	// 精确 deny 一个基础工具也应生效
	svc2 := newTestService(t)
	if _, err := svc2.CreateRule("bot-y", RuleReq{
		Tool: "memory", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}
	if svc2.Evaluate("bot-y", "memory", "telegram", "u1") {
		t.Error("explicit deny on a basic tool must be honored")
	}
	// 同平台其它基础工具仍开放
	if !svc2.Evaluate("bot-y", "calculate", "telegram", "u1") {
		t.Error("other basic tools should remain allowed")
	}
}

// TestEvaluate_NoRulePlatformStillAllowsSensitive 确认「平台完全无规则 → 全开放」
// 的既有语义未被风险分级破坏（未被约束的渠道不锁死）。
func TestEvaluate_NoRulePlatformStillAllowsSensitive(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.CreateRule("bot-x", RuleReq{
		Tool: "sandbox_exec", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}
	// misskey 无任何规则 → 连敏感工具也放行
	if !svc.Evaluate("bot-x", "sandbox_exec", "misskey", "u1") {
		t.Error("platform without any rule should allow even sensitive tools")
	}
}
