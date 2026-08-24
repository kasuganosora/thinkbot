package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// LoopDetectionMiddleware — 检测重复工具调用循环
//
// 借鉴 deer-flow 的 LoopDetectionMiddleware 设计：
//   - 对每次 LLM orchestration 产出的工具调用列表做 hash
//   - 滑动窗口追踪最近 N 次工具调用模式
//   - 相同模式出现 ≥warnThreshold 次 → 注入软警告
//   - 相同模式出现 ≥hardLimit 次 → 注入硬警告（强制 LLM 停止工具调用）
//
// 在 thinkbot 中的实现：
//   - 按 Channel 独立追踪
//   - 从 llm.result.Steps 提取工具调用信息
//   - 稳定 hash：对 tool_name + args 排序后 hash，确保参数顺序不影响匹配
//   - 警告通过延迟注入模式传递
//
// 使用方式：
//
//	detector := NewLoopDetectionConfig().
//	    WithWarnThreshold(3).
//	    WithHardLimit(5)
//	llmStage := stages.NewLLMStage(...)
//	guarded := LoopDetectionMiddleware(detector)(llmStage)
// ============================================================================

// LoopDetectionConfig 配置循环检测策略。
type LoopDetectionConfig struct {
	// WarnThreshold 相同工具调用模式出现多少次后注入软警告。默认 3。
	WarnThreshold int
	// HardLimit 相同模式出现多少次后注入硬警告。默认 5。
	HardLimit int
	// WindowSize 滑动窗口大小（记录最近 N 次的工具调用模式）。默认 20。
	WindowSize int
	// ExemptTools 这些工具（通常是阻塞式长任务类，如 task）的重复调用
	// 不计入循环检测。工作流分析/执行可能持续数十秒到数分钟，bot 提交后阻塞等待
	// 属正常行为，不应被误判为死循环而强制收尾。
	ExemptTools []string

	// LowValueTools 这些工具即使被反复调用也不代表任务推进（统计类 text_stats、
	// 健康检查 health、记忆读取 memory、延迟工具搜索 tool_search、计算类 calculate，
	// 以及跨渠道只读 misskey_* 等）。当「连续多步仅由这些工具构成、且其间没有任何
	// 其它工具调用」时，视为「无进展的混乱调用循环」，触发硬警告强制收尾。
	// 用于弥补「完全相同 hash≥N 才触发」无法捕捉的「每次参数都不同的多变混乱调用」
	// （退化模型交替调 text_stats/memory/misskey_* 直到被打断，正是 cfblog 长任务
	// 做不完的根因之一）。支持通配后缀 "*"（如 "misskey_*" 匹配所有 Misskey 工具）。
	LowValueTools []string

	// ChaosThreshold 连续多少步「仅低价值工具」后触发硬警告。默认 5。≤0 表示禁用。
	ChaosThreshold int
}

// NewLoopDetectionConfig 返回默认循环检测配置。
func NewLoopDetectionConfig() LoopDetectionConfig {
	return LoopDetectionConfig{
		WarnThreshold:  3,
		HardLimit:      5,
		WindowSize:     20,
		ChaosThreshold: 5,
		LowValueTools: []string{
			"text_stats", "text_hash", "text_entities",
			"health", "memory", "tool_search", "calculate", "calculator",
			"misskey_*",
		},
	}
}

// WithWarnThreshold 设置软警告阈值。
func (c LoopDetectionConfig) WithWarnThreshold(n int) LoopDetectionConfig {
	c.WarnThreshold = n
	return c
}

// WithHardLimit 设置硬限制。
func (c LoopDetectionConfig) WithHardLimit(n int) LoopDetectionConfig {
	c.HardLimit = n
	return c
}

// WithExemptTools 设置循环检测豁免工具（其重复调用不计入检测）。
func (c LoopDetectionConfig) WithExemptTools(tools ...string) LoopDetectionConfig {
	c.ExemptTools = append(c.ExemptTools, tools...)
	return c
}

// WithWindowSize 设置窗口大小。
func (c LoopDetectionConfig) WithWindowSize(n int) LoopDetectionConfig {
	c.WindowSize = n
	return c
}

// WithChaosThreshold 设置多变混乱调用触发的连续步数阈值。
func (c LoopDetectionConfig) WithChaosThreshold(n int) LoopDetectionConfig {
	c.ChaosThreshold = n
	return c
}

// WithLowValueTools 设置低价值（不代表推进）工具列表。
func (c LoopDetectionConfig) WithLowValueTools(tools ...string) LoopDetectionConfig {
	c.LowValueTools = append(c.LowValueTools, tools...)
	return c
}

// IsZero 判断配置是否为空。
func (c LoopDetectionConfig) IsZero() bool {
	return c.WarnThreshold == 0 && c.HardLimit == 0 && c.WindowSize == 0
}

// loopDetectionState 是 LoopDetectionMiddleware 的内部状态。
type loopDetectionState struct {
	mu          sync.Mutex
	windows     map[string]*loopWindow // key: channel
	hardWarned  map[string]bool        // key: channel，防止重复硬警告
	chaosStreak map[string]int         // key: channel，连续「仅低价值工具」步数
}

// loopWindow 是 per-channel 的滑动窗口。
type loopWindow struct {
	hashes    []string // 最近 N 个工具调用 hash
	maxSize   int
	freqCount map[string]int // hash → 出现次数
}

func newLoopWindow(maxSize int) *loopWindow {
	return &loopWindow{
		hashes:    make([]string, 0, maxSize),
		maxSize:   maxSize,
		freqCount: make(map[string]int),
	}
}

// push 添加一个 hash 到窗口，返回该 hash 的当前出现次数。
func (w *loopWindow) push(hash string) int {
	w.hashes = append(w.hashes, hash)
	if len(w.hashes) > w.maxSize {
		// 移除最旧的
		old := w.hashes[0]
		w.hashes = w.hashes[1:]
		w.freqCount[old]--
		if w.freqCount[old] <= 0 {
			delete(w.freqCount, old)
		}
	}
	w.freqCount[hash]++
	return w.freqCount[hash]
}

// toolCallsDigest 从 GenerateResult 的 Steps 中提取工具调用信息，生成稳定 hash。
// 返回空字符串表示没有（非豁免的）工具调用。
// exempt 为需要排除的工具名集合（如阻塞式长任务工具 task），其调用不计入循环检测。
func toolCallsDigest(result *llm.GenerateResult, exempt map[string]bool) string {
	if result == nil || len(result.Steps) == 0 {
		return ""
	}

	// 提取所有工具调用的 (name, args) 对并排序（跳过豁免工具）
	type toolCallKey struct {
		name string
		args string
	}
	keys := make([]toolCallKey, 0)

	for _, step := range result.Steps {
		for _, tc := range step.ToolCalls {
			if exempt[tc.ToolName] {
				continue
			}
			argsJSON, err := json.Marshal(tc.Input)
			if err != nil {
				argsJSON = []byte("{}")
			}
			keys = append(keys, toolCallKey{
				name: tc.ToolName,
				args: string(argsJSON),
			})
		}
	}

	if len(keys) == 0 {
		return ""
	}

	// 排序确保稳定
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name != keys[j].name {
			return keys[i].name < keys[j].name
		}
		return keys[i].args < keys[j].args
	})

	// 构建规范化的序列字符串并 hash
	canonical := make([]string, 0, len(keys))
	for _, k := range keys {
		canonical = append(canonical, fmt.Sprintf("%s:%s", k.name, k.args))
	}

	h := sha256.New()
	for _, s := range canonical {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16] // 取前 16 字符足够
}

// isLowValue 判断工具名是否属于「低价值（不代表任务推进）」集合。
// 支持通配后缀 "*"（如 "misskey_*" 匹配所有以 "misskey_" 开头的工具）。
func (c LoopDetectionConfig) isLowValue(name string) bool {
	for _, t := range c.LowValueTools {
		if t == name {
			return true
		}
		if strings.HasSuffix(t, "*") {
			prefix := strings.TrimSuffix(t, "*")
			if prefix != "" && strings.HasPrefix(name, prefix) {
				return true
			}
		}
	}
	return false
}

// nonExemptToolNames 提取本次 GenerateResult 中所有非豁免工具调用的名称。
// 返回空切片表示没有任何（非豁免的）工具调用。供多变混乱调用检测判定
// 「这一步是否仅由低价值工具构成」。
func nonExemptToolNames(result *llm.GenerateResult, exempt map[string]bool) []string {
	names := make([]string, 0)
	if result == nil || len(result.Steps) == 0 {
		return names
	}
	for _, step := range result.Steps {
		for _, tc := range step.ToolCalls {
			if exempt[tc.ToolName] {
				continue
			}
			names = append(names, tc.ToolName)
		}
	}
	return names
}

// LoopDetectionMiddleware 返回一个 Middleware，检测 LLM 工具调用的重复循环。
func LoopDetectionMiddleware(cfg LoopDetectionConfig) Middleware {
	if cfg.IsZero() {
		return func(next core.Stage) core.Stage { return next }
	}

	state := &loopDetectionState{
		windows:     make(map[string]*loopWindow),
		hardWarned:  make(map[string]bool),
		chaosStreak: make(map[string]int),
	}

	// 构建豁免工具集合（轮询类工具不计入循环检测）
	exempt := make(map[string]bool, len(cfg.ExemptTools))
	for _, t := range cfg.ExemptTools {
		exempt[t] = true
	}

	return func(next core.Stage) core.Stage {
		return &core.StageFunc{
			StageName: next.Name(),
			Fn: func(ctx context.Context, env *core.Envelope) (*core.Envelope, error) {
				channel := env.Message.Channel
				if channel == "" {
					return next.Process(ctx, env)
				}

				// 如果已经触发硬警告，跳过
				state.mu.Lock()
				hard := state.hardWarned[channel]
				state.mu.Unlock()
				if hard {
					return next.Process(ctx, env)
				}

				// ---- 执行 ----
				result, err := next.Process(ctx, env)

				// ---- After: 检测循环 ----
				if result != nil {
					if v, ok := result.Get("llm.result"); ok {
						if genResult, ok := v.(*llm.GenerateResult); ok && genResult != nil {
							digest := toolCallsDigest(genResult, exempt)

							// 多变混乱调用检测：连续多步「仅低价值工具」且无任何推进 → 强制收尾。
							// 弥补「完全相同 hash≥N 才触发」无法捕捉的「每次参数都不同的混乱调用」
							// （退化模型交替调 text_stats/memory/misskey_* 直到被打断）。
							lowValueOnly := false
							if cfg.ChaosThreshold > 0 && len(cfg.LowValueTools) > 0 {
								names := nonExemptToolNames(genResult, exempt)
								if len(names) > 0 {
									lowValueOnly = true
									for _, n := range names {
										if !cfg.isLowValue(n) {
											lowValueOnly = false
											break
										}
									}
								}
							}
							state.mu.Lock()
							if lowValueOnly {
								state.chaosStreak[channel]++
							} else {
								state.chaosStreak[channel] = 0
							}
							chaosStreak := state.chaosStreak[channel]
							state.mu.Unlock()

							if digest != "" {
								state.mu.Lock()

								win, exists := state.windows[channel]
								if !exists {
									win = newLoopWindow(cfg.WindowSize)
									state.windows[channel] = win
								}

								count := win.push(digest)

								warnThreshold := cfg.WarnThreshold
								hardLimit := cfg.HardLimit

								if hardLimit > 0 && count >= hardLimit && !state.hardWarned[channel] {
									state.hardWarned[channel] = true
									state.mu.Unlock()

									core.QueueWarning(result, core.Warning{
										Source: "loop_detection",
										Level:  core.WarningLevelHard,
										Message: fmt.Sprintf("You have called the same tool(s) with the same arguments %d times. You are stuck in a loop. STOP making tool calls and produce your final answer NOW with the results collected so far.",
											count),
									})
								} else if warnThreshold > 0 && count >= warnThreshold {
									state.mu.Unlock()

									core.QueueWarning(result, core.Warning{
										Source: "loop_detection",
										Level:  core.WarningLevelSoft,
										Message: fmt.Sprintf("You have repeated the same tool call pattern %d times. Consider wrapping up and producing a final answer instead of continuing to call tools.",
											count),
									})
								} else {
									state.mu.Unlock()
								}
							}

							// 多变混乱调用 → 硬警告（独立判定，仅当本步前尚未硬警告）
							if chaosStreak >= cfg.ChaosThreshold && cfg.ChaosThreshold > 0 && !hard {
								state.mu.Lock()
								if !state.hardWarned[channel] {
									state.hardWarned[channel] = true
									state.mu.Unlock()
									core.QueueWarning(result, core.Warning{
										Source: "loop_detection",
										Level:  core.WarningLevelHard,
										Message: fmt.Sprintf("You have spent %d consecutive steps calling only non-progress tools (stats/health/memory/search/notes) without advancing the task. You appear stuck in a confused loop. STOP making tool calls and produce your final answer NOW with whatever results you have. IMPORTANT: your current system-prompt tool list is the ONLY source of truth. If you did NOT receive a real tool error this turn (e.g. \"No such tool\" / \"tool not found\" / a resolve or parse error), do NOT conclude tools \"disappeared\" or \"vanished\" — a past failure does NOT mean the current tool set is broken.",
											chaosStreak),
									})
								} else {
									state.mu.Unlock()
								}
							}
						}
					}
				}

				return result, err
			},
		}
	}
}

// ResetChannel 重置某 channel 的循环检测状态。
func (s *loopDetectionState) ResetChannel(channel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.windows, channel)
	delete(s.hardWarned, channel)
	delete(s.chaosStreak, channel)
}
