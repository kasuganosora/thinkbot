package tools

import (
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/llm"
)

func TestListTools_Empty(t *testing.T) {
	mgr := NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	tools := mgr.ListTools()
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(tools))
	}
}

func TestListTools_Details(t *testing.T) {
	mgr := NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())

	err := mgr.RegisterMany(
		ToolDef{
			Tool:     llm.Tool{Name: "search", Description: "Search the web"},
			Category: "search",
			Scopes:   []string{"private", "group"},
		},
		ToolDef{
			Tool:     llm.Tool{Name: "calc", Description: "Calculate math expression"},
			Category: "utility",
			Scopes:   []string{},
			PromptSection: &ToolPromptSection{
				Name:    "calc_rules",
				Order:   310,
				Content: "Only use standard math operations.",
				Enabled: true,
			},
			RequireApproval: true,
		},
	)
	if err != nil {
		t.Fatalf("RegisterMany: %v", err)
	}

	tools := mgr.ListTools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	// 按名称排序：calc, search
	if tools[0].Name != "calc" {
		t.Errorf("expected tools[0].Name=calc, got %q", tools[0].Name)
	}
	if tools[1].Name != "search" {
		t.Errorf("expected tools[1].Name=search, got %q", tools[1].Name)
	}

	// 验证 calc 详情
	calc := tools[0]
	if calc.Category != "utility" {
		t.Errorf("calc Category: got %q", calc.Category)
	}
	if !calc.RequireApproval {
		t.Error("calc RequireApproval should be true")
	}
	if !calc.HasPromptSection {
		t.Error("calc HasPromptSection should be true")
	}

	// 验证 search 详情
	search := tools[1]
	if search.Category != "search" {
		t.Errorf("search Category: got %q", search.Category)
	}
	if search.RequireApproval {
		t.Error("search RequireApproval should be false")
	}
	if search.HasPromptSection {
		t.Error("search HasPromptSection should be false")
	}
	if len(search.Scopes) != 2 {
		t.Errorf("search Scopes len: got %d", len(search.Scopes))
	}
}

func TestListTools_AfterUnregister(t *testing.T) {
	mgr := NewToolManager(prompt.NewRegistry(), nil, zap.NewNop().Sugar())
	_ = mgr.Register(ToolDef{
		Tool:     llm.Tool{Name: "temp_tool", Description: "Temporary tool"},
		Category: "test",
	})

	if len(mgr.ListTools()) != 1 {
		t.Fatal("expected 1 tool before unregister")
	}

	mgr.Unregister("temp_tool")

	if len(mgr.ListTools()) != 0 {
		t.Fatal("expected 0 tools after unregister")
	}
}
