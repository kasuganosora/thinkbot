package stages

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// slowTurnProvider：第 1 步调用 task 工具（工具一直阻塞到 ctx 到期），第 2 步的 LLM
// 调用也阻塞到 ctx 到期 —— 复现 09-27「task 阻塞 14 分钟 + 15 分钟硬超时」的回合。
type slowTurnProvider struct{ calls int }

func (p *slowTurnProvider) Name() string { return "slow" }
func (p *slowTurnProvider) DoGenerate(ctx context.Context, _ llm.GenerateParams) (*llm.GenerateResult, error) {
	p.calls++
	if p.calls == 1 {
		return &llm.GenerateResult{
			FinishReason: llm.FinishReasonToolCalls,
			ToolCalls:    []llm.ToolCall{{ToolCallID: "c1", ToolName: "task", Input: map[string]any{}}},
			Usage:        llm.Usage{InputTokens: 1200, OutputTokens: 80, TotalTokens: 1280},
		}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *slowTurnProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("not implemented")
}

type captureUsage struct {
	mu      sync.Mutex
	metrics []llm.UsageMetric
}

func (c *captureUsage) RecordUsage(_ context.Context, m llm.UsageMetric) {
	c.mu.Lock()
	c.metrics = append(c.metrics, m)
	c.mu.Unlock()
}

func runTimedOutTurn(t *testing.T, msg core.Message, replyControl bool) (*core.Envelope, error, *captureUsage) {
	t.Helper()
	task := llm.Tool{
		Name: "task", Description: "task", Parameters: map[string]any{"type": "object"},
		Execute: func(ctx *llm.ToolExecContext, _ any) (any, error) {
			<-ctx.Done()
			return map[string]any{"timedOut": true}, nil
		},
	}
	usage := &captureUsage{}
	s := NewLLMStage("llm", &slowTurnProvider{}, LLMConfig{
		Model: llm.ChatModel("m"), MaxSteps: 5, HardTimeout: 150 * time.Millisecond,
		ToolResolver: sourceToolResolver{tg: []llm.Tool{task}}, UsageRecorder: usage,
		RequireReplyControl: replyControl,
	}, nil, zap.NewNop().Sugar())
	out, err := s.Process(context.Background(), core.NewEnvelope(msg))
	return out, err, usage
}

func tgMsg(chatType string, mentioned bool) core.Message {
	return core.Message{
		ID: "tg-1", BotID: "bot-1", Source: "telegram", Channel: "76017910", UserID: "76017910",
		ChatType: chatType, Mentioned: mentioned, Text: "run the long job",
	}
}

func TestLLMStage_HardTimeoutRepliesAndRecordsUsage(t *testing.T) {
	for _, rc := range []bool{false, true} {
		t.Run(fmt.Sprintf("reply_control=%v", rc), func(t *testing.T) {
			out, err, usage := runTimedOutTurn(t, tgMsg(core.ChatPrivate, false), rc)
			if err != nil {
				t.Fatalf("a timed-out private turn must still reply, got error %v", err)
			}
			replies := replyActions(out)
			if len(replies) != 1 {
				t.Fatalf("want 1 reply, got %+v", out.Actions())
			}
			text := fmt.Sprint(replies[0].Payload)
			if !strings.Contains(text, "时限") || !strings.Contains(text, "后台任务仍在继续运行") {
				t.Fatalf("timeout reply should explain the timeout and the background task, got %q", text)
			}
			if strings.Contains(text, replyControlDelimiter) {
				t.Fatalf("reply-control block leaked: %q", text)
			}
			// 超时回复只陈述事实，不得要求用户回「继续」/「拆小任务」续跑。
			for _, banned := range []string{"回复「继续」", "拆小"} {
				if strings.Contains(text, banned) {
					t.Fatalf("timeout reply must not ask the user to continue, got %q", text)
				}
			}
			if len(usage.metrics) != 1 || usage.metrics[0].Usage.InputTokens != 1200 || usage.metrics[0].Usage.OutputTokens != 80 {
				t.Fatalf("usage of the completed step must be recorded once, got %+v", usage.metrics)
			}
		})
	}
}

func TestLLMStage_HardTimeoutInUnaddressedGroupRecordsUsageOnly(t *testing.T) {
	out, err, usage := runTimedOutTurn(t, tgMsg(core.ChatGroup, false), false)
	if err == nil {
		t.Fatal("unaddressed group turn should still fail with the hard-timeout error")
	}
	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("no reply expected in an unaddressed group, got %+v", got)
	}
	if len(usage.metrics) != 1 || usage.metrics[0].Usage.TotalTokens != 1280 {
		t.Fatalf("usage must be recorded even without a reply, got %+v", usage.metrics)
	}
}

func TestLLMStage_HardTimeoutMentionedInGroupReplies(t *testing.T) {
	out, err, _ := runTimedOutTurn(t, tgMsg(core.ChatGroup, true), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(replyActions(out)) != 1 {
		t.Fatalf("a direct mention in a group should get the timeout reply, got %+v", out.Actions())
	}
}
