package workflow

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/subagent"
)

// scriptedProvider 按调用顺序返回预设文本，用于验证「主审查没给判定 →
// 追加一次收尾调用把判定要回来」这条链路。
type scriptedProvider struct {
	replies []string
	calls   int32
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	return &llm.GenerateResult{Text: p.next(), FinishReason: llm.FinishReasonStop}, nil
}

func (p *scriptedProvider) next() string {
	n := int(atomic.AddInt32(&p.calls, 1))
	if n <= len(p.replies) {
		return p.replies[n-1]
	}
	return p.replies[len(p.replies)-1]
}

func (p *scriptedProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	text := p.next()
	ch := make(chan llm.StreamPart, 2)
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
			return
		case ch <- &llm.TextDeltaPart{Text: text}:
		}
		select {
		case <-ctx.Done():
		case ch <- &llm.FinishPart{FinishReason: llm.FinishReasonStop}:
		}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}

func (p *scriptedProvider) callCount() int { return int(atomic.LoadInt32(&p.calls)) }

// midAnalysisNoVerdict 复刻线上真实形态：审查跑了多步工具、通篇是中途叙述，
// 结尾没有约定的判定 JSON，也没有显式判定行。
const midAnalysisNoVerdict = "我将通过检查实际代码状态、构建和测试，来验证审查输出中的说明。" +
	"构建 (Build)、检查 (vet) 和测试 (test) 全部通过。" +
	"现在，让我通过检查实际文件来验证每一项声称的修复..."

func newTestExecutor(prov llm.Provider) (*Executor, *subagent.SubAgentManager) {
	saMgr := subagent.NewSubAgentManager(prov, "test-model")
	return NewExecutor(saMgr, nil, nil), saMgr
}

// TestReview_VerdictOnlyFollowUpRecoversVerdict 是本次修复的核心回归：
// 主审查没吐 JSON 时，Review 必须**追加一次收尾调用**把判定要回来，
// 而不是直接采用 heuristic 的保守 FAIL。
//
// 线上实测90%（19/21）的 review 掉进 heuristic 路径 → 节点被无谓重跑到迭代耗尽，
// 工作流 88 分钟只完成 3/17 个节点。这条测试守住修复不被回退。
func TestReview_VerdictOnlyFollowUpRecoversVerdict(t *testing.T) {
	prov := &scriptedProvider{replies: []string{
		midAnalysisNoVerdict, // 1) 主审查：无判定
		`{"passed": true, "notes": "构建与测试均通过"}`, // 2) 收尾调用：给出判定
	}}
	e, saMgr := newTestExecutor(prov)
	defer saMgr.CloseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := e.Review(ctx, &DAGNode{ID: "n1", Name: "审查 controllers/"}, "产物内容")
	if err != nil {
		t.Fatalf("Review 失败: %v", err)
	}
	if prov.callCount() < 2 {
		t.Fatalf("期望触发收尾调用（>=2 次 LLM 调用），实际 %d 次", prov.callCount())
	}
	if !res.Passed {
		t.Errorf("收尾调用已明确判PASS，Review 却返回 Passed=false —— "+
			"heuristic 的保守 FAIL 没有被覆盖（source=%s）", res.Source)
	}
	if res.Source != ReviewSourceJSON {
		t.Errorf("收尾调用返回合法 JSON，Source 应为 %q，实际 %q",
			ReviewSourceJSON, res.Source)
	}
}

// TestReview_VerdictOnlyFollowUpRespectsFail 验证收尾调用**不是**「一律放行」：
// 模型明确判 FAIL 时必须如实返回，且带上 feedback 供迭代使用。
//
// 这条与上一条成对存在：只有上一条会让人把修复写成「拿不到判定就 pass」，
// 那是 eac45b5 已经踩过的坑（默认 pass 导致 workflow 假完成）。
func TestReview_VerdictOnlyFollowUpRespectsFail(t *testing.T) {
	prov := &scriptedProvider{replies: []string{
		midAnalysisNoVerdict,
		`{"passed": false, "feedback": "controllers/user.go 存在 SQL 注入", "notes": ""}`,
	}}
	e, saMgr := newTestExecutor(prov)
	defer saMgr.CloseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := e.Review(ctx, &DAGNode{ID: "n2", Name: "审查 models/"}, "产物内容")
	if err != nil {
		t.Fatalf("Review 失败: %v", err)
	}
	if res.Passed {
		t.Error("模型明确判 FAIL，Review 却放行了—— 绝不能退化成「默认 pass」")
	}
	if !strings.Contains(res.Feedback, "SQL 注入") {
		t.Errorf("FAIL 判定必须携带 feedback 供迭代使用，实际 feedback=%q", res.Feedback)
	}
}

// TestReview_NoFollowUpWhenVerdictAlreadyTrustworthy 验证收尾调用只在需要时触发：
// 主审查已给出合法 JSON 时，绝不能多花一次 LLM 调用。
func TestReview_NoFollowUpWhenVerdictAlreadyTrustworthy(t *testing.T) {
	prov := &scriptedProvider{replies: []string{
		"分析过程若干。\n{\"passed\": true, \"notes\": \"\"}",
	}}
	e, saMgr := newTestExecutor(prov)
	defer saMgr.CloseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := e.Review(ctx, &DAGNode{ID: "n3", Name: "审查 cache/"}, "产物内容")
	if err != nil {
		t.Fatalf("Review 失败: %v", err)
	}
	if !res.Passed || res.Source != ReviewSourceJSON {
		t.Errorf("期望直接采用 JSON 判定，实际 passed=%v source=%q", res.Passed, res.Source)
	}
	if got := prov.callCount(); got != 1 {
		t.Errorf("主审查判定已可信，不应触发收尾调用；期望 1 次 LLM 调用，实际 %d 次", got)
	}
}

// TestReview_FollowUpFailureKeepsHeuristic 验证收尾调用也拿不到判定时，
// 保持 heuristic 兜底结论（而非报错、也不擅自改写成pass）。
//
// 注意：heuristic 并非「一律 FAIL」——它是词频比较，含「全部通过」这类信号时会判 PASS。
// 所以这里断言的是**与 heuristic 的结论一致**（契约），而不是硬编码 true/false。
func TestReview_FollowUpFailureKeepsHeuristic(t *testing.T) {
	prov := &scriptedProvider{replies: []string{
		midAnalysisNoVerdict, // 主审查无判定
		"我还需要更多信息才能判断。",      // 收尾调用同样没给判定
	}}
	e, saMgr := newTestExecutor(prov)
	defer saMgr.CloseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := e.Review(ctx, &DAGNode{ID: "n4", Name: "审查 auth/"}, "产物内容")
	if err != nil {
		t.Fatalf("收尾调用失败不应让 Review 整体报错: %v", err)
	}
	if res.Source != ReviewSourceHeuristic {
		t.Errorf("两次都拿不到判定时应保留 heuristic 来源，实际 %q", res.Source)
	}
	wantPassed, _ := heuristicReviewVerdict(midAnalysisNoVerdict)
	if res.Passed != wantPassed {
		t.Errorf("收尾失败时必须原样保留 heuristic 结论：期望 passed=%v，实际 %v",
			wantPassed, res.Passed)
	}
}

// TestReview_FollowUpFailureOnAmbiguousTextStaysConservative 验证在**没有任何明确信号**
// 的文本上，两次都拿不到判定时仍保守判 FAIL，绝不静默放行未审查产物。
//
// 这条守的是 eac45b5 的教训：旧实现解析失败默认 Passed=true，导致 fail→重跑自环
// 永不触发、workflow 假完成。
func TestReview_FollowUpFailureOnAmbiguousTextStaysConservative(t *testing.T) {
	ambiguous := "我先看一下这个目录的结构，然后再决定下一步怎么做。"
	prov := &scriptedProvider{replies: []string{
		ambiguous,
		"我还需要更多信息才能判断。",
	}}
	e, saMgr := newTestExecutor(prov)
	defer saMgr.CloseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := e.Review(ctx, &DAGNode{ID: "n6", Name: "审查 upload/"}, "产物内容")
	if err != nil {
		t.Fatalf("Review 不应报错: %v", err)
	}
	if res.Passed {
		t.Error("文本无任何明确判定信号时必须保守判 FAIL，绝不能静默放行（见 eac45b5）")
	}
}

// TestReviewRawTail_KeepsTailNotHead 守住诊断能力：
// 判定 JSON 在**末尾**，日志必须截末尾。历史上这里用 Truncate(raw,200)（截开头），
// 日志里全是「我来核实一下…」的开场白→ 2026-08-06 排查时因此误判了根因。
func TestReviewRawTail_KeepsTailNotHead(t *testing.T) {
	head := strings.Repeat("我来核实一下这些关键修复。", 60)
	tail := `{"passed": true, "notes": "ok"}`
	raw := head + "\n" + tail

	got := reviewRawTail(raw)
	if !strings.Contains(got, "passed") {
		t.Errorf("reviewRawTail 必须保留末尾的判定 JSON，实际=%q", got)
	}
	if strings.HasPrefix(got, "我来核实") {
		t.Error("reviewRawTail 截取了开头 —— 判定证据在末尾，别再改回 head")
	}
}

// TestReviewVerdictOnly_SkipsToolsToAvoidSameFailureMode 验证收尾调用必须无工具。
//
// 主审查拿不到判定的根因就是「带工具走多步编排、输出预算被中途叙述吃掉、
// 末步以工具调用收尾」。收尾调用若再带工具，会重复同一个失败模式。
//
// ⚠️ 这条测试**必须真的挂上 tool resolver** 才有意义：SubAgentManager 在
// toolMgr==nil 时本就不会注入工具，此时无论有没有 WithSkipTools 断言都恒成立
// ——那是一条永远不会失败的空测试。2026-08-06 反向验证时正是靠「删掉
// WithSkipTools 后测试仍通过」发现了这个空过陷阱。改这条测试前先确认
// `assertRealGuard` 仍然有效。
func TestReviewVerdictOnly_SkipsToolsToAvoidSameFailureMode(t *testing.T) {
	prov := &toolWitnessProvider{reply: `{"passed": true, "notes": ""}`}
	saMgr := subagent.NewSubAgentManager(prov, "test-model")
	defer saMgr.CloseAll()
	attachProbeTool(t, saMgr)
	e := NewExecutor(saMgr, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 先自检：确认这套装配**确实会**注入工具，否则下面的断言是空过的。
	if _, err := e.Review(ctx, &DAGNode{ID: "probe", Name: "自检"}, "产物"); err != nil {
		t.Fatalf("自检调用失败: %v", err)
	}
	if !prov.sawTools() {
		t.Fatal("自检失败：带工具路径没有注入工具，本测试无法证伪 —— " +
			"说明 tool resolver 没挂上，断言会恒成立（空测试）")
	}

	// 正式断言：重置见证标记，只观察收尾调用。
	prov.resetTools()
	if _, err := e.reviewVerdictOnly(ctx, &DAGNode{ID: "n5", Name: "审查 database/"},
		midAnalysisNoVerdict); err != nil {
		t.Fatalf("reviewVerdictOnly 失败: %v", err)
	}
	if prov.sawTools() {
		t.Error("收尾判定调用注入了工具 —— 会重演「带工具多步、末步无正文」的失败模式，" +
			"必须使用 WithSkipTools()")
	}
}

// attachProbeTool 给 manager 挂一个真实的 ToolManager（含一个哑工具），
// 使「带工具」路径真的会把 tools 传给 provider。
func attachProbeTool(t *testing.T, saMgr *subagent.SubAgentManager) {
	t.Helper()
	tm := agenttools.NewToolManager(prompt.NewRegistry(), nil, nil)
	err := tm.Register(agenttools.ToolDef{
		Tool: llm.Tool{
			Name:        "probe_tool",
			Description: "test-only no-op tool",
			Execute: func(ctx *llm.ToolExecContext, input any) (any, error) {
				return "ok", nil
			},
		},
		Category: "utility",
	})
	if err != nil {
		t.Fatalf("注册探针工具失败: %v", err)
	}
	saMgr.SetToolResolver(tm, agenttools.ToolSessionContext{})
}

// toolWitnessProvider 记录每次调用是否带了工具定义。
type toolWitnessProvider struct {
	reply    string
	withTool int32
}

func (p *toolWitnessProvider) Name() string { return "tool-witness" }

func (p *toolWitnessProvider) DoGenerate(ctx context.Context, params llm.GenerateParams) (*llm.GenerateResult, error) {
	p.record(params)
	return &llm.GenerateResult{Text: p.reply, FinishReason: llm.FinishReasonStop}, nil
}

func (p *toolWitnessProvider) record(params llm.GenerateParams) {
	if len(params.Tools) > 0 {
		atomic.StoreInt32(&p.withTool, 1)
	}
}

func (p *toolWitnessProvider) DoStream(ctx context.Context, params llm.GenerateParams) (*llm.StreamResult, error) {
	p.record(params)
	ch := make(chan llm.StreamPart, 2)
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
			return
		case ch <- &llm.TextDeltaPart{Text: p.reply}:
		}
		select {
		case <-ctx.Done():
		case ch <- &llm.FinishPart{FinishReason: llm.FinishReasonStop}:
		}
	}()
	return &llm.StreamResult{Stream: ch}, nil
}

func (p *toolWitnessProvider) sawTools() bool { return atomic.LoadInt32(&p.withTool) == 1 }

func (p *toolWitnessProvider) resetTools() { atomic.StoreInt32(&p.withTool, 0) }

// TestReviewPrompt_DemandsVerdictNotToolCall 守住提示词里的关键约束：
// 模型必须被明确告知「不要以工具调用收尾」「即使验证不完整也要给判定」。
// 这两句是把 heuristic 命中率从 90% 压下去的源头治理，删掉即回归。
func TestReviewPrompt_DemandsVerdictNotToolCall(t *testing.T) {
	p := buildReviewSystemPrompt("")
	for _, want := range []string{
		"NEVER end your reply with a tool call",
		"still emit a verdict",
		"Budget your tool use",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("review system prompt 缺少关键约束 %q —— "+
				"这是防止「拿不到判定」的源头治理，不能删", want)
		}
	}
}

// TestReviewResultFromJSON_AcceptsRealWorldFieldAliases 用**线上真实形态**守住判定字段
// 别名兼容。
//
// 2026-08-06 实测：prompt 明确要求 `{"passed": ...}`，但 40 条「判定缺失」样本里
// 模型实际用的是 `verdict`(23 次)和 `pass`(4 次)。旧实现只认 bool 型`passed`，
// 于是这些**明明给了判定**的回复全部落进 heuristic 兜底、被保守判 FAIL、触发无谓重跑。
// 用同一批样本回归验证：旧逻辑命中 0/40，扩展别名后命中 28/40。
func TestReviewResultFromJSON_AcceptsRealWorldFieldAliases(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantPassed bool
	}{
		// 以下 raw 均取自线上真实样本的结尾形态
		{"verdict字符串fail", "分析若干。\n```json\n{\"verdict\": \"fail\"}\n```", false},
		{"verdict字符串pass", "分析若干。\n```json\n{\"verdict\": \"pass\"}\n```", true},
		{"verdict大写FAIL", "分析若干。\n```json\n{\"verdict\": \"FAIL\"}\n```", false},
		{"pass布尔false带reason", `{"pass": false, "reason": "输出只是意图声明"}`, false},
		{"passed布尔true", `{"passed": true, "notes": ""}`, true},
		{"中文通过", `{"verdict": "通过"}`, true},
		{"中文不通过", `{"verdict": "不通过"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := reviewResultFromJSON(lastNonEmptyLine(tc.raw))
			if !ok {
				// 末行取不到时允许全文扫描（与 parseReviewResult 同策略）
				r, ok = reviewResultFromJSON(tc.raw)
			}
			if !ok {
				t.Fatalf("未识别出判定 —— 会退化到 heuristic 保守判 FAIL 并触发重跑\nraw=%q", tc.raw)
			}
			if r.Passed != tc.wantPassed {
				t.Errorf("判定错误：want passed=%v got %v", tc.wantPassed, r.Passed)
			}
			if r.Source != ReviewSourceJSON {
				t.Errorf("Source 应为 json，实际 %q", r.Source)
			}
		})
	}
}

// TestReviewResultFromJSON_RejectsNonVerdictObjects 守住「解析成功 ≠ 解析对了」这条底线。
//
// 扩展字段别名后风险变大：`status` / `result` 这类键在普通 JSON 里很常见。
// 必须确认——**只有值能被识别成判定词时才算判定**，否则代码片段/配置对象会被
// 当成判定（bool 零值 = false），静默把产物判为不通过（2026-08-04 踩过）。
func TestReviewResultFromJSON_RejectsNonVerdictObjects(t *testing.T) {
	for _, raw := range []string{
		`{"timeout": 5000}`,                    // 无判定字段
		`{"status": "running"}`,                // status 存在但值不是判定词
		`{"result": 42}`,                       // result 存在但值是数字
		`{"status": "ok", "extra": 1}`,         // ok 是可识别的通过词 —— 这条应被接受
		`func main() { fmt.Println("hello") }`, // 根本不是 JSON
	} {
		r, ok := reviewResultFromJSON(raw)
		if raw == `{"status": "ok", "extra": 1}` {
			if !ok || !r.Passed {
				t.Errorf(`{"status":"ok"} 应被识别为通过，实际 ok=%v r=%+v`, ok, r)
			}
			continue
		}
		if ok {
			t.Errorf("不含判定的对象被误判为判定：raw=%q → %+v\n"+
				"这会让代码片段/配置被当成 FAIL，静默丢弃产物", raw, r)
		}
	}
}

// TestVerdictFromWord_UnknownWordsRejected 验证不认识的词不会被当成判定。
// 「拿不到结论」必须走收尾调用/兜底，绝不能猜。
func TestVerdictFromWord_UnknownWordsRejected(t *testing.T) {
	for _, w := range []string{"maybe", "partial", "运行中", "", "unknown"} {
		if _, ok := verdictFromWord(w); ok {
			t.Errorf("不认识的判定词 %q 不应被接受", w)
		}
	}
}
