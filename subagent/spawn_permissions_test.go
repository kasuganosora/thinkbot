package subagent

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/toolperm"
)

// toolCaptureProvider 记录每次调用携带的工具名（子代理实际拿到的工具）。
type toolCaptureProvider struct {
	mu    sync.Mutex
	tools [][]string
}

func (p *toolCaptureProvider) Name() string { return "capture" }

func (p *toolCaptureProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	names := make([]string, 0, len(params.Tools))
	for _, t := range params.Tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	p.mu.Lock()
	p.tools = append(p.tools, names)
	p.mu.Unlock()
	return &llm.GenerateResult{Text: "done", FinishReason: llm.FinishReasonStop}, nil
}

func (p *toolCaptureProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	r, err := p.DoGenerate(ctx, params)
	if err != nil {
		return nil, err
	}
	ch := make(chan llm.StreamPart, 1)
	ch <- &llm.TextDeltaPart{Text: r.Text}
	close(ch)
	return &llm.StreamResult{Stream: ch}, nil
}

const (
	permBot    = "bot-perm"
	tgAdmin    = "10001"
	tgStranger = "20002"
)

// workspaceTools 是子代理干活需要的敏感工具（exec / 浏览器 / web / 文件）。
var workspaceTools = []string{"browser__fetch", "sandbox_exec", "sandbox_read_file", "web_fetch"}

// newPermHarness 复现线上配置：telegram 平台的权限规则只对管理员放行工作空间工具。
func newPermHarness(t *testing.T) (*tools.ToolManager, *SubAgentManager, *toolCaptureProvider) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := dao.Migrate(db); err != nil {
		t.Fatal(err)
	}
	svc := toolperm.NewService(db, nil)
	enabled, sortv := true, 0
	for _, name := range append(append([]string{}, workspaceTools...), "spawn", "misskey_create_note") {
		if _, err := svc.CreateRule(permBot, toolperm.RuleReq{
			Tool: name, Platform: "telegram", UserIDs: []string{tgAdmin},
			Decision: toolperm.DecisionAllow, Enabled: &enabled, Sort: &sortv,
		}); err != nil {
			t.Fatal(err)
		}
	}

	mgr := tools.NewToolManager(prompt.NewRegistry(), nil, nil)
	mgr.SetAccessEvaluator(svc.NewEvaluator())
	noop := llm.ToolExecuteFunc(func(*llm.ToolExecContext, any) (any, error) { return "ok", nil })
	for _, name := range workspaceTools {
		if err := mgr.Register(tools.ToolDef{Tool: llm.Tool{Name: name, Description: name, Execute: noop}}); err != nil {
			t.Fatal(err)
		}
	}
	// 对外发言工具：父回合（TG 管理员）可用，子代理一律不可用。
	if err := mgr.Register(tools.ToolDef{Tool: llm.Tool{Name: "misskey_create_note", Description: "post", Execute: noop}}); err != nil {
		t.Fatal(err)
	}
	// 仅子代理场景可见的工具：父回合没有 → 子代理也不得拿到（不多于父回合）。
	if err := mgr.Register(tools.ToolDef{Scopes: []string{"subagent"}, Tool: llm.Tool{Name: "subagent_only", Description: "x", Execute: noop}}); err != nil {
		t.Fatal(err)
	}

	prov := &toolCaptureProvider{}
	sa := NewSubAgentManager(prov, "test-model")
	sa.SetToolResolver(mgr, tools.ToolSessionContext{BotID: permBot})
	if err := RegisterTools(mgr, sa); err != nil {
		t.Fatal(err)
	}
	return mgr, sa, prov
}

func tgTurn(userID string) tools.ToolSessionContext {
	return tools.ToolSessionContext{
		BotID: permBot, Channel: userID, ChatID: userID, ChatType: "private",
		UserID: userID, UserIdentifiers: []string{userID}, SourceChannelType: "telegram",
	}
}

func names(ts []llm.Tool) map[string]bool {
	m := make(map[string]bool, len(ts))
	for _, t := range ts {
		m[t.Name] = true
	}
	return m
}

// 回归（09-28 06:55）：spawn 的子代理只带 BotID，按空平台评估，敏感工具默认禁止。
func TestSubagentWithoutParentContextLacksWorkspaceTools(t *testing.T) {
	_, sa, _ := newPermHarness(t)
	got, err := sa.resolveTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range workspaceTools {
		if names(got)[n] {
			t.Fatalf("without a parent context %s should stay denied (old behaviour), got %v", n, got)
		}
	}
}

func TestSpawnFromTelegramAdminTurnGetsWorkspaceTools(t *testing.T) {
	mgr, _, prov := newPermHarness(t)
	parent := tgTurn(tgAdmin)
	ctx := tools.ContextWithSessionContext(context.Background(), parent)

	parentTools, err := mgr.ResolveTools(ctx, &parent)
	if err != nil {
		t.Fatal(err)
	}
	var spawn *llm.Tool
	for i := range parentTools {
		if parentTools[i].Name == "spawn" {
			spawn = &parentTools[i]
		}
	}
	if spawn == nil {
		t.Fatalf("parent turn should have spawn, got %v", parentTools)
	}
	if _, err := spawn.Execute(&llm.ToolExecContext{Context: ctx, ToolName: "spawn"}, map[string]any{
		"tasks": []any{"check the workspace"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(prov.tools) == 0 {
		t.Fatal("sub-agent never called the model")
	}
	// 发给模型的工具名会剥掉 sandbox_ 前缀（orchestrate），比较前统一剥掉。
	strip := func(n string) string { return strings.TrimPrefix(n, llm.SandboxToolPrefix) }
	sub := map[string]bool{}
	for _, n := range prov.tools[0] {
		sub[strip(n)] = true
	}
	for _, n := range workspaceTools {
		if !sub[strip(n)] {
			t.Errorf("spawned sub-agent from a TG admin turn must get %s, got %v", n, prov.tools[0])
		}
	}
	pm := map[string]bool{}
	for _, t := range parentTools {
		pm[strip(t.Name)] = true
	}
	for n := range sub {
		if !pm[n] {
			t.Errorf("sub-agent got %s which the parent turn does not have (%v)", n, prov.tools[0])
		}
	}
	for _, n := range []string{"spawn", "misskey_create_note", "subagent_only"} {
		if sub[n] {
			t.Errorf("sub-agent must not get %s, got %v", n, prov.tools[0])
		}
	}
}

func TestSpawnFromTelegramStrangerTurnGetsNoWorkspaceTools(t *testing.T) {
	_, sa, _ := newPermHarness(t)
	ctx := tools.ContextWithSessionContext(context.Background(), tgTurn(tgStranger))
	got, err := sa.resolveTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range workspaceTools {
		if names(got)[n] {
			t.Fatalf("a non-admin turn must not hand %s to its sub-agent, got %v", n, got)
		}
	}
}
