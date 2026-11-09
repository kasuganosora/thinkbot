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
// LazyResponseMiddleware — 检测"不调工具直接编造结论"的偷懒行为（两级级联 + 对账）
//
// 问题场景：
//   - 用户问"有没有安装 git"，模型不执行 which git 就直接说"未安装"
//   - 用户问"系统信息"，模型不执行命令就编造表格
//   - 用户让模型改代码，模型一个工具没调却说"改完了，go build 通过、make proto 成功"
//
// 三条检测路径（按代价从低到高）：
//   - [确定性对账] 动作完成声明 ↔ 运行时证据（本轮 Steps + 上轮持久化命令）。
//     命中即同轮 loop-back，不需要 LLM 裁决，且可离线回放测误报率。
//   - [一级] 词表粗筛（高召回，允许误报）：环境状态断言 + 裸动作完成声明。
//   - [二级] LLM 语义裁决（LazyJudge）：只在一级命中后才调用，读"回复原文 +
//     用户问题 + 本轮是否真调了工具"，输出结构化裁决。
//
// 关键设计点（沿用管线既有的 L1 提取→置信门控→晋升 哲学）：
//   1. 一级反着调：有二级兜底后词表放宽，宁可多漏给二级也不要在一级把真偷懒放跑。
//   2. 二级用便宜快模型：只读一条回复+规则，无会话全上下文（loop-back 低频）。
//   3. 裁决喂关键证据：用户问题 + 实际工具调用，比词表准得多。
//   4. 不对称权限：裁决器只能"确认"或"否决"一级已召回的回复，绝不能凭空扩大
//      罪名去触发本没命中的回复——即使裁决器抽风，爆炸半径也被一级词表限死。
//
// 降级路径：
//   - 模型已调工具且文本未命中任何检测 → 复位该 channel 警告标记，直接发送。
//   - 一级未命中且对账通过 → 直接发送（零额外开销）。
//   - 二级裁决 lazy=false（否决）→ 直接发送原回复。
//   - 二级裁决器报错 → fail-open 放行原回复（不重算，避免双重发送风险）。
//   - 确定性对账命中 / 二级确认偷懒 → 注入硬警告，同轮 loop-back 重算；
//     ClearActions 保证修正轮成为 envelope 内唯一回复（防 2026-09-16 Telegram
//     重复回复事故）。
//   - cfg.Judge == nil（未配置二级）→ 一级命中也不重算（降级为只检测不拦截）；
//     确定性对账不依赖 Judge，仍然生效。
//
// 每次一级召回/对账命中都会经 LazyJudgeSink 落库（旁路、非阻塞）。
// ============================================================================

// lazyPatterns 匹配"可能在编造环境状态"的中文文本模式（一级，高召回）。
// 这些模式本身合法，但在"无工具调用"前提下出现极大概率是偷懒猜测。
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
// 仅在一级正则召回后才被调用；裁决器只能确认或否决，不能扩大触发面。
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
	// PriorCommands returns the shell commands persisted for the previous
	// assistant turn. It lets claim reconciliation accept truthful references
	// to work done in an earlier turn ("already pushed" / "build passed then").
	// Nil means no prior evidence (more conservative).
	PriorCommands func(env *core.Envelope) []string
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
//
// 除环境状态断言外，还召回「裸动作完成声明」（改完了 / 已修复 / 已删除）；
// 带具体命令的完成声明（go build 通过 / 已推送）交给确定性对账先处理，
// 不在这里重复召回，避免同一句话被两级各判一次。
func hasLazyIndicators(text string) bool {
	if len(strings.TrimSpace(text)) < 10 {
		return false // 太短不算偷懒
	}
	if changeClaimRe.MatchString(text) {
		return true
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

// environmentWarning is the original lazy-guard hard warning.
const environmentWarning = `[SYSTEM WARNING - URGENT]
You just provided an answer about environment/system state WITHOUT calling any tools to verify.
This is a CRITICAL violation. You MUST NOT guess or assume environment state.

Rules you MUST follow:
1. For ANY question about what is installed, what files exist, system info, network status, or any other verifiable fact — you MUST call the appropriate tool FIRST (exec/shell for commands, read_file/list_dir for files, web_fetch for URLs, etc.)
2. Only AFTER receiving actual tool results may you base your answer on real data.
3. If a tool call fails, report the failure honestly — do not fabricate a result.
4. Your previous response was likely hallucinated. The user can see you did not call any tools.`

// claimWarning explains exactly which claim had no runtime evidence, so the
// recomputed turn can either perform the action or state honestly that it did not.
const claimWarning = `[SYSTEM WARNING - URGENT]
Your previous response claimed that an action was completed, but the runtime execution record for this turn contains NO matching tool call.
Stating that something was done when it was not is a CRITICAL violation.

Rules you MUST follow:
1. A statement such as "build passed", "tests passed", "generated successfully", "committed", "pushed", or "the change is done" is only allowed when THIS TURN actually ran the corresponding tool and the result succeeded.
2. Earlier turns' self-descriptions are NOT evidence. If the work happened in an earlier turn, say so explicitly instead of implying you did it now.
3. If you cannot or did not run the action, say honestly that it was not executed and offer to do it.
4. The user can see the tool calls; an unbacked completion claim will be rejected.`

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
				hasTools := hadToolCalls(genResult)

				// loopBack 复用同一 envelope 再次重算，只重算一次、不会无限递归
				//（next 是更内层 stage，不含本 middleware）。重跑前清空首轮
				// Action，使修正轮成为 envelope 里唯一的回复；重算失败则还原首轮，
				// 保证至少有一条回复出站（2026-09-16 Telegram 重复回复事故）。
				loopBack := func(message string, lazy bool, judgeRes *LazyJudgeResult, claimCheck string) (*core.Envelope, error) {
					state.mu.Lock()
					alreadyWarned := state.warned[channel]
					if !alreadyWarned {
						state.warned[channel] = true
					}
					state.mu.Unlock()

					if alreadyWarned {
						recordLazy(cfg.Sink, ctx, env, channel, len(genResult.Text), hasTools, lazy, judgeRes, false, "suppressed", claimCheck)
						return result, err
					}

					warning := core.Warning{
						Source:  "lazy_response",
						Level:   core.WarningLevelHard,
						Message: message,
					}
					core.QueueWarning(result, warning)
					firstActions := result.Actions()
					result.ClearActions()
					if rerun, rerr := next.Process(ctx, result); rerr == nil {
						core.QueueWarning(rerun, warning)
						recordLazy(cfg.Sink, ctx, env, channel, len(genResult.Text), hasTools, lazy, judgeRes, false, "loopback", claimCheck)
						return rerun, rerr
					}
					for _, a := range firstActions {
						result.AddAction(a)
					}
					recordLazy(cfg.Sink, ctx, env, channel, len(genResult.Text), hasTools, lazy, judgeRes, false, "loopback_failed", claimCheck)
					return result, err
				}

				// ---- 确定性「声明↔证据对账」----
				// 本轮 Steps 是主证据；上轮持久化命令作为补充证据，避免把
				// 「上一轮已推送」这类真实复述误判为编造。有工具轮次同样对账，
				// 覆盖「部分编造」：调了几个工具，却把没跑的步骤也说成完成。
				ev := EvidenceFromResult(genResult)
				if cfg.PriorCommands != nil {
					ev.PriorCommands = cfg.PriorCommands(env)
				}
				if claim := CheckCompletionClaims(ClaimVisibleText(genResult.Text), ev); claim != nil {
					return loopBack(claimWarning, true, nil, claim.Kind+":"+claim.Required)
				}

				// 对账通过且本轮确实调用了工具 → 复位该 channel 的警告标记，
				// 允许后续再偷懒时仍被警告，避免同 channel 永久静默。
				// 与改造前「有工具调用就复位」的语义保持一致，不因一级词表
				// 是否命中而改变。
				if hasTools {
					state.mu.Lock()
					if state.warned[channel] {
						state.warned[channel] = false
					}
					state.mu.Unlock()
				}

				// ---- 一级：高召回正则召回（允许误报）----
				if !hasLazyIndicators(genResult.Text) {
					return result, err // 未召回 → 直接发送，零额外开销
				}

				// 本轮确实调用了工具：词表召回只作旁路，不送二级裁决、也不
				// loop-back（工具结果就在上下文里，模型裁决性价比低且易误伤）。
				// 不落库，避免大量正常工具轮次淹没标注语料。
				if hasTools {
					return result, err
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
					recordLazy(cfg.Sink, ctx, env, channel, len(genResult.Text), hasTools, false, judgeRes, cfg.Judge == nil, finalAct, "")
					return result, err
				}

				return loopBack(environmentWarning, true, judgeRes, "")
			},
		}
	}
}

// recordLazy 落库一条两级裁决记录（旁路、非阻塞）。
func recordLazy(sink LazyJudgeSink, ctx context.Context, env *core.Envelope, channel string, replyLen int, hadTools, lazy bool, r *LazyJudgeResult, noJudge bool, finalAction, claimCheck string) {
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
	} else if claimCheck != "" {
		reason = "deterministic claim↔evidence mismatch: " + claimCheck
	}
	sink.RecordLazyJudge(ctx, LazyJudgeRecord{
		TS:             time.Now(),
		BotID:          env.Message.BotID,
		Channel:        channel,
		Stage1Matched:  true,
		ReplyLen:       replyLen,
		HadToolCalls:   hadTools,
		Lazy:           lazy,
		EntityAsserted: r != nil && r.EntityAsserted,
		Confidence:     conf,
		Reason:         reason,
		FinalAction:    finalAction,
		ClaimCheck:     claimCheck,
	})
}
