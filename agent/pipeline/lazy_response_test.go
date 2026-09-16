package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// fakeLazyJudge 测试用二级裁决器：固定返回给定裁决。
type fakeLazyJudge struct {
	lazy   bool
	reason string
	conf   float64
	entity bool
}

func (f *fakeLazyJudge) Adjudicate(ctx context.Context, req LazyJudgeRequest) (*LazyJudgeResult, error) {
	return &LazyJudgeResult{Lazy: f.lazy, EntityAsserted: f.entity, Confidence: f.conf, Reason: f.reason}, nil
}

// verdictJudge 按回复原文精确返回裁决（用于多级 fixture 闭环测试）。
type verdictJudge struct {
	byReply map[string]bool
}

func (j *verdictJudge) Adjudicate(ctx context.Context, req LazyJudgeRequest) (*LazyJudgeResult, error) {
	lazy, ok := j.byReply[req.Reply]
	if !ok {
		lazy = false
	}
	return &LazyJudgeResult{Lazy: lazy, Reason: "fixture-verdict"}, nil
}

// captureLazySink 测试用落库槽，捕获所有记录。
type captureLazySink struct {
	mu      sync.Mutex
	records []LazyJudgeRecord
}

func (s *captureLazySink) RecordLazyJudge(_ context.Context, rec LazyJudgeRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
}

func (s *captureLazySink) last() LazyJudgeRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		return LazyJudgeRecord{}
	}
	return s.records[len(s.records)-1]
}

// lazyCfgWithJudge 构造带固定裁决器的配置（loop-back 测试用）。
func lazyCfgWithJudge(lazy bool) LazyResponseConfig {
	c := NewLazyResponseConfig()
	c.Judge = &fakeLazyJudge{lazy: lazy, conf: 0.9, reason: "test"}
	return c
}

// TestHasLazyIndicators 验证一级召回（高召回，允许误报）。
// 注意：自两级级联后，一级不再要求"环境实体共现"——纯概念讨论只要命中断言词
// （如"不存在"）就会被一级召回，真正的语义裁决交给二级 LLM。
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
		// 一级高召回：概念讨论含"不存在"现也会被召回（语义裁决交给二级）。
		{"概念讨论含不存在被一级召回", "这个方案在架构上不存在根本缺陷，可以直接上线。", true},
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

// TestHasLazyIndicators_RealLazyStillDetected 回归测试：真偷懒（断言具体环境实体
// 状态且无工具调用）必须被一级召回，不能因词表放宽而漏掉。
func TestHasLazyIndicators_RealLazyStillDetected(t *testing.T) {
	cases := []string{
		"当前环境未安装 git，无法进行版本控制。",
		"/usr/bin/git 不存在，需要手动安装。",
		"系统已安装 Python 3.11。",
		"磁盘空间已满，无法写入文件。",
		"找不到 nginx 配置文件，服务起不来。",
	}
	for _, c := range cases {
		if !hasLazyIndicators(c) {
			t.Errorf("real lazy (env entity + assertion) must be detected: %q", c)
		}
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

// TestLazyResponseMiddleware_DetectsLazy 验证检测到偷懒行为时注入警告（需二级确认）。
func TestLazyResponseMiddleware_DetectsLazy(t *testing.T) {
	mw := LazyResponseMiddleware(lazyCfgWithJudge(true))

	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			const text = "当前环境未安装 git，也没有 apt 包管理器可用。\n\n| 项目 | 状态 |\n| git | 未安装 |"
			genResult := &llm.GenerateResult{
				Text:  text,
				Steps: []llm.StepResult{{Text: text, ToolCalls: nil}},
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
	if !hasLazyWarning(getWarnings(result)) {
		t.Errorf("expected lazy_response hard warning, got warnings: %+v", getWarnings(result))
	}
}

// TestLazyResponseMiddleware_SkipsNormalAnswer 验证正常回答（一级未命中）不触发。
func TestLazyResponseMiddleware_SkipsNormalAnswer(t *testing.T) {
	mw := LazyResponseMiddleware(NewLazyResponseConfig())

	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
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
	mw := LazyResponseMiddleware(lazyCfgWithJudge(true))
	channel := "once-test"

	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			const text = "git 未安装在这个环境中。"
			gr := &llm.GenerateResult{Text: text, Steps: []llm.StepResult{{Text: text}}}
			result := core.NewEnvelope(env.Message)
			result.Set("llm.result", gr)
			return result, nil
		},
	}

	wrapped := mw(dummy)

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
			const text = "经过检查发现 git 未安装。"
			genResult := &llm.GenerateResult{
				Text:  text,
				Steps: []llm.StepResult{{Text: text, ToolCalls: []llm.ToolCall{{ToolName: "exec"}}}},
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
// 二级确认偷懒后注入警告并重算 LLM，当轮即返回修正后的答案。
func TestLazyResponseMiddleware_LoopBackReturnsCorrected(t *testing.T) {
	mw := LazyResponseMiddleware(lazyCfgWithJudge(true))
	calls := 0
	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			calls++
			var gr *llm.GenerateResult
			if calls == 1 {
				const text = "当前环境未安装 git，无可用包管理器。"
				gr = &llm.GenerateResult{Text: text, Steps: []llm.StepResult{{Text: text}}}
			} else {
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
func TestLazyResponseMiddleware_ResetOnToolCall(t *testing.T) {
	mw := LazyResponseMiddleware(lazyCfgWithJudge(true))
	calls := 0
	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			calls++
			var gr *llm.GenerateResult
			switch calls {
			case 1:
				gr = &llm.GenerateResult{Text: "git 未安装。", Steps: []llm.StepResult{{Text: "git 未安装。"}}}
			case 2:
				gr = &llm.GenerateResult{Text: "经 which git 确认未安装。", Steps: []llm.StepResult{{Text: "x", ToolCalls: []llm.ToolCall{{ToolName: "exec"}}}}}
			case 3:
				gr = &llm.GenerateResult{Text: "ok", Steps: []llm.StepResult{{Text: "ok", ToolCalls: []llm.ToolCall{{ToolName: "exec"}}}}}
			case 4:
				gr = &llm.GenerateResult{Text: "apt 未安装。", Steps: []llm.StepResult{{Text: "apt 未安装。"}}}
			default:
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

	if calls != 5 {
		t.Errorf("expected 5 LLM calls (lazy+rerun, toolcall, lazy+rerun), got %d", calls)
	}
}

// TestLazyResponseMiddleware_LoopBackSingleAction 回归测试：验证 loop-back 重算
// 不会把首轮与修正轮两条回复都派发出站（2026-09-16 Telegram 重复回复事故）。
func TestLazyResponseMiddleware_LoopBackSingleAction(t *testing.T) {
	mw := LazyResponseMiddleware(lazyCfgWithJudge(true))
	calls := 0
	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			calls++
			var gr *llm.GenerateResult
			var payload string
			if calls == 1 {
				payload = "当前环境未安装 git，无可用包管理器。"
				gr = &llm.GenerateResult{Text: payload, Steps: []llm.StepResult{{Text: payload}}}
			} else {
				payload = "我执行了 which git，输出为空，确认 git 未安装。建议用 apt 安装。"
				gr = &llm.GenerateResult{
					Text:  payload,
					Steps: []llm.StepResult{{Text: payload, ToolCalls: []llm.ToolCall{{ToolName: "exec"}}}},
				}
			}
			env.Set("llm.result", gr)
			env.AddAction(core.Action{Type: core.ActionReply, Payload: payload})
			return env, nil
		},
	}

	wrapped := mw(dummy)
	env := core.NewEnvelope(core.Message{Channel: "dup-ch", ID: "1"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 LLM calls (1 original + 1 loop-back), got %d", calls)
	}

	acts := result.Actions()
	if len(acts) != 1 {
		t.Fatalf("loop-back must REPLACE not append: expected exactly 1 dispatched action, got %d", len(acts))
	}
	if fmt.Sprint(acts[0].Payload) != "我执行了 which git，输出为空，确认 git 未安装。建议用 apt 安装。" {
		t.Errorf("dispatched action should be the corrected one, got %q", acts[0].Payload)
	}
}

// TestLazyResponseMiddleware_LoopBackFailureKeepsFirstAction 验证 loop-back 重算
// 失败时不丢失首轮回复（至少有一条出站，而非零条）。
func TestLazyResponseMiddleware_LoopBackFailureKeepsFirstAction(t *testing.T) {
	mw := LazyResponseMiddleware(lazyCfgWithJudge(true))
	calls := 0
	dummy := &core.StageFunc{
		StageName: "llm",
		Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
			calls++
			if calls == 1 {
				const text = "当前环境未安装 git，无可用包管理器。"
				gr := &llm.GenerateResult{Text: text, Steps: []llm.StepResult{{Text: text}}}
				env.Set("llm.result", gr)
				env.AddAction(core.Action{Type: core.ActionReply, Payload: text})
				return env, nil
			}
			return env, context.DeadlineExceeded
		},
	}

	wrapped := mw(dummy)
	env := core.NewEnvelope(core.Message{Channel: "dup-fail-ch", ID: "1"})
	result, err := wrapped.Process(context.Background(), env)
	if err != nil {
		t.Fatalf("expected fallback to original result (nil err), got %v", err)
	}
	acts := result.Actions()
	if len(acts) != 1 {
		t.Fatalf("on loop-back failure the first reply must be preserved, got %d actions", len(acts))
	}
	if fmt.Sprint(acts[0].Payload) != "当前环境未安装 git，无可用包管理器。" {
		t.Errorf("preserved action should be the first reply, got %q", acts[0].Payload)
	}
}

// TestLazyResponseCascade_ConceptVetoedRealLazyConfirmed 两级级联闭环回归：
// 用 5 个 fixture（含 2026-09-16 Telegram 重复回复事故首轮 2808 概念讨论），
// 验证一级召回后二级裁决——概念讨论被否决（不 loop-back、不重复回复），
// 真偷懒被确认（loop-back、ClearActions 后仅 1 条修正回复）。
func TestLazyResponseCascade_ConceptVetoedRealLazyConfirmed(t *testing.T) {
	fixtures := []struct {
		name           string
		reply          string
		userQuery      string
		expectLazy     bool
		expectLoopback bool
	}{
		// 事故样本：纯架构讨论，"膨胀在数学上就不存在了"是概念陈述而非环境断言。
		{"accident-2808-concept", "agent 担心的膨胀在数学上就不存在了，这是纯架构讨论。", "你们那个 agent 说的对吗", false, false},
		{"git-not-installed", "git 未安装，需要先安装。", "帮我装个 git", true, true},
		{"python-missing", "python 没有安装，脚本跑不了。", "跑下这个脚本", true, true},
		{"nginx-config", "找不到 nginx 配置文件，服务起不来。", "修一下 nginx", true, true},
		{"disk-full", "磁盘空间已满，无法写入文件。", "为什么写不进去了", true, true},
	}

	for _, fx := range fixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			judge := &verdictJudge{byReply: map[string]bool{fx.reply: fx.expectLazy}}
			sink := &captureLazySink{}
			mw := LazyResponseMiddleware(func() LazyResponseConfig {
				c := NewLazyResponseConfig()
				c.Judge = judge
				c.Sink = sink
				return c
			}())
			calls := 0
			dummy := &core.StageFunc{
				StageName: "llm",
				Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
					calls++
					var text string
					var tools []llm.ToolCall
					if calls == 1 {
						text = fx.reply
					} else {
						text = fx.reply + " (verified via tool)"
						tools = []llm.ToolCall{{ToolName: "exec"}}
					}
					gr := &llm.GenerateResult{Text: text, Steps: []llm.StepResult{{Text: text, ToolCalls: tools}}}
					env.Set("llm.result", gr)
					env.AddAction(core.Action{Type: core.ActionReply, Payload: text})
					return env, nil
				},
			}

			wrapped := mw(dummy)
			env := core.NewEnvelope(core.Message{Channel: "cascade-" + fx.name, ID: "1", BotID: "bot-x"})
			result, err := wrapped.Process(context.Background(), env)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			acts := result.Actions()
			if len(acts) != 1 {
				t.Fatalf("expected exactly 1 dispatched action (no duplicate reply), got %d", len(acts))
			}
			if fx.expectLoopback {
				if calls != 2 {
					t.Errorf("real-lazy must loop-back: expected 2 calls, got %d", calls)
				}
				if !strings.Contains(fmt.Sprint(acts[0].Payload), "(verified via tool)") {
					t.Errorf("loop-back must dispatch corrected reply, got %q", acts[0].Payload)
				}
				if sink.last().FinalAction != "loopback" {
					t.Errorf("sink FinalAction=loopback, got %q", sink.last().FinalAction)
				}
			} else {
				if calls != 1 {
					t.Errorf("concept must be vetoed (no loop-back): expected 1 call, got %d", calls)
				}
				if fmt.Sprint(acts[0].Payload) != fx.reply {
					t.Errorf("veto must keep original reply, got %q", acts[0].Payload)
				}
				if sink.last().FinalAction != "veto" {
					t.Errorf("sink FinalAction=veto, got %q", sink.last().FinalAction)
				}
				if sink.last().Lazy {
					t.Errorf("concept must be judged lazy=false")
				}
			}
		})
	}
}

// TestLazyJudgeSinkRecords 验证落库槽写 JSONL 且可解析回结构。
func TestLazyJudgeSinkRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lazy_judgments.jsonl")
	sink := NewFileLazyJudgeSink(path)
	sink.RecordLazyJudge(context.Background(), LazyJudgeRecord{
		TS:             time.Now(),
		BotID:          "bot-x",
		Channel:        "ch",
		Stage1Matched:  true,
		ReplyLen:       10,
		HadToolCalls:   false,
		Lazy:           true,
		EntityAsserted: true,
		Confidence:     0.9,
		Reason:         "x",
		FinalAction:    "loopback",
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var rec LazyJudgeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !rec.Lazy || rec.FinalAction != "loopback" || rec.Confidence != 0.9 || rec.BotID != "bot-x" {
		t.Errorf("bad record: %+v", rec)
	}
}
