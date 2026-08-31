package tools

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/llm"
)

// ListAllTools 必须包含动态 ToolProvider 提供的工具。
//
// 回归背景：工具权限界面原先只调 ListTools()（仅静态注册表），
// 导致 MCP 动态工具（如 browser__navigate）在 UI 上完全不可见 ——
// 管理员无法为浏览器工具配置允许/禁止规则，尽管这些工具在
// ResolveTools 中确实会过 toolperm 评估。
func TestListAllTools_IncludesDynamic(t *testing.T) {
	mgr := NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())

	if err := mgr.Register(ToolDef{
		Tool:     llm.Tool{Name: "calculate", Description: "Calc"},
		Category: "utility",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr.AddProvider(ToolFunc(func(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
		return []llm.Tool{
			{Name: "browser__navigate", Description: "Open a URL"},
			{Name: "browser__click", Description: "Click element"},
		}, nil
	}))

	all := mgr.ListAllTools(context.Background())
	names := make(map[string]ToolInfo, len(all))
	for _, ti := range all {
		names[ti.Name] = ti
	}

	for _, want := range []string{"calculate", "browser__navigate", "browser__click"} {
		if _, ok := names[want]; !ok {
			t.Errorf("ListAllTools missing %q; got %v", want, all)
		}
	}

	// 动态工具应带 DynamicCategory，供 API 层映射中文分组名
	if got := names["browser__navigate"].Category; got != DynamicCategory {
		t.Errorf("browser__navigate Category: got %q, want %q", got, DynamicCategory)
	}
	// 静态工具的 Category 不能被覆盖
	if got := names["calculate"].Category; got != "utility" {
		t.Errorf("calculate Category: got %q, want utility", got)
	}
	// 描述必须保留（UI 需要展示）
	if got := names["browser__navigate"].Description; got != "Open a URL" {
		t.Errorf("browser__navigate Description: got %q", got)
	}
}

// 同名时静态工具优先，且不产生重复项（与 Resolve 的去重语义一致）。
func TestListAllTools_StaticWinsOnDuplicate(t *testing.T) {
	mgr := NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	if err := mgr.Register(ToolDef{
		Tool:     llm.Tool{Name: "shared", Description: "static version"},
		Category: "utility",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mgr.AddProvider(ToolFunc(func(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
		return []llm.Tool{{Name: "shared", Description: "dynamic version"}}, nil
	}))

	all := mgr.ListAllTools(context.Background())
	count := 0
	for _, ti := range all {
		if ti.Name == "shared" {
			count++
			if ti.Description != "static version" {
				t.Errorf("expected static version to win, got %q", ti.Description)
			}
			if ti.Category != "utility" {
				t.Errorf("expected static category utility, got %q", ti.Category)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 %q entry, got %d", "shared", count)
	}
}

// provider 出错时不应让整个列表失败（MCP 服务器可能临时不可用），
// 静态工具必须照常返回。
func TestListAllTools_ProviderErrorIsSkipped(t *testing.T) {
	mgr := NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	if err := mgr.Register(ToolDef{
		Tool:     llm.Tool{Name: "calculate", Description: "Calc"},
		Category: "utility",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mgr.AddProvider(ToolFunc(func(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
		return nil, errors.New("mcp server unreachable")
	}))
	mgr.AddProvider(ToolFunc(func(ctx context.Context, sctx *ToolSessionContext) ([]llm.Tool, error) {
		return []llm.Tool{{Name: "browser__navigate", Description: "Open a URL"}}, nil
	}))

	all := mgr.ListAllTools(context.Background())
	if len(all) != 2 {
		t.Fatalf("expected 2 tools (static + healthy provider), got %d: %v", len(all), all)
	}
	// 结果需按名称排序，便于 UI 稳定展示
	if all[0].Name != "browser__navigate" || all[1].Name != "calculate" {
		t.Errorf("expected sorted by name, got %q, %q", all[0].Name, all[1].Name)
	}
}

// 没有任何 provider 时，行为与 ListTools 一致。
func TestListAllTools_NoProviders(t *testing.T) {
	mgr := NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	if err := mgr.Register(ToolDef{
		Tool:     llm.Tool{Name: "now", Description: "Current time"},
		Category: "utility",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	all := mgr.ListAllTools(context.Background())
	if len(all) != 1 || all[0].Name != "now" {
		t.Fatalf("expected only static tool now, got %v", all)
	}
}
