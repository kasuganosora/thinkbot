package toolperm

import (
	"context"
	"testing"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := dao.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewService(db, nil)
}

func boolp(b bool) *bool { return &b }
func intp(i int) *int    { return &i }

func TestEvaluate_NoRulePlatformAllows(t *testing.T) {
	svc := newTestService(t)
	// 仅给 telegram 加一条「禁用 sandbox_exec」规则（无 allow 基底）
	if _, err := svc.CreateRule("bot-x", RuleReq{
		Tool: "sandbox_exec", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	}); err != nil {
		t.Fatal(err)
	}
	// telegram 上 sandbox_exec → 命中 deny → 禁止
	if svc.Evaluate("bot-x", "sandbox_exec", "telegram", "u1") {
		t.Fatal("telegram sandbox_exec should be denied")
	}
	// telegram 上其它工具 → 平台有规则但无命中 → 安全默认禁止
	if svc.Evaluate("bot-x", "web_search", "telegram", "u1") {
		t.Fatal("telegram web_search should be denied (platform has rule, no match)")
	}
	// misskey 平台完全无规则 → 保守默认（修复 5142）：敏感工具禁止、基础工具放行
	if svc.Evaluate("bot-x", "web_search", "misskey", "u1") {
		t.Fatal("misskey has no rule → sensitive web_search must be denied by default")
	}
	if !svc.Evaluate("bot-x", "now", "misskey", "u1") {
		t.Fatal("misskey has no rule → basic tool `now` must still be allowed")
	}
	// web 平台会被惰性播种 allow 基线 → 放行
	if !svc.Evaluate("bot-x", "sandbox_exec", "web", "u1") {
		t.Fatal("web should be allowed by default-seeded rule")
	}
}

func TestSeedWebDefault_WebAllowedAndEmptyPlatformsAllowed(t *testing.T) {
	svc := newTestService(t)
	// 触发惰性播种
	rules, err := svc.ListRules("bot-web")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected exactly 1 seeded rule, got %d", len(rules))
	}
	r := rules[0]
	if r.Tool != "*" || r.Platform != "web" || r.Decision != DecisionAllow || !r.Enabled {
		t.Fatalf("unexpected web default rule: %+v", r)
	}
	// web 平台任意工具/用户 → 允许（基线命中）
	if !svc.Evaluate("bot-web", "sandbox_exec", "web", "anyone") {
		t.Fatal("web should allow all by default")
	}
	// telegram 平台无任何规则覆盖 → 保守默认（修复 5142）：敏感工具禁止、基础工具放行
	if svc.Evaluate("bot-web", "sandbox_exec", "telegram", "anyone") {
		t.Fatal("telegram with no rule → sensitive sandbox_exec must be denied by default")
	}
	if !svc.Evaluate("bot-web", "now", "telegram", "anyone") {
		t.Fatal("telegram with no rule → basic tool `now` must still be allowed")
	}
}

func TestEvaluate_FirstMatchWins_Override(t *testing.T) {
	svc := newTestService(t)
	// 先播种 web 默认（sort=0, allow all）
	if err := svc.SeedWebDefault("bot1"); err != nil {
		t.Fatal(err)
	}
	// 再插一条更具体的 deny：sandbox_exec / web / 全部用户（sort=1，但首条匹配=sort 最小者先赢）
	// 要让 deny 生效，必须排在 allow 之前 → sort 更小
	_, err := svc.CreateRule("bot1", RuleReq{
		Tool: "sandbox_exec", Platform: "web", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	})
	if err != nil {
		t.Fatal(err)
	}
	// sandbox_exec 在 web 上被 deny 规则（sort=-1）首条匹配 → 禁止
	if svc.Evaluate("bot1", "sandbox_exec", "web", "u1") {
		t.Fatal("sandbox_exec should be denied on web")
	}
	// 其他工具在 web 上仍由 allow 规则命中 → 允许
	if !svc.Evaluate("bot1", "web_search", "web", "u1") {
		t.Fatal("web_search should still be allowed on web")
	}
}

func TestEvaluate_UserScoped(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SeedWebDefault("bot2"); err != nil {
		t.Fatal(err)
	}
	// 仅对 u1 禁用 sandbox_exec（web）
	_, err := svc.CreateRule("bot2", RuleReq{
		Tool: "sandbox_exec", Platform: "web", UserIDs: []string{"u1"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Evaluate("bot2", "sandbox_exec", "web", "u1") {
		t.Fatal("u1 should be denied sandbox_exec")
	}
	if !svc.Evaluate("bot2", "sandbox_exec", "web", "u2") {
		t.Fatal("u2 should be allowed sandbox_exec (not in deny list)")
	}
}

func TestEvaluate_DisabledRuleIgnored(t *testing.T) {
	svc := newTestService(t)
	// 仅一条「禁用的 deny」规则（telegram）→ 不参与评估 → 平台无活动规则
	// → 开放基线放行（禁用规则既不授予也不拒绝）。
	_, err := svc.CreateRule("bot3", RuleReq{
		Tool: "*", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(false), Sort: intp(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 禁用的 deny 规则被忽略 → 平台无活动规则 → 保守默认（修复 5142）：敏感工具禁止
	if svc.Evaluate("bot3", "web_search", "telegram", "u1") {
		t.Fatal("disabled deny rule ignored; sensitive web_search must be denied by default")
	}
	// 基础工具仍放行
	if !svc.Evaluate("bot3", "now", "telegram", "u1") {
		t.Fatal("disabled deny rule ignored; basic tool `now` must still be allowed")
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"", "x", true},
		{"sandbox_*", "sandbox_exec", true},
		{"sandbox_*", "web_search", false},
		{"*_file", "read_file", true},
		{"read_file", "read_file", true},
		{"read_file", "read_file_x", false},
		{"a*b", "axxb", true},
		{"a*b", "ab", true},
		{"a*b", "ba", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q,%q)=%v want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestCRUD_UpdateDelete(t *testing.T) {
	svc := newTestService(t)
	r, err := svc.CreateRule("bot4", RuleReq{
		Tool: "sandbox_exec", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 更新为 allow + 禁用
	upd, err := svc.UpdateRule("bot4", r.ID, RuleReq{
		Decision: DecisionAllow, Enabled: boolp(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Decision != DecisionAllow || upd.Enabled {
		t.Fatalf("unexpected update: %+v", upd)
	}
	// 删除
	if err := svc.DeleteRule("bot4", r.ID); err != nil {
		t.Fatal(err)
	}
	// 再删应报 not found
	if err := svc.DeleteRule("bot4", r.ID); err == nil {
		t.Fatal("expected not-found error on second delete")
	}
	// 更新不存在也应报 not found
	if _, err := svc.UpdateRule("bot4", r.ID, RuleReq{}); err == nil {
		t.Fatal("expected not-found error on update missing rule")
	}
}

func TestResetDefaults(t *testing.T) {
	svc := newTestService(t)
	_, _ = svc.CreateRule("bot5", RuleReq{Tool: "x", Platform: "web", UserIDs: []string{"*"}})
	if err := svc.ResetDefaults("bot5"); err != nil {
		t.Fatal(err)
	}
	rules, err := svc.ListRules("bot5")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Platform != "web" || rules[0].Decision != DecisionAllow {
		t.Fatalf("reset should leave only web default, got %+v", rules)
	}
}

// TestEvaluator_FilterTools 验证 ToolManager 接入的评估器能按 platform 过滤工具，
// 并确认 web 渠道（SourceChannelType="web"）能命中默认全开规则。
//
// 注意：web 默认全开规则 platform=web，仅覆盖 web。
// 非 web 平台（如 telegram）默认没有任何规则覆盖 → 默认禁止，
// 因此要验证 telegram 上的「首条匹配生效」语义，必须显式为 telegram 建立一条 allow 基底规则，
// 再用更靠前（sort 更小）的 deny 规则把 sandbox_exec 掐掉。
func TestEvaluator_FilterTools(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SeedWebDefault("bot-ev"); err != nil {
		t.Fatal(err)
	}
	// telegram 基底：允许全部（sort=0）
	if _, err := svc.CreateRule("bot-ev", RuleReq{
		Tool: "*", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionAllow, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}
	// telegram 上禁止 sandbox_exec，且排得更靠前（sort=-1 → 首条匹配生效）
	if _, err := svc.CreateRule("bot-ev", RuleReq{
		Tool: "sandbox_exec", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	}); err != nil {
		t.Fatal(err)
	}

	ev := svc.NewEvaluator()
	tools := []llm.Tool{
		{Name: "sandbox_exec"},
		{Name: "web_search"},
		{Name: "now"},
	}

	// web 渠道 → 全部允许（命中 web 默认）
	webCtx := &agenttools.ToolSessionContext{BotID: "bot-ev", SourceChannelType: "web", UserID: "u1"}
	webOut, err := ev.FilterTools(context.Background(), tools, webCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(webOut) != len(tools) {
		t.Fatalf("web should allow all tools, got %d/%d", len(webOut), len(tools))
	}

	// telegram 渠道 → sandbox_exec 被禁止（首条匹配 deny），其余命中 allow 基底
	tgCtx := &agenttools.ToolSessionContext{BotID: "bot-ev", SourceChannelType: "telegram", UserID: "u1"}
	tgOut, err := ev.FilterTools(context.Background(), tools, tgCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tgOut) != 2 {
		t.Fatalf("telegram should drop only sandbox_exec, got %d tools: %+v", len(tgOut), tgOut)
	}
	for _, tl := range tgOut {
		if tl.Name == "sandbox_exec" {
			t.Fatal("sandbox_exec must be filtered out on telegram")
		}
	}

	// 平台「有规则但无命中」→ 按风险区分：敏感工具禁止、基础工具放行。
	// 用仅含 telegram deny(sandbox) 规则的新 bot 演示。
	svc2 := newTestService(t)
	if _, err := svc2.CreateRule("bot-ev2", RuleReq{
		Tool: "sandbox_exec", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	}); err != nil {
		t.Fatal(err)
	}
	ev2 := svc2.NewEvaluator()
	// telegram：sandbox_exec 命中 deny 被禁；web_search 为敏感工具且无命中 → 禁止；
	// now 是基础工具 → 即便平台进入白名单模式仍保留（见 risk.go）。
	tg2Ctx := &agenttools.ToolSessionContext{BotID: "bot-ev2", SourceChannelType: "telegram", UserID: "u1"}
	tg2Out, err := ev2.FilterTools(context.Background(), tools, tg2Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tg2Out) != 1 || tg2Out[0].Name != "now" {
		t.Fatalf("only the basic tool `now` should survive, got %d: %+v", len(tg2Out), tg2Out)
	}
}

// TestEvaluate_WebDefaultAllowRegardlessOfOtherPlatformRules 验证：
// 即便 bot 已配置其他平台（如 telegram）的规则、但 web 下没有任何规则，
// web 会话仍默认全部允许（web 是开放基线，与 bot 是否有其他平台规则无关）。
func TestEvaluate_WebDefaultAllowRegardlessOfOtherPlatformRules(t *testing.T) {
	svc := newTestService(t)
	// 只加一条 telegram 的 deny 规则，完全不涉及 web
	if _, err := svc.CreateRule("bot-mix", RuleReq{
		Tool: "sandbox_exec", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	}); err != nil {
		t.Fatal(err)
	}
	// web 上无任何规则 → 默认允许
	if !svc.Evaluate("bot-mix", "sandbox_exec", "web", "u1") {
		t.Fatal("web must allow when no web rule exists, even if bot has telegram rules")
	}
	if !svc.Evaluate("bot-mix", "web_search", "web", "u1") {
		t.Fatal("web must allow all tools when no web rule exists")
	}
	// telegram 上的 deny 规则仍生效
	if svc.Evaluate("bot-mix", "sandbox_exec", "telegram", "u1") {
		t.Fatal("telegram deny rule must still hold")
	}
	// 其他非 web 平台（无规则）→ 保守默认（修复 5142）：敏感工具禁止、基础工具放行
	if svc.Evaluate("bot-mix", "web_search", "misskey", "u1") {
		t.Fatal("misskey with no rule → sensitive web_search must be denied by default")
	}
	if !svc.Evaluate("bot-mix", "now", "misskey", "u1") {
		t.Fatal("misskey with no rule → basic tool `now` must still be allowed")
	}
}

// TestEvaluator_SystemSessionBypass 验证系统/内部会话（cron、心跳等）
// 不受 bot 工具权限约束：即便非 web 平台被 deny，系统会话仍放行全部工具。
func TestEvaluator_SystemSessionBypass(t *testing.T) {
	svc := newTestService(t)
	// 给 bot 加一条 telegram 全禁规则，模拟非 web 平台会被禁止
	if _, err := svc.CreateRule("bot-sys", RuleReq{
		Tool: "*", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	}); err != nil {
		t.Fatal(err)
	}
	ev := svc.NewEvaluator()
	tools := []llm.Tool{{Name: "sandbox_exec"}, {Name: "web_search"}}

	// 普通 telegram 会话：被 deny 规则禁止 → 0 工具
	tgCtx := &agenttools.ToolSessionContext{BotID: "bot-sys", SourceChannelType: "telegram", UserID: "u1"}
	tgOut, err := ev.FilterTools(context.Background(), tools, tgCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tgOut) != 0 {
		t.Fatalf("telegram should be denied, got %d", len(tgOut))
	}

	// 系统会话（IsSystem=true，无 channel）：直接放行全部工具，不受权限表约束
	sysCtx := &agenttools.ToolSessionContext{BotID: "bot-sys", IsSystem: true, UserID: "system"}
	sysOut, err := ev.FilterTools(context.Background(), tools, sysCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sysOut) != len(tools) {
		t.Fatalf("system session must bypass tool perm, got %d/%d", len(sysOut), len(tools))
	}
}

// TestEvaluator_CallTimeGuard 验证 FilterTools 对放行的工具包裹了「调用时二次复核」：
// 即便列表已通过过滤，执行前仍会按最新规则重新判定。若过滤后新增 deny 规则，
// 同一工具的执行应被拦截（证明二次防线真实生效，而非仅依赖列表过滤）。
func TestEvaluator_CallTimeGuard(t *testing.T) {
	svc := newTestService(t)
	if err := svc.SeedWebDefault("bot-g"); err != nil {
		t.Fatal(err)
	}
	ev := svc.NewEvaluator()
	tools := []llm.Tool{
		{Name: "web_search", Execute: func(_ *llm.ToolExecContext, _ any) (any, error) {
			return "ok", nil
		}},
	}
	webCtx := &agenttools.ToolSessionContext{BotID: "bot-g", SourceChannelType: "web", UserID: "u1"}
	out, err := ev.FilterTools(context.Background(), tools, webCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Execute == nil {
		t.Fatalf("expected 1 wrapped tool carrying Execute, got %d", len(out))
	}
	// 初始：web 允许 → 执行成功，且确实调用了底层 handler
	res, err := out[0].Execute(&llm.ToolExecContext{}, nil)
	if err != nil {
		t.Fatalf("initial call should succeed: %v", err)
	}
	if res != "ok" {
		t.Fatalf("unexpected exec result: %v", res)
	}
	// 过滤后新增 deny 规则：执行前的二次复核应拦截
	if _, err := svc.CreateRule("bot-g", RuleReq{
		Tool: "web_search", Platform: "web", UserIDs: []string{"*"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(-1),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := out[0].Execute(&llm.ToolExecContext{}, nil); err == nil {
		t.Fatal("call-time guard should block after deny rule added")
	}
}

// TestEvaluator_NilSessionContext 验证 sctx 为 nil 时不 panic 且保守放行。
// ResolveTools 的调用方可能传入 nil/零值上下文（如内部自省路径），
// 评估器必须容错而不是解引用空指针崩掉整个请求。
func TestEvaluator_NilSessionContext(t *testing.T) {
	svc := newTestService(t)
	ev := svc.NewEvaluator()
	tools := []llm.Tool{{Name: "web_search"}, {Name: "now"}}

	out, err := ev.FilterTools(context.Background(), tools, nil)
	if err != nil {
		t.Fatalf("nil sctx must not error: %v", err)
	}
	if len(out) != len(tools) {
		t.Fatalf("nil sctx should pass through all tools, got %d/%d", len(out), len(tools))
	}
}

// TestUpdateRule_PartialDoesNotResetFields 是提权事故的回归测试。
//
// 背景：前端「启用」开关/排序按钮只回传 {enabled} 或 {sort}。若 UpdateRule 对
// 缺失字段做全量归一化，一条 (sandbox_exec, telegram, deny) 规则会被静默重置成
// (*, *, ["*"], allow) —— 关掉一个开关反而放开了全部工具。必须保持部分更新语义。
func TestUpdateRule_PartialDoesNotResetFields(t *testing.T) {
	svc := newTestService(t)
	created, err := svc.CreateRule("bot-partial", RuleReq{
		Tool: "sandbox_exec", Platform: "telegram", UserIDs: []string{"u1"},
		Decision: DecisionDeny, Enabled: boolp(true), Sort: intp(5),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 仅切换 enabled（模拟前端开关），其余字段必须原样保留
	upd, err := svc.UpdateRule("bot-partial", created.ID, RuleReq{Enabled: boolp(false)})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Tool != "sandbox_exec" {
		t.Errorf("tool must be preserved, got %q", upd.Tool)
	}
	if upd.Platform != "telegram" {
		t.Errorf("platform must be preserved, got %q", upd.Platform)
	}
	if upd.Decision != DecisionDeny {
		t.Errorf("decision must be preserved (allow would be privilege escalation), got %q", upd.Decision)
	}
	if len(upd.UserIDs) != 1 || upd.UserIDs[0] != "u1" {
		t.Errorf("userIds must be preserved, got %v", upd.UserIDs)
	}
	if upd.Sort != 5 {
		t.Errorf("sort must be preserved, got %d", upd.Sort)
	}
	if upd.Enabled {
		t.Error("enabled should have been set to false")
	}

	// 仅改 sort（模拟上移/下移）同样不得动其他字段
	upd2, err := svc.UpdateRule("bot-partial", created.ID, RuleReq{Sort: intp(1)})
	if err != nil {
		t.Fatal(err)
	}
	if upd2.Tool != "sandbox_exec" || upd2.Platform != "telegram" || upd2.Decision != DecisionDeny {
		t.Errorf("sort-only update must not touch other fields: %+v", upd2)
	}
	if upd2.Sort != 1 {
		t.Errorf("sort should be 1, got %d", upd2.Sort)
	}
	if upd2.Enabled {
		t.Error("enabled should remain false after sort-only update")
	}

	// 显式全量更新仍然生效
	upd3, err := svc.UpdateRule("bot-partial", created.ID, RuleReq{
		Tool: "web_search", Platform: "web", UserIDs: []string{"u2", "u3"},
		Decision: DecisionAllow, Enabled: boolp(true), Sort: intp(9),
	})
	if err != nil {
		t.Fatal(err)
	}
	if upd3.Tool != "web_search" || upd3.Platform != "web" || upd3.Decision != DecisionAllow {
		t.Errorf("explicit full update should apply, got %+v", upd3)
	}
	if len(upd3.UserIDs) != 2 {
		t.Errorf("userIds should be replaced, got %v", upd3.UserIDs)
	}
}
