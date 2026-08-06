package api

import (
	"testing"

	"github.com/kasuganosora/thinkbot/workflow"
)

// TestWorkflowEngine_PrefersEquippedEngine 守住 2026-08-06 线上事故的修复：
// WorkflowService（Recover / Sweeper / UI 重试）必须复用 BotService 已装配
// 工作区工具的引擎，而不是自建 ToolMgr=nil 的残废实例。
//
// 事故表现：进程重启后 Recover 接管工作流，节点执行的 SubAgent 碰不到工作区，
// 产出从 5000~10000 字的真实审查报告退化成 48~117 字的「我将先探索项目结构…」
// 纯计划，且照样通过 review 被判completed —— 工作成果被静默丢弃。
func TestWorkflowEngine_PrefersEquippedEngine(t *testing.T) {
	s := &BotService{wfEngines: make(map[string]*workflow.Manager)}

	if got := s.WorkflowEngine(); got != nil {
		t.Fatalf("没有任何 bot 启动时应返回 nil，实际%v", got)
	}

	// 用非 nil 的哨兵值代表「已装配的引擎」——这里只验证发布/查找/清理的接线，
	// 不需要真正跑起来的引擎。
	equipped := &workflow.Manager{}
	s.publishWorkflowEngine("bot-1", equipped)

	got := s.WorkflowEngine()
	if got == nil {
		t.Fatal("已发布装配好的引擎，WorkflowEngine() 却返回 nil —— " +
			"WorkflowService 会退化去自建 ToolMgr=nil 的残废实例")
	}
	if got != equipped {
		t.Errorf("返回的不是已发布的那个引擎：want %p got %p", equipped, got)
	}
}

// TestPublishWorkflowEngine_IgnoresNil 验证 nil 引擎不会被登记，
// 否则 WorkflowEngine() 遍历时可能返回 nil 却被调用方当成有效实例。
func TestPublishWorkflowEngine_IgnoresNil(t *testing.T) {
	s := &BotService{wfEngines: make(map[string]*workflow.Manager)}
	s.publishWorkflowEngine("bot-1", nil)

	if len(s.wfEngines) != 0 {
		t.Errorf("nil 引擎不应被登记，实际 map 有 %d 项", len(s.wfEngines))
	}
	if got := s.WorkflowEngine(); got != nil {
		t.Errorf("期望 nil，实际 %v", got)
	}
}

// TestWorkflowEngine_SkipsNilEntries 验证遍历时跳过 nil 值。
// 直接构造含 nil 的 map（模拟未来某处绕过 publishWorkflowEngine 写入），
// 确保 WorkflowEngine() 不会把 nil 当成可用引擎交出去。
func TestWorkflowEngine_SkipsNilEntries(t *testing.T) {
	equipped := &workflow.Manager{}
	s := &BotService{wfEngines: map[string]*workflow.Manager{
		"bot-nil": nil,
		"bot-ok":  equipped,
	}}

	if got := s.WorkflowEngine(); got != equipped {
		t.Errorf("应跳过 nil 项返回可用引擎：want %p got %p", equipped, got)
	}
}
