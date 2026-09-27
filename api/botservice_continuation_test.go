package api

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/toolperm"
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

func TestContinuationIdentity(t *testing.T) {
	tg := &workflow.Origin{ChannelType: "telegram", ChatID: "76017910", UserID: "76017910", Username: "sion"}
	cases := []struct {
		name     string
		origin   *workflow.Origin
		platform string
		chatID   string
		want     continuationUser
	}{
		{"tg origin, same chat", tg, "telegram", "76017910", continuationUser{"76017910", "sion"}},
		{"tg origin, other chat", tg, "telegram", "999", continuationUser{userID: "system"}},
		{"tg origin injected via web", tg, "web", "", continuationUser{userID: "system"}},
		{"no origin (old workflow)", nil, "telegram", "76017910", continuationUser{userID: "system"}},
		{"web origin", &workflow.Origin{ChannelType: "web", UserID: "alice"}, "web", "", continuationUser{userID: "alice"}},
	}
	for _, c := range cases {
		if got := continuationIdentity(c.origin, c.platform, c.chatID); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// 2026-09-28: the continuation message reused the session id as message id
// (second one dropped by ingress dedup) and its reply was never stored.
func TestTelegramContinuationMessage(t *testing.T) {
	id := continuationUser{userID: "76017910", username: "sion"}
	extra := map[string]any{agenttools.ExtraKeyChatSessionID: "tg:76017910", metaKeyWorkflowContinuation: "wf-1"}
	a := buildTelegramContinuationMessage("bot", "Telegram", "76017910", idgen.New("wfc"), "t1", id, "x", extra)
	b := buildTelegramContinuationMessage("bot", "Telegram", "76017910", idgen.New("wfc"), "t2", id, "y", extra)
	if a.ID == b.ID || a.ID == "tg:76017910" || a.ID == "" {
		t.Fatalf("continuation message ids must be unique and never the session id: %q %q", a.ID, b.ID)
	}
	if a.Metadata["__history_managed"] != true {
		t.Fatal("continuation reply must be persisted by the outbound chat-history enricher")
	}
	if a.UserID != "76017910" || a.Metadata["username"] != "sion" || a.Metadata["channel_type"] != "telegram" ||
		a.Metadata[metaKeyWorkflowContinuation] != "wf-1" {
		t.Fatalf("message = %+v", a)
	}
}

// A continuation of a workflow started in a Telegram admin turn resolves the
// same tools as that turn (exec/web/browser/spawn), a stranger's gets none of
// them, and a workflow without a stored origin keeps the old "system" identity.
func TestContinuationInheritsOriginalTurnTools(t *testing.T) {
	const botID, admin, stranger = "bot-perm", "10001", "20002"
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=private"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := dao.Migrate(db); err != nil {
		t.Fatal(err)
	}
	svc := toolperm.NewService(db, nil)
	sensitive := []string{"sandbox_exec", "web_fetch", "browser__fetch", "spawn"}
	enabled, sortv := true, 0
	for _, name := range sensitive {
		if _, err := svc.CreateRule(botID, toolperm.RuleReq{Tool: name, Platform: "telegram", UserIDs: []string{admin},
			Decision: toolperm.DecisionAllow, Enabled: &enabled, Sort: &sortv}); err != nil {
			t.Fatal(err)
		}
	}
	mgr := agenttools.NewToolManager(prompt.NewRegistry(), nil, nil)
	mgr.SetAccessEvaluator(svc.NewEvaluator())
	noop := llm.ToolExecuteFunc(func(*llm.ToolExecContext, any) (any, error) { return "ok", nil })
	for _, name := range append([]string{"memory"}, sensitive...) {
		if err := mgr.Register(agenttools.ToolDef{Tool: llm.Tool{Name: name, Description: name, Execute: noop}}); err != nil {
			t.Fatal(err)
		}
	}
	resolve := func(msg core.Message) map[string]bool {
		env := core.NewEnvelope(msg)
		env.Set("bot.id", botID)
		got, err := mgr.ResolveForEnvelope(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, tl := range got {
			m[tl.Name] = true
		}
		return m
	}
	turn := func(user string) core.Message {
		return core.Message{ID: "m", BotID: botID, Source: "Telegram", Channel: user, ChatType: core.ChatPrivate,
			UserID: user, Metadata: map[string]any{"channel_type": "telegram"}}
	}
	continuation := func(origin *workflow.Origin, chat string) core.Message {
		id := continuationIdentity(origin, "telegram", chat)
		return buildTelegramContinuationMessage(botID, "Telegram", chat, idgen.New("wfc"), "t", id, "x", nil)
	}

	adminTurn := resolve(turn(admin))
	adminCont := resolve(continuation(&workflow.Origin{ChannelType: "telegram", ChatID: admin, UserID: admin}, admin))
	for _, n := range sensitive {
		if !adminTurn[n] || !adminCont[n] {
			t.Fatalf("%s: admin turn %v, continuation %v", n, adminTurn[n], adminCont[n])
		}
	}
	for n := range adminCont {
		if !adminTurn[n] {
			t.Fatalf("continuation got %s which the original turn did not have", n)
		}
	}
	strangerCont := resolve(continuation(&workflow.Origin{ChannelType: "telegram", ChatID: stranger, UserID: stranger}, stranger))
	legacyCont := resolve(continuation(nil, admin))
	for _, n := range sensitive {
		if strangerCont[n] || legacyCont[n] {
			t.Fatalf("%s must stay denied: stranger %v, no-origin %v", n, strangerCont[n], legacyCont[n])
		}
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
