package pipeline

import (
	"context"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// getWarnings 从 Envelope 中提取延迟警告列表。
func getWarnings(env *core.Envelope) []core.Warning {
	v, ok := env.Get(core.WarningsKey)
	if !ok {
		return nil
	}
	warnings, ok := v.([]core.Warning)
	if !ok {
		return nil
	}
	return warnings
}

// hasLazyWarning 判断 warnings 中是否包含 lazy_response 的硬警告。
func hasLazyWarning(warnings []core.Warning) bool {
	for _, w := range warnings {
		if w.Source == "lazy_response" && w.Level == core.WarningLevelHard {
			return true
		}
	}
	return false
}

// TestHasLazyIndicators 验证偷懒模式检测。
func TestHasLazyIndicators(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected bool
	}{
		{"空字符串", "", false},
		{"太短", "没有", false},
		{"普通回答", "好的，我来帮你处理这个问题。", false},
		{"未安装声明", "当前环境未安装 git，无法进行版本控制。", true},
		{"不存在声明", "/usr/bin/git 不存在，需要手动安装。", true},
		{"编造表格", "| 项目 | 状态 |\n| --- | --- |\n| git | 未安装 |", true},
		{"已安装声明", "系统已安装 Python 3.11。", true},
		{"尝试结果编造", "尝试执行 which git 结果显示命令不存在。", true},
		{"正常工具调用结果（无模式命中）", "文件内容如下...", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hasLazyIndicators(tc.text)
			if got != tc.expected {
				t.Errorf("hasLazyIndicators(%q) = %v, want %v", tc.text, got, tc.expected)
			}
		})
	}
}

// TestHadToolCalls 验证工具调用检测。
func TestHadToolCalls(t *testing.T) {
	t.Run("nil result", func(t *testing.T) {
		if hadToolCalls(nil) {
			t.Error("nil should return false")
		}
	})
	t.Run("empty steps", func(t *testing.T) {
		if hadToolCalls(&llm.GenerateResult{Steps: []llm.StepResult{}}) {
			t.Error("empty steps should return false")
		}
	})
	t.Run("has tool calls", func(t *testing.T) {
		result := &llm.GenerateResult{
			Steps: []llm.StepResult{
				{ToolCalls: []llm.ToolCall{{ToolName: "exec"}}},
			},
		}
		if !hadToolCalls(result) {
			t.Error("should detect tool calls")
		}
	})
}

// TestLazyResponseMiddleware_Disabled 验证禁用配置时为 no-op。
func TestLazyResponseMiddleware_Disabled(t *testing.T) {
	mw := LazyResponseMiddleware(LazyResponseConfig{Enabled: false})
	dummy := &core.StageFunc{
		StageName: "dummy",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			return env, nil
		},
	}
	wrapped := mw(dummy)
	if wrapped.Name() != dummy.Name() {
		t.Error("disabled middleware should return original stage")
	}
}

// TestLazyResponseMiddleware_DetectsLazy 验证检测到偷懒行为时注入警告。
func TestLazyResponseMiddleware_DetectsLazy(t *testing.T) {
	mw := LazyResponseMiddleware(NewLazyResponseConfig())

	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			// 模拟一个"无工具调用但有环境状态断言"的结果
			const text = "当前环境未安装 git，也没有 apt 包管理器可用。\n\n| 项目 | 状态 |\n| git | 未安装 |"
			genResult := &llm.GenerateResult{
				Text: text,
				Steps: []llm.StepResult{
					{Text: text, ToolCalls: nil}, // 无 tool calls
				},
			}
			result := core.NewEnvelope(env.Message)
			result.Set("llm.result", genResult)
			return result, nil
		},
	}

	wrapped := mw(dummy)
	env := core.NewEnvelope(core.Message{Channel: "test-ch", ID: "msg-1"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 应该注入了硬警告
	if !hasLazyWarning(getWarnings(result)) {
		t.Errorf("expected lazy_response hard warning, got warnings: %+v", getWarnings(result))
	}
}

// TestLazyResponseMiddleware_SkipsNormalAnswer 验证正常回答不触发警告。
func TestLazyResponseMiddleware_SkipsNormalAnswer(t *testing.T) {
	mw := LazyResponseMiddleware(NewLazyResponseConfig())

	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			// 正常知识性回答，无工具调用但无偷懒模式
			const text = "RAG 是检索增强生成技术，结合了信息检索和语言生成。"
			genResult := &llm.GenerateResult{
				Text:  text,
				Steps: []llm.StepResult{{Text: text}},
			}
			result := core.NewEnvelope(env.Message)
			result.Set("llm.result", genResult)
			return result, nil
		},
	}

	wrapped := mw(dummy)
	env := core.NewEnvelope(core.Message{Channel: "test-ch2", ID: "msg-2"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if hasLazyWarning(getWarnings(result)) {
		t.Error("normal answer should not trigger lazy_response warning")
	}
}

// TestLazyResponseMiddleware_OncePerChannel 验证同一 channel 只警告一次。
func TestLazyResponseMiddleware_OncePerChannel(t *testing.T) {
	mw := LazyResponseMiddleware(NewLazyResponseConfig())
	channel := "once-test"

	buildLazyEnvelope := func(msgID string) *core.Envelope {
		const text = "git 未安装在这个环境中。"
		gr := &llm.GenerateResult{
			Text:  text,
			Steps: []llm.StepResult{{Text: text}},
		}
		env := core.NewEnvelope(core.Message{Channel: channel, ID: msgID})
		env.Set("llm.result", gr)
		return env
	}

	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			return buildLazyEnvelope(env.Message.ID), nil
		},
	}

	wrapped := mw(dummy)

	// 第一次 → 应该警告
	r1, _ := wrapped.Process(context.Background(), core.NewEnvelope(core.Message{Channel: channel, ID: "1"}))
	count1 := 0
	for _, w := range getWarnings(r1) {
		if w.Source == "lazy_response" {
			count1++
		}
	}
	if count1 != 1 {
		t.Errorf("first call expected 1 warning, got %d", count1)
	}

	// 第二次 → 不应再警告
	r2, _ := wrapped.Process(context.Background(), core.NewEnvelope(core.Message{Channel: channel, ID: "2"}))
	count2 := 0
	for _, w := range getWarnings(r2) {
		if w.Source == "lazy_response" {
			count2++
		}
	}
	if count2 != 0 {
		t.Errorf("second call expected 0 warnings (once per channel), got %d", count2)
	}
}

// TestLazyResponseMiddleware_SkipsWithToolCalls 验证有工具调用时不触发。
func TestLazyResponseMiddleware_SkipsWithToolCalls(t *testing.T) {
	mw := LazyResponseMiddleware(NewLazyResponseConfig())

	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			// 有工具调用，即使文本包含"未安装"也不应触发
			const text = "经过检查发现 git 未安装。"
			genResult := &llm.GenerateResult{
				Text: text,
				Steps: []llm.StepResult{
					{
						Text:      text,
						ToolCalls: []llm.ToolCall{{ToolName: "exec"}},
					},
				},
			}
			result := core.NewEnvelope(env.Message)
			result.Set("llm.result", genResult)
			return result, nil
		},
	}

	wrapped := mw(dummy)
	env := core.NewEnvelope(core.Message{Channel: "ch3", ID: "msg-3"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if hasLazyWarning(getWarnings(result)) {
		t.Error("should not warn when tool calls are present")
	}
}

// TestLazyResponseMiddleware_LoopBackReturnsCorrected 验证同轮 loop-back：
// 首次产出无依据的偷懒答案时，注入警告并重算 LLM，当轮即返回修正后的答案。
func TestLazyResponseMiddleware_LoopBackReturnsCorrected(t *testing.T) {
	mw := LazyResponseMiddleware(NewLazyResponseConfig())
	calls := 0
	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			calls++
			var gr *llm.GenerateResult
			if calls == 1 {
				// 首次：无工具调用的偷懒答案
				const text = "当前环境未安装 git，无可用包管理器。"
				gr = &llm.GenerateResult{Text: text, Steps: []llm.StepResult{{Text: text}}}
			} else {
				// loop-back 重算：模型这次调了工具，给出有依据的答案
				const text = "我执行了 which git，输出为空，确认 git 未安装。建议用 apt 安装。"
				gr = &llm.GenerateResult{
					Text:  text,
					Steps: []llm.StepResult{{Text: text, ToolCalls: []llm.ToolCall{{ToolName: "exec"}}}},
				}
			}
			result := core.NewEnvelope(env.Message)
			result.Set("llm.result", gr)
			return result, nil
		},
	}

	wrapped := mw(dummy)
	env := core.NewEnvelope(core.Message{Channel: "lb-ch", ID: "1"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 LLM calls (1 original + 1 loop-back), got %d", calls)
	}
	v, ok := result.Get("llm.result")
	if !ok {
		t.Fatal("expected llm.result on returned envelope")
	}
	gr := v.(*llm.GenerateResult)
	if !hadToolCalls(gr) {
		t.Errorf("loop-back should return the corrected answer (with tool call), got: %q", gr.Text)
	}
}

// TestLazyResponseMiddleware_ResetOnToolCall 验证：模型成功调工具后，
// 同 channel 的警告标记被复位，后续再偷懒仍会被拦截（不会永久静默）。
// 调用序列：lazy(1+重算2) → 工具调用(3,复位) → lazy 再次(4+重算5)。
func TestLazyResponseMiddleware_ResetOnToolCall(t *testing.T) {
	mw := LazyResponseMiddleware(NewLazyResponseConfig())
	calls := 0
	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			calls++
			var gr *llm.GenerateResult
			switch calls {
			case 1: // 首次：偷懒
				gr = &llm.GenerateResult{Text: "git 未安装。", Steps: []llm.StepResult{{Text: "git 未安装。"}}}
			case 2: // 首次的重算：已纠正（带工具调用）
				gr = &llm.GenerateResult{Text: "经 which git 确认未安装。", Steps: []llm.StepResult{{Text: "x", ToolCalls: []llm.ToolCall{{ToolName: "exec"}}}}}
			case 3: // 工具调用结果 → 复位警告标记
				gr = &llm.GenerateResult{Text: "ok", Steps: []llm.StepResult{{Text: "ok", ToolCalls: []llm.ToolCall{{ToolName: "exec"}}}}}
			case 4: // 再次偷懒
				gr = &llm.GenerateResult{Text: "apt 未安装。", Steps: []llm.StepResult{{Text: "apt 未安装。"}}}
			default: // calls==5：再次的重算
				gr = &llm.GenerateResult{Text: "经 which apt 确认。", Steps: []llm.StepResult{{Text: "x", ToolCalls: []llm.ToolCall{{ToolName: "exec"}}}}}
			}
			result := core.NewEnvelope(env.Message)
			result.Set("llm.result", gr)
			return result, nil
		},
	}

	wrapped := mw(dummy)
	_, _ = wrapped.Process(context.Background(), core.NewEnvelope(core.Message{Channel: "reset-ch", ID: "1"})) // 1,2
	_, _ = wrapped.Process(context.Background(), core.NewEnvelope(core.Message{Channel: "reset-ch", ID: "2"})) // 3 (复位)
	_, _ = wrapped.Process(context.Background(), core.NewEnvelope(core.Message{Channel: "reset-ch", ID: "3"})) // 4,5

	// 若复位失效：call 3 不会改变 warned=true，call 4 会因已警告而跳过 loop-back → calls==4
	// 复位生效：call 4 重新触发 loop-back → calls==5
	if calls != 5 {
		t.Errorf("expected 5 LLM calls (lazy+rerun, toolcall, lazy+rerun), got %d", calls)
	}
}
