package toolperm

import (
	"context"
	"testing"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newDiagDB 起一个带完整 schema 的内存库，供需要自定义 logger 的用例使用。
func newDiagDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := dao.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestEvaluator_FilterTools_LogsHiddenTools 固化「工具被权限摘掉时必须留痕」。
//
// 回归背景（2026-10-26）：企微 bot 的 mcp_server 被白名单模式静默从工具列表里
// 移除，日志一行没有，模型侧只表现为 tool_search 无匹配，排查只能靠反推规则表。
// 过滤本身不可省，但过滤必须可见 —— 这条测试保证可见性不被后续重构悄悄删掉。
func TestEvaluator_FilterTools_LogsHiddenTools(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	svc := NewService(newDiagDB(t), zap.New(core).Sugar())

	// telegram 上只放行基础工具：sandbox_exec 是敏感工具，会被过滤掉。
	if _, err := svc.CreateRule("bot-log", RuleReq{
		Tool: "now", Platform: "telegram", UserIDs: []string{"*"},
		Decision: DecisionAllow, Enabled: boolp(true), Sort: intp(1),
	}); err != nil {
		t.Fatal(err)
	}

	tools := []llm.Tool{{Name: "now"}, {Name: "sandbox_exec"}}
	out, err := svc.NewEvaluator().FilterTools(context.Background(), tools,
		&agenttools.ToolSessionContext{BotID: "bot-log", SourceChannelType: "telegram", UserID: "u1"})
	if err != nil {
		t.Fatalf("FilterTools: %v", err)
	}
	if len(out) != 1 || out[0].Name != "now" {
		t.Fatalf("expected only now, got %+v", out)
	}

	entries := logs.FilterMessage("toolperm: tools hidden by permission").All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d (%+v)", len(entries), logs.All())
	}
	e := entries[0]
	if got := e.ContextMap()["filtered"]; got != int64(1) {
		t.Errorf("filtered count: got %v", got)
	}
	if got := e.ContextMap()["tools"]; got != "sandbox_exec" {
		t.Errorf("filtered tool names: got %v", got)
	}
	if got := e.ContextMap()["platform"]; got != "telegram" {
		t.Errorf("platform: got %v", got)
	}
}

// TestEvaluator_FilterTools_NoLogWhenNothingHidden 没有被过滤的工具时不打日志，
// 避免每轮都刷一行噪音。
func TestEvaluator_FilterTools_NoLogWhenNothingHidden(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	svc := NewService(newDiagDB(t), zap.New(core).Sugar())

	if _, err := svc.CreateRule("bot-quiet", RuleReq{
		Tool: "*", Platform: "web", UserIDs: []string{"*"},
		Decision: DecisionAllow, Enabled: boolp(true), Sort: intp(0),
	}); err != nil {
		t.Fatal(err)
	}

	tools := []llm.Tool{{Name: "now"}, {Name: "sandbox_exec"}}
	if _, err := svc.NewEvaluator().FilterTools(context.Background(), tools,
		&agenttools.ToolSessionContext{BotID: "bot-quiet", SourceChannelType: "web", UserID: "u1"}); err != nil {
		t.Fatalf("FilterTools: %v", err)
	}
	if n := len(logs.FilterMessage("toolperm: tools hidden by permission").All()); n != 0 {
		t.Fatalf("expected no log when nothing filtered, got %d", n)
	}
}
