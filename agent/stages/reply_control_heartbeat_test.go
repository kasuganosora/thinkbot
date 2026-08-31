package stages

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// captureSystemProvider 记录最近一次生成的 system prompt，用于断言
// 某段协议说明是否被注入。
type captureSystemProvider struct {
	system string
}

func (p *captureSystemProvider) Name() string { return "capture" }

func (p *captureSystemProvider) DoGenerate(_ context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.system = params.System
	return &llm.GenerateResult{
		Text:         `{"decision":"silent","reason":"test"}`,
		FinishReason: llm.FinishReasonStop,
	}, nil
}

func (p *captureSystemProvider) DoStream(context.Context, llm.GenerateParams) (*llm.StreamResult, error) {
	return nil, errors.New("stream not supported")
}

// TestReplyControlNotInjectedForHeartbeat 是 2026-08-29 心跳决策被静默丢弃的
// prompt 层回归测试。
//
// 根因：心跳走真实编排链路，与出站回复共用 LLMStage。开启 REPLY_CONTROL 时，
// 模型同时收到「心跳决策 JSON」与「@@REPLY_CONTROL@@{"send":…}」两套协议，
// 于是合并成 [{decision…}, {"send":false}] 数组，解析失败 → 降级 silent →
// bot 想记的内部笔记（记忆缓冲超限告警）被整体丢弃。
//
// 心跳的出站由 decision 字段经 ChannelPoster 处理，send 对它无意义，
// 故心跳路径不应注入该协议；普通消息则必须照常注入。
func TestReplyControlNotInjectedForHeartbeat(t *testing.T) {
	const marker = "REPLY CONTROL PROTOCOL"

	run := func(source string) string {
		p := &captureSystemProvider{}
		stage := NewLLMStage("llm", p, LLMConfig{
			Model:               llm.ChatModel("capture"),
			MaxSteps:            1,
			HardMaxSteps:        1,
			RequireReplyControl: true,
		}, noop.NewTracerProvider(), zap.NewNop().Sugar())

		msg := core.Message{
			ID:     "m-replay-control",
			BotID:  "bot-rc",
			Source: source,
			Text:   "",
		}
		if _, err := stage.Process(context.Background(), core.NewEnvelope(msg)); err != nil {
			t.Fatalf("source=%s: process: %v", source, err)
		}
		return p.system
	}

	if got := run(core.SourceHeartbeat); strings.Contains(got, marker) {
		t.Error("心跳路径不应注入 REPLY_CONTROL 协议——两套协议会让模型产出 JSON 数组导致决策被丢弃")
	}

	if got := run("web"); !strings.Contains(got, marker) {
		t.Error("普通消息路径应照常注入 REPLY_CONTROL 协议，实际缺失")
	}
}
