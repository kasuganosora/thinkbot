package pipeline

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// LazyResponseMiddleware — 检测"不调工具直接编造结论"的偷懒行为（两级级联）
//
// 问题场景：
//   - 用户问"有没有安装 git"，模型不执行 which git 就直接说"未安装"
//   - 用户问"系统信息"，模型不执行命令就编造表格
//
// 两级级联（把裁判权从僵化正则交还给模型）：
//   - [一级] 词表粗筛（高召回，允许误报）：纯正则命中"可能在编造环境状态"
//     的文本模式。任务从"判对"降级为"别漏"——混沌靠二级消解，不在一级硬判。
//   - [二级] LLM 语义裁决（LazyJudge）：只在一级命中后才调用，读"回复原文 +
//     用户问题 + 本轮是否真调了工具"，输出结构化裁决 {lazy, entity_asserted,
//     confidence, reason}。语义在字符串之外，只有模型能消解"X 不存在"到底是
//     环境断言还是概念陈述这类混沌。
//
// 关键设计点（沿用管线既有的 L1 提取→置信门控→晋升 哲学）：
//   1. 一级反着调：有二级兜底后词表放宽，宁可多漏给二级也不要在一级把真偷懒放跑。
//   2. 二级用便宜快模型：只读一条回复+规则，无会话全上下文（loop-back 低频，
//      成本可忽略）。
//   3. 裁决喂关键证据：用户问题 + 实际工具调用，比词表准得多。
//   4. 不对称权限：裁决器只能"确认"或"否决"一级已召回的回复，绝不能凭空扩大
//      罪名去触发本没命中的回复——即使裁决器抽风，爆炸半径也被一级词表限死。
//
// 降级路径：
//   - 模型已调工具（纠正行为）→ 复位该 channel 警告标记，直接发送。
//   - 一级未命中 → 直接发送（零额外开销）。
//   - 二级裁决 lazy=false（否决）→ 直接发送原回复。
//   - 二级裁决器报错 → fail-open 放行原回复（不重算，避免双重发送风险）。
//   - 二级确认 lazy=true → 注入硬警告，同轮 loop-back 重算；ClearActions 保证
//     修正轮成为 envelope 内唯一回复（防 2026-09-16 Telegram 重复回复事故）。
//   - cfg.Judge == nil（未配置二级）→ 一级命中也不重算（降级为只检测不拦截）。
//
// 每次一级召回都会经 LazyJudgeSink 落库（旁路、非阻塞），攒标注语料。
// ============================================================================

// lazyPatterns 匹配"可能在编造环境状态"的中文文本模式（一级，高召回）。
// 这些模式本身合法，但在"无工具调用"前提下出现极大概率是偷懒猜测。
// 注意：一级不再要求"环境实体共现"——那道门控会让真偷懒（用不同措辞断言）
// 在一级漏掉；语义是否真偷懒交给二级 LLM 裁决。
var lazyPatterns = []*regexp.Regexp{
	// 环境状态否定断言
	regexp.MustCompile(`(?i)(未安装|没有安装|不存在|无法找到|找不到|不可用|未检测到|无可用)`),
	// 环境状态肯定断言（带具体值）
	regexp.MustCompile(`(?i)(已安装|已配置|当前版本是|运行的是|系统为|操作系统是|内核版本|内存大小|磁盘空间)`),
	// 编造命令输出痕迹
	regexp.MustCompile(`(?i)(结果[是为]|如下[表结]|汇总[如结果]|尝试过.*?结果)`),
	// 表格输出（Markdown 表格常见于环境探测结果的编造）
	regexp.MustCompile(`\|.*\|.*\|\n\|[-:]+\|`),
	// 未经核实的断言口吻（放宽召回：这些强暗示"没验证就下结论"）
	regexp.MustCompile(`(?i)(未验证|没(有|去)?验证|未经核实|未经(检验|确认)|没有(实际)?(确认|核实)|我(猜|估计|推测|觉得可能))`),
}

// lazyResponseState 按通道追踪是否已注入过警告。
type lazyResponseState struct {
	mu     sync.Mutex
	warned map[string]bool // channel → 已警告
}

func newLazyResponseState() *lazyResponseState {
	return &lazyResponseState{
		warned: make(map[string]bool),
	}
}

// LazyJudgeRequest 喂给二级裁决器的证据。
type LazyJudgeRequest struct {
	// Reply 本轮 LLM 生成的回复原文。
	Reply string
	// UserQuery 本轮用户的问题（决定"是否该验证"的关键上下文）。
	UserQuery string
	// HadToolCalls 本轮实际是否调用了工具（true=已验证，不可能偷懒）。
	HadToolCalls bool
}

// LazyJudgeResult 二级 LLM 裁决结果。
type LazyJudgeResult struct {
	// Lazy 是否偷懒（无工具验证就断言环境状态）。
	Lazy bool `json:"lazy"`
	// EntityAsserted 回复是否断言了具体可验证的环境实体。
	EntityAsserted bool `json:"entity_asserted"`
	// Confidence 裁决置信度 0..1。
	Confidence float64 `json:"confidence"`
	// Reason 一句话理由（落库用）。
	Reason string `json:"reason"`
}

// LazyJudge 二级 LLM 语义裁决器（Tier 2）。把"是否偷懒"的裁判权从正则交给模型。
// 仅在一級正则召回后才被调用；裁决器只能确认或否决，不能扩大触发面。
type LazyJudge interface {
	Adjudicate(ctx context.Context, req LazyJudgeRequest) (*LazyJudgeResult, error)
}

// LazyResponseConfig 配置防偷懒策略。
type LazyResponseConfig struct {
	// Enabled 是否启用。默认 true。
	Enabled bool
	// Judge 二级 LLM 裁决器。nil 时退化为"只检测不拦截"（一级命中也不重算）。
	Judge LazyJudge
	// Sink 判定结果落库槽（旁路观测）。nil 则不落库。
	Sink LazyJudgeSink
}

// NewLazyResponseConfig 返回默认防偷懒配置。
func NewLazyResponseConfig() LazyResponseConfig {
	return LazyResponseConfig{
		Enabled: true,
	}
}

// IsZero 判断配置是否为空。
func (c LazyResponseConfig) IsZero() bool {
	return !c.Enabled
}

// hasLazyIndicators 一级召回：文本是否"可能"在偷懒（高召回，允许误报）。
// 仅做正则粗筛——语义是否真偷懒由二级 LLM 裁决，不在此硬判。
func hasLazyIndicators(text string) bool {
	if len(strings.TrimSpace(text)) < 10 {
		return false // 太短不算偷懒
	}
	for _, pat := range lazyPatterns {
		if pat.MatchString(text) {
			return true
		}
	}
	return false
}

// hadToolCalls 检查 result 的所有步骤中是否有实际执行过的工具调用。
func hadToolCalls(result *llm.GenerateResult) bool {
	if result == nil || len(result.Steps) == 0 {
		return false
	}
	for _, step := range result.Steps {
		if len(step.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// LazyResponseMiddleware 返回一个 Middleware，检测并抑制"不调工具就下结论"的行为。
func LazyResponseMiddleware(cfg LazyResponseConfig) Middleware {
	if cfg.IsZero() {
		return func(next core.Stage) core.Stage { return next }
	}

	state := newLazyResponseState()

	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name() + ".lazy-guard",
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				// ---- 执行下游 Stage ----
				result, err := next.Process(ctx, env)
				if err != nil || result == nil {
					return result, err
				}

				// 提取 LLM 执行结果
				v, ok := result.Get("llm.result")
				if !ok {
					return result, err
				}
				genResult, ok := v.(*llm.GenerateResult)
				if !ok || genResult == nil {
					return result, err
				}

				channel := env.Message.Channel

				// 模型已调用工具（纠正了行为）→ 复位该 channel 的警告标记，
				// 允许后续再偷懒时仍被警告，避免同 channel 永久静默。
				if hadToolCalls(genResult) {
					state.mu.Lock()
					if state.warned[channel] {
						state.warned[channel] = false
					}
					state.mu.Unlock()
					return result, err
				}

				// ---- 一级：高召回正则召回（允许误报）----
				if !hasLazyIndicators(genResult.Text) {
					return result, err // 未召回 → 直接发送，零额外开销
				}

				// ---- 二级：LLM 语义裁决（仅一级命中才调用）----
				// 非对称权限：裁决器只能确认/否决一级已召回的回复，不能扩大触发面。
				var judgeRes *LazyJudgeResult
				if cfg.Judge != nil {
					judgeRes, err = cfg.Judge.Adjudicate(ctx, LazyJudgeRequest{
						Reply:        genResult.Text,
						UserQuery:    env.Message.Text,
						HadToolCalls: false,
					})
					if err != nil {
						// 裁决失败：fail-open 放行原回复（不重算），避免双重发送风险。
						judgeRes = &LazyJudgeResult{Lazy: false, Reason: "judge error: " + err.Error()}
					}
				}

				if judgeRes == nil || !judgeRes.Lazy {
					// 无 judge 配置，或二级否决（含 judge 失败）→ 直接发送原回复。
					finalAct := "veto"
					if judgeRes != nil && strings.HasPrefix(judgeRes.Reason, "judge error:") {
						finalAct = "judge_error_allow"
					}
					if cfg.Judge == nil {
						finalAct = "no_judge"
					}
					recordLazy(cfg.Sink, ctx, env, channel, len(genResult.Text), false, judgeRes, cfg.Judge == nil, finalAct)
					return result, err
				}

				// 二级确认偷懒 → 同轮 loop-back 重算。
				state.mu.Lock()
				alreadyWarned := state.warned[channel]
				if !alreadyWarned {
					state.warned[channel] = true
				}
				state.mu.Unlock()

				if alreadyWarned {
					// 本 channel 当轮已警告过，避免重复 loop-back 刷屏。
					recordLazy(cfg.Sink, ctx, env, channel, len(genResult.Text), true, judgeRes, false, "suppressed")
					return result, err
				}

				warning := core.Warning{
					Source: "lazy_response",
					Level:  core.WarningLevelHard,
					Message: `[SYSTEM WARNING - URGENT]
You just provided an answer about environment/system state WITHOUT calling any tools to verify.
This is a CRITICAL violation. You MUST NOT guess or assume environment state.

Rules you MUST follow:
1. For ANY question about what is installed, what files exist, system info, network status, or any other verifiable fact — you MUST call the appropriate tool FIRST (exec/shell for commands, read_file/list_dir for files, web_fetch for URLs, etc.)
2. Only AFTER receiving actual tool results may you base your answer on real data.
3. If a tool call fails, report the failure honestly — do not fabricate a result.
4. Your previous response was likely hallucinated. The user can see you did not call any tools.`,
				}
				core.QueueWarning(result, warning)

				// 同轮 loop-back：把硬警告注入 prompt 后立即重算 LLM，当轮即返回
				// 修正后的答案。next 是更内层 stage（不含本 middleware），只会重算
				// 一次、不会无限递归。
				//
				// 关键：首轮回复已被追加进 result.Actions。loop-back 复用同一
				// envelope 再次重算，会再追加一条修正轮回复。若不清除首轮 Action，
				// engine 会把两条都派发出站 → 同一条消息重复回复（见 2026-09-16
				// Telegram 重复回复事故，trace 9fbd42e3…）。故重跑前清空首轮
				// Action，使修正轮成为 envelope 里唯一的回复；重算失败则还原首轮，
				// 保证至少有一条回复出站。
				firstActions := result.Actions()
				result.ClearActions()
				if rerun, rerr := next.Process(ctx, result); rerr == nil {
					// 把警告随修正结果一并带回，保持返回 Envelope 的警告一致性。
					core.QueueWarning(rerun, warning)
					recordLazy(cfg.Sink, ctx, env, channel, len(genResult.Text), true, judgeRes, false, "loopback")
					return rerun, rerr
				}
				// 重算失败：还原首轮回复，避免无回复出站。
				for _, a := range firstActions {
					result.AddAction(a)
				}
				recordLazy(cfg.Sink, ctx, env, channel, len(genResult.Text), true, judgeRes, false, "loopback_failed")
				return result, err
			},
		}
	}
}

// recordLazy 落库一条两级裁决记录（旁路、非阻塞）。
func recordLazy(sink LazyJudgeSink, ctx context.Context, env *core.Envelope, channel string, replyLen int, lazy bool, r *LazyJudgeResult, noJudge bool, finalAction string) {
	if sink == nil {
		return
	}
	conf := 0.0
	if r != nil {
		conf = r.Confidence
	}
	reason := "(no judge result)"
	if noJudge {
		reason = "(no tier-2 judge configured)"
	} else if r != nil && r.Reason != "" {
		reason = r.Reason
	}
	sink.RecordLazyJudge(ctx, LazyJudgeRecord{
		TS:             time.Now(),
		BotID:          env.Message.BotID,
		Channel:        channel,
		Stage1Matched:  true,
		ReplyLen:       replyLen,
		HadToolCalls:   false,
		Lazy:           lazy,
		EntityAsserted: r != nil && r.EntityAsserted,
		Confidence:     conf,
		Reason:         reason,
		FinalAction:    finalAction,
	})
}
