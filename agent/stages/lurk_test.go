package stages

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// lurkStubProvider 与 suppressStubProvider 类似，但额外记录收到的 system prompt，
// 用于验证潜水模式是否注入了「观察者 + soul」prompt。
type lurkStubProvider struct {
	text       string
	called     int
	gotSystem  string
	gotTools   int
}

func (p *lurkStubProvider) Name() string { return "stub" }

func (p *lurkStubProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.called++
	p.gotSystem = params.System
	p.gotTools = len(params.Tools)
	return &llm.GenerateResult{
		Text:         p.text,
		FinishReason: llm.FinishReasonStop,
	}, nil
}

func (p *lurkStubProvider) DoStream(_ context.Context, _ llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("stream not supported in this stub")
}

func noteActions(env *core.Envelope) []core.Action {
	var out []core.Action
	for _, a := range env.Actions() {
		if a.Type == core.ActionNote {
			out = append(out, a)
		}
	}
	return out
}

// TestLLMStage_LurkModeCapturesNoteNotReply 是「潜水要学到东西」的核心回归测试。
//
// 场景：渠道只读（潜水），lurk-detect enricher 已设置 KVLurkMode。
// 期望：LLM 仍被调用（照样思考），但**不产出 ActionReply**（绝不发帖），
// 而是把思考结果作为 ActionNote（内部学习笔记）写入 L0。
func TestLLMStage_LurkModeCapturesNoteNotReply(t *testing.T) {
	p := &lurkStubProvider{text: "这人在做 Go + misskey 集成，偏好 sqlite，值得记。"}
	stage := newSuppressTestStage(p)

	env := core.NewEnvelope(core.Message{
		ID: "lk1", Text: "刚把 misskey 接上了", Source: "misskey-ch", Channel: "misskey:timeline", UserID: "u1",
	})
	env.Set(core.KVLurkMode, true)
	env.Set(core.KVSoulContent, "你是栞娜，直接有用、有自己判断的数字搭档。")

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}

	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("lurk mode must not produce ActionReply, got %d: %+v", len(got), got)
	}
	notes := noteActions(out)
	if len(notes) != 1 {
		t.Fatalf("lurk mode should capture exactly 1 learning note, got %d", len(notes))
	}
	if notes[0].Payload != p.text {
		t.Errorf("unexpected note payload: %v", notes[0].Payload)
	}
	if notes[0].Metadata["category"] != "lurk" {
		t.Errorf("note category should be lurk, got %v", notes[0].Metadata["category"])
	}
	// 潜水学习笔记必须以 bot 全局 scope 落库（Channel 为空），才能跨渠道被召回。
	if notes[0].Channel != "" {
		t.Errorf("lurk learning note should use bot-scope (Channel empty), got %q", notes[0].Channel)
	}
	if p.called != 1 {
		t.Errorf("LLM should be invoked in lurk mode, called=%d", p.called)
	}
}

// TestLLMStage_LurkModeSkipsNone 验证模型输出 [NONE] 时不写噪声笔记。
func TestLLMStage_LurkModeSkipsNone(t *testing.T) {
	p := &lurkStubProvider{text: "[NONE]"}
	stage := newSuppressTestStage(p)

	env := core.NewEnvelope(core.Message{
		ID: "lk2", Text: "早", Source: "misskey-ch", Channel: "misskey:timeline", UserID: "u1",
	})
	env.Set(core.KVLurkMode, true)

	out, err := stage.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("process err: %v", err)
	}
	if got := replyActions(out); len(got) != 0 {
		t.Fatalf("lurk mode must not produce ActionReply, got %+v", got)
	}
	if notes := noteActions(out); len(notes) != 0 {
		t.Fatalf("lurk mode should skip [NONE] note, got %+v", notes)
	}
}

// TestLLMStage_LurkModeUsesSoulObserverPrompt 验证潜水 prompt 结合了 soul + 观察者指令，
// 且工具被清空（潜水观察者不调用工具，杜绝经工具发帖的副作用）。
func TestLLMStage_LurkModeUsesSoulObserverPrompt(t *testing.T) {
	soul := "你是栞娜，直接有用、有自己判断的数字搭档。"
	p := &lurkStubProvider{text: "记一下：对方在用-fedora。"}
	stage := newSuppressTestStage(p)

	env := core.NewEnvelope(core.Message{
		ID: "lk3", Text: "x", Source: "misskey-ch", Channel: "misskey:timeline", UserID: "u1",
	})
	env.Set(core.KVLurkMode, true)
	env.Set(core.KVSoulContent, soul)

	if _, err := stage.Process(context.Background(), env); err != nil {
		t.Fatalf("process err: %v", err)
	}

	if !strings.Contains(p.gotSystem, soul) {
		t.Errorf("lurk system prompt must embed soul content, got: %q", p.gotSystem)
	}
	if !strings.Contains(p.gotSystem, "OBSERVER MODE") {
		t.Errorf("lurk system prompt must contain observer instruction, got: %q", p.gotSystem)
	}
	if p.gotTools != 0 {
		t.Errorf("lurk mode must disable tools, got %d tools", p.gotTools)
	}
}
