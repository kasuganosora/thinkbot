package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
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
}

// NewLoopDetectionConfig 返回默认循环检测配置。
func NewLoopDetectionConfig() LoopDetectionConfig {
	return LoopDetectionConfig{
		WarnThreshold: 3,
		HardLimit:     5,
		WindowSize:    20,
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

// WithWindowSize 设置窗口大小。
func (c LoopDetectionConfig) WithWindowSize(n int) LoopDetectionConfig {
	c.WindowSize = n
	return c
}

// IsZero 判断配置是否为空。
func (c LoopDetectionConfig) IsZero() bool {
	return c.WarnThreshold == 0 && c.HardLimit == 0 && c.WindowSize == 0
}

// loopDetectionState 是 LoopDetectionMiddleware 的内部状态。
type loopDetectionState struct {
	mu         sync.Mutex
	windows    map[string]*loopWindow // key: channel
	hardWarned map[string]bool        // key: channel，防止重复硬警告
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
// 返回空字符串表示没有工具调用。
func toolCallsDigest(result *llm.GenerateResult) string {
	if result == nil || len(result.Steps) == 0 {
		return ""
	}

	// 提取所有工具调用的 (name, args) 对并排序
	type toolCallKey struct {
		name string
		args string
	}
	keys := make([]toolCallKey, 0)

	for _, step := range result.Steps {
		for _, tc := range step.ToolCalls {
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

// LoopDetectionMiddleware 返回一个 Middleware，检测 LLM 工具调用的重复循环。
func LoopDetectionMiddleware(cfg LoopDetectionConfig) Middleware {
	if cfg.IsZero() {
		return func(next core.Stage) core.Stage { return next }
	}

	state := &loopDetectionState{
		windows:    make(map[string]*loopWindow),
		hardWarned: make(map[string]bool),
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
							digest := toolCallsDigest(genResult)
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
}
