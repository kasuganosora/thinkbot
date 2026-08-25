package stages

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// suppressStubProvider 返回固定文本，用于验证 LLMStage 的出站处理。
type suppressStubProvider struct {
	text   string
	called int
}

func (p *suppressStubProvider) Name() string { return "stub" }

func (p *suppressStubProvider) DoGenerate(_ context.Context, _ llm.GenerateParams) (*llm.GenerateResult, error) {
	p.called++
	return &llm.GenerateResult{
		Text:         p.text,
		FinishReason: llm.FinishReasonStop,
	}, nil
}

func (p *suppressStubProvider) DoStream(_ context.Context, _ llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("stream not supported in this stub")
}

func newSuppressTestStage(p llm.Provider) *LLMStage {
	return NewLLMStage("llm", p, LLMConfig{
		MessageBuilder: func(msg core.Message) []llm.Message {
			return []llm.Message{llm.UserMessage(msg.Text)}
		},
	}, nil, nil)
}

func replyActions(env *core.Envelope) []core.Action {
	var out []core.Action
	for _, a := range env.Actions() {
		if a.Type == core.ActionReply {
			out = append(out, a)
		}
	}
	return out
}

// TestLLMStage_SuppressedReplyNotSent 是「心里话不外泄」的核心回归测试。
//
// 场景：上游 engagement 判定「此刻不该说话」并设置 KVSuppressReply。
// 期望：LLM 照样被调用（Bot 仍在思考、结果供记忆使用），但**不产出 ActionReply**。
func TestLLMStage_SuppressedReplyNotSent(t *testing.T) {
	p := &suppressStubProvider{text: "我觉得这群人真吵，但我不该说出来"}
	stage := newSuppressTestStage(p)

	env := core.NewEnvelope(core.Message{
		ID: "m1", Text: "闲聊", Source: "misskey-ch", Channel: "room-1", UserID: "u1",
	})
	env.Set(core.KVSuppressReply, true)
	env.Set(core.KVSuppressReplyReason, "engagement_declined")

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}

	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("suppressed turn must not produce ActionReply, got %d: %+v", len(got), got)
	}
	// LLM 仍应被调用：Bot「照样听、照样想」
	if p.called != 1 {
		t.Errorf("LLM should still be invoked when reply is suppressed, called=%d", p.called)
	}
	// 结果仍写入 KV，供记忆等下游 Stage 使用
	if _, ok := out.Get("llm.result"); !ok {
		t.Error("llm.result should still be stored for downstream memory stages")
	}
}

// TestLLMStage_NotSuppressedSendsReply 验证未抑制时正常发送（防反向故障）。
func TestLLMStage_NotSuppressedSendsReply(t *testing.T) {
	p := &suppressStubProvider{text: "你好，有什么可以帮你？"}
	stage := newSuppressTestStage(p)

	env := core.NewEnvelope(core.Message{
		ID: "m2", Text: "在吗", Source: "misskey-ch", Channel: "room-1", UserID: "u1",
	})

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}
	got := replyActions(out)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 reply action, got %d", len(got))
	}
	if got[0].Payload != "你好，有什么可以帮你？" {
		t.Errorf("unexpected payload: %v", got[0].Payload)
	}
}

// TestLLMStage_StripsThinkTagsBeforeSending 验证出站前清洗 <think> 标签。
//
// 背景：DeepSeek-R1 / GLM / QwQ 等模型把推理过程内联成 <think>...</think>
// 写在正文里（而非结构化的Reasoning 字段）。项目原先只在**记忆写入侧**清洗，
// 出站链路零调用 —— 这些内心独白会被原样发给用户。
func TestLLMStage_StripsThinkTagsBeforeSending(t *testing.T) {
	p := &suppressStubProvider{
		text: "<think>用户可能在试探我，先别暴露太多。这人很烦。</think>你好，我在。",
	}
	stage := newSuppressTestStage(p)

	env := core.NewEnvelope(core.Message{
		ID: "m3", Text: "在吗", Source: "misskey-ch", Channel: "room-1", UserID: "u1",
	})
	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}

	got := replyActions(out)
	if len(got) != 1 {
		t.Fatalf("expected 1 reply action, got %d", len(got))
	}
	payload, _ := got[0].Payload.(string)
	if strings.Contains(payload, "<think>") || strings.Contains(payload, "这人很烦") {
		t.Fatalf("thinking content leaked into reply: %q", payload)
	}
	if !strings.Contains(payload, "你好，我在。") {
		t.Errorf("actual reply content lost after stripping: %q", payload)
	}
}

// TestLLMStage_EmptyAfterStripNotSent 验证「整段都是思考」时不发空消息。
func TestLLMStage_EmptyAfterStripNotSent(t *testing.T) {
	p := &suppressStubProvider{text: "<think>算了，这条不用回。</think>   "}
	stage := newSuppressTestStage(p)

	env := core.NewEnvelope(core.Message{
		ID: "m4", Text: "闲聊", Source: "misskey-ch", Channel: "room-1", UserID: "u1",
	})
	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}
	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("must not send empty/thinking-only reply, got %+v", got)
	}
}

// newSuppressTestStageRC 构造可配置 RequireReplyControl 的测试 Stage。
func newSuppressTestStageRC(p llm.Provider, requireRC bool) *LLMStage {
	return NewLLMStage("llm", p, LLMConfig{
		MessageBuilder: func(msg core.Message) []llm.Message {
			return []llm.Message{llm.UserMessage(msg.Text)}
		},
		RequireReplyControl: requireRC,
	}, nil, nil)
}

// TestLLMStage_ModelSendTrueOverridesRhythmSuppress 验证：REPLY_CONTROL 开启时，
// 模型显式 send:true 且含公开内容，可覆盖上游节奏门（rhythm_speak_tendency）的抑制。
// 这是 2026-08-25 日志审计发现的回归：群内被问技术问题时模型已用 <public>+send:true
// 给出答案，却被节奏门整条吞掉。
func TestLLMStage_ModelSendTrueOverridesRhythmSuppress(t *testing.T) {
	p := &suppressStubProvider{
		text: "<public>rssCloud 挺有年代感，现在更推荐 WebSub。</public>@@REPLY_CONTROL@@{\"send\": true}",
	}
	stage := newSuppressTestStageRC(p, true)

	env := core.NewEnvelope(core.Message{
		ID: "m5", Text: "RSS 实时推送现在用什么？", Source: "misskey-ch", Channel: "room-1", UserID: "u1",
	})
	env.Set(core.KVSuppressReply, true)
	env.Set(core.KVSuppressReplyReason, "rhythm_speak_tendency")

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}
	got := replyActions(out)
	if len(got) != 1 {
		t.Fatalf("model send:true must override rhythm suppress, got %d replies: %+v", len(got), got)
	}
	payload, _ := got[0].Payload.(string)
	if !strings.Contains(payload, "WebSub") {
		t.Errorf("public content lost after override, payload=%q", payload)
	}
}

// TestLLMStage_ModelSendFalseStillSuppressed 验证：模型自己 send:false 时，
// 即便上游已抑制，结果仍不出站（覆盖分支不改变模型的否决权）。
func TestLLMStage_ModelSendFalseStillSuppressed(t *testing.T) {
	p := &suppressStubProvider{
		text: "<internal>这条是引流广告，不互动。</internal>@@REPLY_CONTROL@@{\"send\": false}",
	}
	stage := newSuppressTestStageRC(p, true)

	env := core.NewEnvelope(core.Message{
		ID: "m6", Text: "来看我家新品", Source: "misskey-ch", Channel: "room-1", UserID: "u1",
	})
	env.Set(core.KVSuppressReply, true)
	env.Set(core.KVSuppressReplyReason, "rhythm_speak_tendency")

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}
	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("model send:false must stay suppressed, got %+v", got)
	}
}

// TestLLMStage_NoControlBlockFailClosed 验证：REPLY_CONTROL 开启但模型漏掉控制块时，
// 仍 fail-closed 不出站（覆盖分支仅在模型显式 send:true 时生效）。
func TestLLMStage_NoControlBlockFailClosed(t *testing.T) {
	p := &suppressStubProvider{text: "我随便说点什么"}
	stage := newSuppressTestStageRC(p, true)

	env := core.NewEnvelope(core.Message{
		ID: "m7", Text: "在吗", Source: "misskey-ch", Channel: "room-1", UserID: "u1",
	})
	env.Set(core.KVSuppressReply, true)
	env.Set(core.KVSuppressReplyReason, "engagement_declined")

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}
	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("missing control block must fail-closed, got %+v", got)
	}
}

// TestLLMStage_RCGateOffRhythmWins 验证：REPLY_CONTROL 未开启时，
// 上游节奏门仍绝对优先（即便模型文本里出现 send:true 字样也不解析）。
func TestLLMStage_RCGateOffRhythmWins(t *testing.T) {
	p := &suppressStubProvider{
		text: "<public>答案</public>@@REPLY_CONTROL@@{\"send\": true}",
	}
	stage := newSuppressTestStageRC(p, false)

	env := core.NewEnvelope(core.Message{
		ID: "m8", Text: "问题", Source: "misskey-ch", Channel: "room-1", UserID: "u1",
	})
	env.Set(core.KVSuppressReply, true)
	env.Set(core.KVSuppressReplyReason, "rhythm_speak_tendency")

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}
	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("RC off: rhythm gate must win, got %+v", got)
	}
}
