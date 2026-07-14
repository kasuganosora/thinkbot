package pipeline

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// LazyResponseMiddleware — 检测"不调工具直接编造结论"的偷懒行为
//
// 问题场景：
//   - 用户问"有没有安装 git"，模型不执行 which git 就直接说"未安装"
//   - 用户问"系统信息"，模型不执行命令就编造表格
//
// 工作原理：
//   - LLMStage 执行完毕后，检查 GenerateResult：
//     1) 完全没有 tool_calls（一步到位的"答案"）
//     2) 回复文本命中环境状态断言模式（未安装 / 不存在 / 结果表格等）
//   - 两个条件同时满足 → 注入硬警告，要求下一轮必须先调工具
//   - 同一 channel 只警告一次（避免每轮都刷）
//
// 使用方式：
//
//	wrappedLLM := pipeline.WithMiddleware(llmStage,
//	    pipeline.LazyResponseMiddleware(),
//	)
// ============================================================================

// lazyPatterns 匹配"可能在编造环境状态"的中文文本模式。
// 这些模式本身是合法的自然语言表达，但在"无工具调用"的前提下
// 出现这些模式极大概率是偷懒猜测。
var lazyPatterns = []*regexp.Regexp{
	// 环境状态否定断言
	regexp.MustCompile(`(?i)(未安装|没有安装|不存在|无法找到|找不到|不可用|未检测到|无可用)`),
	// 环境状态肯定断言（带具体值）
	regexp.MustCompile(`(?i)(已安装|已配置|当前版本是|运行的是|系统为|操作系统是|内核版本|内存大小|磁盘空间)`),
	// 编造命令输出痕迹
	regexp.MustCompile(`(?i)(结果[是为]|如下[表结]|汇总[如结果]|尝试过.*?结果)`),
	// 表格输出（Markdown 表格常见于环境探测结果的编造）
	regexp.MustCompile(`\|.*\|.*\|\n\|[-:]+\|`),
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

// LazyResponseConfig 配置防偷懒策略。
type LazyResponseConfig struct {
	// Enabled 是否启用。默认 true。
	Enabled bool
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

// hasLazyIndicators 检测文本是否包含"可能偷懒"的指示模式。
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

				// 核心判断：无工具调用 + 文本命中偷懒模式 → 注入警告
				if !hadToolCalls(genResult) && hasLazyIndicators(genResult.Text) {
					channel := env.Message.Channel

					state.mu.Lock()
					alreadyWarned := state.warned[channel]
					if !alreadyWarned {
						state.warned[channel] = true
					}
					state.mu.Unlock()

					if !alreadyWarned {
						core.QueueWarning(result, core.Warning{
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
						})
					}
				}

				return result, err
			},
		}
	}
}
