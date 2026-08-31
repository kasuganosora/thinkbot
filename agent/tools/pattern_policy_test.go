package tools

import (
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// Pattern 匹配测试
// ============================================================================

func TestMatchPattern_Exact(t *testing.T) {
	if !matchPattern("web_search", "web_search") {
		t.Error("expected exact match")
	}
	if matchPattern("web_search", "web_fetch") {
		t.Error("expected no match for different names")
	}
}

func TestMatchPattern_Wildcard_Prefix(t *testing.T) {
	if !matchPattern("sandbox_*", "sandbox_exec") {
		t.Error("expected prefix wildcard match")
	}
	if !matchPattern("sandbox_*", "sandbox_read_file") {
		t.Error("expected prefix wildcard match")
	}
	if matchPattern("sandbox_*", "web_fetch") {
		t.Error("expected no match for different prefix")
	}
}

func TestMatchPattern_Wildcard_Suffix(t *testing.T) {
	if !matchPattern("*_file", "read_file") {
		t.Error("expected suffix wildcard match")
	}
	if !matchPattern("*_file", "write_file") {
		t.Error("expected suffix wildcard match")
	}
}

func TestMatchPattern_Wildcard_All(t *testing.T) {
	if !matchPattern("*", "anything") {
		t.Error("expected * to match everything")
	}
	if !matchPattern("*", "sandbox_exec") {
		t.Error("expected * to match sandbox_exec")
	}
}

func TestMatchPattern_Wildcard_Middle(t *testing.T) {
	if !matchPattern("read_*_file", "read_text_file") {
		t.Error("expected middle wildcard match")
	}
}

func TestMatchPattern_Or(t *testing.T) {
	if !matchPattern("web_search|web_fetch", "web_search") {
		t.Error("expected OR match for web_search")
	}
	if !matchPattern("web_search|web_fetch", "web_fetch") {
		t.Error("expected OR match for web_fetch")
	}
	if matchPattern("web_search|web_fetch", "calculate") {
		t.Error("expected no OR match for calculate")
	}
}

// ============================================================================
// PatternPolicy 测试
// ============================================================================

func TestPatternPolicy_Evaluate_DefaultAllow(t *testing.T) {
	p := PatternPolicy{}
	decision := p.Evaluate("unknown_tool", "telegram", "group")
	if decision != PermAllow {
		t.Errorf("expected allow by default, got %s", decision)
	}
}

func TestPatternPolicy_Evaluate_Deny(t *testing.T) {
	p := PatternPolicy{
		Rules: []PatternRule{
			{Pattern: "sandbox_exec", Decision: PermDeny},
		},
	}
	decision := p.Evaluate("sandbox_exec", "telegram", "group")
	if decision != PermDeny {
		t.Errorf("expected deny, got %s", decision)
	}
}

func TestPatternPolicy_Evaluate_LastMatchWins(t *testing.T) {
	p := PatternPolicy{
		Rules: []PatternRule{
			{Pattern: "sandbox_*", Decision: PermDeny},
			{Pattern: "sandbox_read_*", Decision: PermAllow},
		},
	}
	// sandbox_read_file 应该匹配最后一条规则 = allow
	decision := p.Evaluate("sandbox_read_file", "telegram", "group")
	if decision != PermAllow {
		t.Errorf("expected allow (last match), got %s", decision)
	}
	// sandbox_exec 只匹配第一条 = deny
	decision = p.Evaluate("sandbox_exec", "telegram", "group")
	if decision != PermDeny {
		t.Errorf("expected deny, got %s", decision)
	}
}

func TestPatternPolicy_Evaluate_ContextFilter(t *testing.T) {
	p := PatternPolicy{
		Rules: []PatternRule{
			{Pattern: "sandbox_exec", Decision: PermDeny, Channel: "telegram"},
			{Pattern: "sandbox_exec", Decision: PermAllow, Channel: "web"},
		},
	}
	if p.Evaluate("sandbox_exec", "telegram", "group") != PermDeny {
		t.Error("expected deny on telegram")
	}
	if p.Evaluate("sandbox_exec", "web", "group") != PermAllow {
		t.Error("expected allow on web")
	}
}

func TestPatternPolicy_FilterTools(t *testing.T) {
	p := PatternPolicy{
		Rules: []PatternRule{
			{Pattern: "sandbox_*", Decision: PermDeny},
		},
	}
	tools := []llm.Tool{
		{Name: "sandbox_exec"},
		{Name: "web_search"},
		{Name: "calculate"},
	}

	sctx := &ToolSessionContext{Channel: "telegram", ChatType: "group"}
	filtered := p.FilterTools(tools, sctx)

	if len(filtered) != 2 {
		t.Fatalf("expected 2 tools after filter, got %d", len(filtered))
	}
	for _, tool := range filtered {
		if tool.Name == "sandbox_exec" {
			t.Error("sandbox_exec should be filtered out")
		}
	}
}

// ============================================================================
// 预设策略测试
// ============================================================================

func TestReadOnlyPolicy(t *testing.T) {
	p := ReadOnlyPolicy()
	// 写操作被拒绝
	if p.Evaluate("sandbox_write_file", "web", "group") != PermDeny {
		t.Error("expected deny for sandbox_write_file")
	}
	if p.Evaluate("sandbox_exec", "web", "group") != PermDeny {
		t.Error("expected deny for sandbox_exec")
	}
	// 读操作允许
	if p.Evaluate("sandbox_read_file", "web", "group") != PermAllow {
		t.Error("expected allow for sandbox_read_file")
	}
}

func TestSafePolicy(t *testing.T) {
	p := SafePolicy()
	// 危险操作需要确认
	if p.Evaluate("sandbox_exec", "web", "group") != PermAsk {
		t.Error("expected ask for sandbox_exec")
	}
	// 安全操作允许
	if p.Evaluate("web_search", "web", "group") != PermAllow {
		t.Error("expected allow for web_search")
	}
}

func TestSubagentPolicy(t *testing.T) {
	p := SubagentPolicy()
	// 允许的工具
	if p.Evaluate("now", "web", "group") != PermAllow {
		t.Error("expected allow for now")
	}
	if p.Evaluate("web_search", "web", "group") != PermAllow {
		t.Error("expected allow for web_search")
	}
	// 危险工具被拒绝
	if p.Evaluate("sandbox_exec", "web", "group") != PermDeny {
		t.Error("expected deny for sandbox_exec")
	}
	// 未知工具默认拒绝
	if p.Evaluate("unknown_tool", "web", "group") != PermDeny {
		t.Error("expected deny for unknown tool")
	}
}

func TestGroupChatPolicy(t *testing.T) {
	p := GroupChatPolicy()
	if p.Evaluate("sandbox_exec", "telegram", "group") != PermDeny {
		t.Error("expected deny for sandbox_exec in group")
	}
	if p.Evaluate("web_search", "telegram", "group") != PermAllow {
		t.Error("expected allow for web_search in group")
	}
}
