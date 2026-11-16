package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/prompt"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/cron"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/toolperm"
)

type fakeCronRunner struct {
	fn func(ctx context.Context, env *core.Envelope) (*core.Envelope, []core.Action, error)
}

func (f fakeCronRunner) ProcessSync(ctx context.Context, env *core.Envelope) (*core.Envelope, []core.Action, error) {
	return f.fn(ctx, env)
}

func TestPresentToolList_IncludesCronWithSensitiveRisk(t *testing.T) {
	list := presentToolList(fallbackToolList())
	var cronItem *agenttools.ToolInfo
	for i := range list {
		if list[i].Name == "cron" {
			cronItem = &list[i]
			break
		}
	}
	if cronItem == nil {
		t.Fatal("expected 'cron' in fallbackToolList / presentToolList")
	}
	if cronItem.Category != "定时任务" {
		t.Errorf("expected category '定时任务', got %q", cronItem.Category)
	}
	if cronItem.Description != "管理 Bot 自身的定时任务（crontab）" {
		t.Errorf("unexpected description: %q", cronItem.Description)
	}
	if cronItem.Risk != toolperm.RiskSensitive {
		t.Errorf("expected risk=%q, got %q", toolperm.RiskSensitive, cronItem.Risk)
	}
}

func TestBotService_SharedCronStoreConsistency(t *testing.T) {
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	svc := &BotService{
		logger: zap.NewNop().Sugar(),
	}
	botID := "bot-shared-cron"

	// 1) 先用共享 Store 启动/查询一次，使 Store.loaded 变为 true（复现此前的内存分裂条件）
	sharedStore := svc.getCronStore(botID)
	if sharedStore != svc.getCronStore(botID) {
		t.Fatal("expected getCronStore to return the singleton Store for the same bot")
	}
	if got := len(sharedStore.ListActive()); got != 0 {
		t.Fatalf("expected 0 initial active jobs, got %d", got)
	}

	// 2) 注册 Agent cron 工具并经工具创建定时任务
	toolMgr := agenttools.NewToolManager(prompt.NewRegistry(), nil, nil)
	if err := cron.RegisterTools(toolMgr, svc.GetCronManager(botID)); err != nil {
		t.Fatalf("RegisterTools failed: %v", err)
	}

	defs, err := toolMgr.ResolveTools(context.Background(), &agenttools.ToolSessionContext{
		BotID:    botID,
		ChatType: core.ChatPrivate,
		IsSystem: true,
	})
	if err != nil {
		t.Fatalf("ResolveTools failed: %v", err)
	}
	var cronTool *llm.Tool
	for i := range defs {
		if defs[i].Name == "cron" {
			cronTool = &defs[i]
			break
		}
	}
	if cronTool == nil {
		t.Fatal("expected 'cron' tool to be resolved for private session")
	}

	raw, execErr := cronTool.Execute(&llm.ToolExecContext{Context: context.Background()}, map[string]any{
		"action":   "create",
		"name":     "Agent 创建任务",
		"prompt":   "汇总今日待办",
		"schedule": "0 9 * * 1-5",
	})
	if execErr != nil {
		t.Fatal(execErr)
	}
	res := raw.(map[string]any)
	if res["success"] != true {
		t.Fatalf("cron create failed: %v", res)
	}
	jobID := res["job_id"].(string)

	// 调度器持有的同一个 sharedStore 无需重载磁盘即可立即看到新任务
	if _, ok := sharedStore.Get(jobID); !ok {
		t.Fatal("job created by Agent cron tool is invisible to scheduler's shared store")
	}
	if len(svc.GetCronManager(botID).ListJobs()) != 1 {
		t.Fatalf("expected GetCronManager to see 1 job, got %d", len(svc.GetCronManager(botID).ListJobs()))
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "data", "cron", botID+"_cron.json")); err != nil {
		t.Fatalf("expected persisted cron file in temp dir: %v", err)
	}
}

func TestUserCronExecutor_SuppressesAutoReplyAndDeliversCleanOutput(t *testing.T) {
	var deliveredContent string
	var deliveredChannel string
	deliverCalls := 0

	exec := NewUserCronExecutor("bot-1", zap.NewNop().Sugar()).
		WithDeliverer(func(_ context.Context, job *cron.Job, _, content string) error {
			deliverCalls++
			deliveredChannel = job.Channel
			deliveredContent = content
			return nil
		})

	exec.SetRunner(fakeCronRunner{
		fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, []core.Action, error) {
			if !cron.IsCronSession(ctx) {
				t.Error("expected cron session marker in context")
			}
			if env.Message.Source != core.SourceCron {
				t.Errorf("expected Source=%q, got %q", core.SourceCron, env.Message.Source)
			}
			if env.Message.ChatType != core.ChatPrivate {
				t.Errorf("expected ChatType=%q, got %q", core.ChatPrivate, env.Message.ChatType)
			}
			sup, _ := env.Get(core.KVSuppressReply)
			reason, _ := env.Get(core.KVSuppressReplyReason)
			if sup != true || !core.IsHardSuppressReason(reason) {
				t.Errorf("expected hard reply suppression on cron envelope, got suppress=%v reason=%v", sup, reason)
			}

			env.Set("llm.result", &llm.GenerateResult{
				Text:  "<think>internal plan</think><public>早安！今日沪指上涨 1.2%。</public>\n@@REPLY_CONTROL@@{\"send\":true}",
				Usage: llm.Usage{InputTokens: 120, OutputTokens: 35, TotalTokens: 155},
				Steps: []llm.StepResult{
					{ToolCalls: []llm.ToolCall{{ToolName: "web_search"}}},
					{},
				},
			})
			return env, nil, nil
		},
	})

	// 1) 带目标渠道的任务：清洗后投递，且返回统计数据
	res, err := exec.Execute(context.Background(), &cron.Job{
		ID:             "cron-1",
		Name:           "早报",
		Prompt:         "生成今日早报",
		Channel:        "tg-main",
		ChannelType:    "telegram",
		ConversationID: "998877",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if deliverCalls != 1 {
		t.Fatalf("expected 1 delivery call, got %d", deliverCalls)
	}
	if deliveredChannel != "tg-main" {
		t.Errorf("expected channel tg-main, got %q", deliveredChannel)
	}
	if deliveredContent != "早安！今日沪指上涨 1.2%。" {
		t.Errorf("unexpected delivered content: %q", deliveredContent)
	}
	if res.Output != "早安！今日沪指上涨 1.2%。" || res.ToolCalls != 1 || res.Steps != 2 || res.Usage.TotalTokens != 155 {
		t.Errorf("unexpected ExecuteResult: %+v", res)
	}

	// 2) 无目标渠道的纯后台任务：正常完成且不触发投递回调
	bgRes, err := exec.Execute(context.Background(), &cron.Job{
		ID:     "cron-2",
		Name:   "后台巡检",
		Prompt: "执行巡检",
	})
	if err != nil {
		t.Fatalf("background Execute failed: %v", err)
	}
	if deliverCalls != 1 {
		t.Fatalf("expected no extra delivery for background job, got %d calls", deliverCalls)
	}
	if bgRes.Output != "早安！今日沪指上涨 1.2%。" {
		t.Errorf("unexpected background ExecuteResult.Output: %q", bgRes.Output)
	}
}
