package api

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/util/idgen"
	"github.com/kasuganosora/thinkbot/workflow"
)

// TestSplitSessionChannel 锁定工作流续跑的会话渠道解析逻辑：
// onWorkflowCompleted 依赖它把续跑指令路由到正确的渠道（TG 走真实渠道主动推回，
// web/未知退回 WebChannel）。解析错误会导致完成总结错投或丢失。
func TestSplitSessionChannel(t *testing.T) {
	cases := []struct {
		in     string
		kind   string
		target string
	}{
		{"tg:76019910", "tg", "76019910"},
		{"web:abc", "web", "abc"},
		{"mk:channel:user123", "mk", "channel:user123"},
		{"session-no-colon", "", "session-no-colon"},
		{"", "", ""},
	}
	for _, c := range cases {
		k, tgt := splitSessionChannel(c.in)
		if k != c.kind || tgt != c.target {
			t.Errorf("splitSessionChannel(%q) = (%q,%q), want (%q,%q)",
				c.in, k, tgt, c.kind, c.target)
		}
	}
}

func TestWorkflowContinuationText(t *testing.T) {
	done := &workflow.Workflow{ID: "wf-ok", Status: workflow.WorkflowCompleted, Requirement: "do it",
		Nodes: []*workflow.DAGNode{{ID: "n1", Status: workflow.NodeCompleted}}}
	if txt := workflowContinuationText(done); !strings.Contains(txt, "已执行完成") || !strings.Contains(txt, "do it") {
		t.Fatalf("completed text: %s", txt)
	}
	// 2026-09-28: a failed workflow was announced as "已执行完成（共 2 个节点，0 个已完成）".
	failed := &workflow.Workflow{ID: "wf-bad", Status: workflow.WorkflowFailed, Requirement: "do it", Error: "boom",
		Nodes: []*workflow.DAGNode{{ID: "n1", Status: workflow.NodeFailed}, {ID: "n2", Status: workflow.NodeSkipped}}}
	txt := workflowContinuationText(failed)
	if strings.Contains(txt, "已执行完成") || !strings.Contains(txt, "没有成功") || !strings.Contains(txt, "boom") ||
		!strings.Contains(txt, "不要声称任务已经完成") {
		t.Fatalf("failed text: %s", txt)
	}
}

// 2026-09-28: the continuation message reused the session id as message id
// (second one dropped by ingress dedup) and its reply was never stored.
func TestTelegramContinuationMessage(t *testing.T) {
	id := continuationUser{userID: "system"}
	extra := map[string]any{agenttools.ExtraKeyChatSessionID: "tg:76017910", metaKeyWorkflowContinuation: "wf-1"}
	a := buildTelegramContinuationMessage("bot", "Telegram", "76017910", idgen.New("wfc"), "t1", id, "x", extra)
	b := buildTelegramContinuationMessage("bot", "Telegram", "76017910", idgen.New("wfc"), "t2", id, "y", extra)
	if a.ID == b.ID || a.ID == "tg:76017910" || a.ID == "" {
		t.Fatalf("continuation message ids must be unique and never the session id: %q %q", a.ID, b.ID)
	}
	if a.Metadata["__history_managed"] != true {
		t.Fatal("continuation reply must be persisted by the outbound chat-history enricher")
	}
	if a.UserID != "system" || a.Metadata["channel_type"] != "telegram" ||
		a.Metadata[metaKeyWorkflowContinuation] != "wf-1" {
		t.Fatalf("message = %+v", a)
	}
}

// The ack stage confirms the workflow only when the continuation turn ran to
// the end (ctx not cancelled by shutdown).
func TestWorkflowContinuationAckStage(t *testing.T) {
	repo := workflow.NewRepository(nil, zap.NewNop().Sugar())
	wf := workflow.NewWorkflow("wf-ack", "req", nil)
	wf.Status = workflow.WorkflowCompleted
	wf.BotID = "bot-a"
	if err := repo.Save(wf); err != nil {
		t.Fatal(err)
	}
	m := workflow.NewManager(repo, nil, nil, nil, workflow.EngineConfig{MaxParallel: 1}, zap.NewNop().Sugar(), nil)
	m.SetNeedsContinuation("wf-ack", true)
	s := &BotService{wfEngines: map[string]*workflow.Manager{"bot-a": m}}
	stage := s.workflowContinuationAckStage()
	env := core.NewEnvelope(core.Message{BotID: "bot-a", Metadata: map[string]any{metaKeyWorkflowContinuation: "wf-ack"}})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stage.Process(cancelled, env); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get("wf-ack"); !got.NeedsContinuation {
		t.Fatal("a turn cut short by shutdown must not confirm the continuation")
	}
	if _, err := stage.Process(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get("wf-ack"); got.NeedsContinuation {
		t.Fatal("a finished continuation turn must clear the persisted flag")
	}
}
