package workflow

import (
	"context"
	"testing"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// TestLatestWorkflowForSession_MatchesBotAndSession 验证按bot + 会话能查回最近一条
// 工作流。这是前端刷新页面后恢复工作流卡片的数据来源：activeWorkflowId 只从实时 SSE
// 事件赋值，刷新即丢，而工作流仍在后台运行。
func TestLatestWorkflowForSession_MatchesBotAndSession(t *testing.T) {
	m := newTestManagerForSessionLookup(t)

	save := func(id, botID, sessionID string) {
		wf := NewWorkflow(id, "req-"+id, nil)
		wf.BotID = botID
		wf.SessionID = sessionID
		if err := m.repo.Save(wf); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	save("wf-a", "bot-1", "sess-1")
	save("wf-b", "bot-1", "sess-2")
	save("wf-c", "bot-2", "sess-1")

	got := m.LatestWorkflowForSession("bot-1", "sess-2")
	if got == nil {
		t.Fatal("期望查到 wf-b，实际 nil —— 前端刷新后无法恢复工作流卡片")
	}
	if got.ID != "wf-b" {
		t.Errorf("会话匹配错误：want wf-b, got %s", got.ID)
	}

	// 跨 bot 不能串：同样的 sessionID 在别的 bot 下不应命中
	if other := m.LatestWorkflowForSession("bot-2", "sess-1"); other == nil || other.ID != "wf-c" {
		t.Errorf("跨 bot 匹配错误：want wf-c, got %v", other)
	}
}

// TestLatestWorkflowForSession_NoMatch 验证查不到时返回 nil 而非报错/误配。
// 「这个会话没有工作流」是绝大多数会话的正常状态。
func TestLatestWorkflowForSession_NoMatch(t *testing.T) {
	m := newTestManagerForSessionLookup(t)

	wf := NewWorkflow("wf-x", "req", nil)
	wf.BotID = "bot-1"
	wf.SessionID = "sess-1"
	if err := m.repo.Save(wf); err != nil {
		t.Fatalf("save: %v", err)
	}

	if got := m.LatestWorkflowForSession("bot-1", "sess-999"); got != nil {
		t.Errorf("不存在的会话不应命中，实际返回 %s", got.ID)
	}
	if got := m.LatestWorkflowForSession("", "sess-1"); got != nil {
		t.Error("botID 为空时应返回 nil，避免跨 bot 误配")
	}
}

// TestLatestWorkflowForSession_SkipsLegacyRecords 验证历史工作流（没有 BotID/SessionID）
// 不会被误配到某个会话上。老数据是空字段，绝不能因为「空==空」就命中。
func TestLatestWorkflowForSession_SkipsLegacyRecords(t *testing.T) {
	m := newTestManagerForSessionLookup(t)

	legacy := NewWorkflow("wf-legacy", "old req", nil) // BotID/SessionID 均为空
	if err := m.repo.Save(legacy); err != nil {
		t.Fatalf("save: %v", err)
	}

	if got := m.LatestWorkflowForSession("bot-1", "sess-1"); got != nil {
		t.Errorf("历史工作流不应被误配，实际返回 %s", got.ID)
	}
}

// TestSubmitToolRecordsOriginFromContext 验证 task 工具**真的**把「来源 bot + 会话」
// 落到 Workflow 上——直接调用工具的 Execute，而不是手工构造 Workflow。
//
// 关键点：来源必须在**执行时**从 context 读取，不能在注册时捕获——
// 工具是静态注册的（bot 启动时一次），而 bot/会话每次调用才确定。
// 另一条看似可行的路（会话感知 Provider）实际不通：ToolRegistry.Resolve 里
// **同名时静态工具优先**，Provider 提供的同名 task 会被直接丢弃。
func TestSubmitToolRecordsOriginFromContext(t *testing.T) {
	m := newTestManagerForSessionLookup(t)
	def := submitToolDef(m)

	ctx := agenttools.ContextWithCallOrigin(context.Background(), agenttools.CallOrigin{
		BotID:     "bot-42",
		SessionID: "sess-42",
	})
	// 立刻取消：Submit 会同步落库初始工作流，随后的分析/调度依赖 LLM，
	// 用已取消的 ctx 让它尽快退出，本测试只关心「来源有没有落库」。
	ctx, cancel := context.WithCancel(ctx)
	cancel()

	_, _ = def.Execute(&llm.ToolExecContext{Context: ctx},
		map[string]any{"requirement": "审查所有模块"})

	got := m.LatestWorkflowForSession("bot-42", "sess-42")
	if got == nil {
		t.Fatal("task 工具没有把来源落到 Workflow 上 —— 前端刷新后无法按会话恢复卡片")
	}
	if got.BotID != "bot-42" || got.SessionID != "sess-42" {
		t.Errorf("来源落库不正确：botID=%q sessionID=%q", got.BotID, got.SessionID)
	}
}

// TestCallOriginFromContext_Empty 验证没有注入来源时返回零值而非 panic。
// 非 web 渠道 / SubAgent 路径都没有会话概念，必须容忍。
func TestCallOriginFromContext_Empty(t *testing.T) {
	origin := agenttools.CallOriginFromContext(context.Background())
	if origin.BotID != "" || origin.SessionID != "" {
		t.Errorf("空 context 应返回零值，实际 %+v", origin)
	}
}

// newTestManagerForSessionLookup 构造一个仅用于仓库查询的 Manager（纯内存 repo）。
func newTestManagerForSessionLookup(t *testing.T) *Manager {
	t.Helper()
	mgr, saMgr := Setup(WireConfig{Model: "test-model"})
	if mgr == nil {
		t.Fatal("Setup 返回 nil manager")
	}
	t.Cleanup(func() {
		if saMgr != nil {
			saMgr.CloseAll()
		}
	})
	return mgr
}
