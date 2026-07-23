package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kasuganosora/thinkbot/util/traceid"
)

// ============================================================================
// loopController — 动态步数控制器
//
// 背景：orchestration 循环原先只有一个静态硬上限 MaxSteps，导致复杂任务
// （如大规模代码修复）在还没跑完时就被腰斩，而简单任务又无法从更小的上限
// 中获益。loopController 用「软/硬双层预算 + 进展感知」取代单一硬上限：
//
//   - soft  (= MaxSteps)     ：常规预算。step < soft 时无条件放行。
//   - hard  (= HardMaxSteps) ：绝对安全网。任何情况都不会突破。
//   - 单次内重复检测          ：soft 之后，只要模型仍在发出「不同」的工具调用
//                              （说明还在推进），就一路放行到 hard；一旦连续
//                              repeatLimit 步产生「完全相同」的工具调用签名
//                              （原地打转 / 死循环），立即停止，不浪费到 hard。
//
// 关键洞察：orchestration 循环只有在「上一步产生了可执行工具调用」时才会进入
// 下一轮（否则 FinishReason != tool-calls 会直接 break）。因此在 soft..hard
// 区间内，唯一值得停下的理由就是「陷入重复循环」。这样即可做到：
//   复杂但持续推进的任务  → 自动延长到 hard
//   原地打转的死循环      → 提前拦截
// 无需人工为每种任务猜测一个合适的步数上限。
// ============================================================================

// defaultRepeatLimit 是软预算内判定「陷入重复循环」的连续相同签名步数阈值。
// 软预算内容忍少量重复（可能是合理的重试）。
const defaultRepeatLimit = 3

// tightRepeatLimit 是超出软预算后收紧的重复阈值。任务已消耗大量步数，
// 此时对原地打转零容忍：连续 2 步相同签名即判定 stalled。
const tightRepeatLimit = 2

// defaultHardMultiplier 是未显式设置 HardMaxSteps 时，hard 相对 soft 的倍数。
const defaultHardMultiplier = 3

// loopController 追踪单次 orchestration 的步数预算与重复循环状态。
// 非并发安全：每次 orchestration 独占一个实例，循环本身是串行推进的。
type loopController struct {
	soft        int    // 软预算 = MaxSteps（<0 表示无限）
	hard        int    // 硬上限 = HardMaxSteps
	repeatLimit int    // 连续相同签名达到此值判定 stalled
	lastSig     string // 上一步的工具调用签名
	repeatCount int    // 当前签名连续出现次数
	stalled     bool   // 是否已判定陷入重复循环
}

// newLoopController 根据 soft(MaxSteps) 与 hard(HardMaxSteps) 构造控制器。
//
//	soft < 0  ：无限模式（对应 MaxSteps == -1），永不因步数上限停止。
//	soft == 0 ：单步 fast path 已在 orchestrate 上游处理，不会进入循环，
//	            此处退化为「最多 1 步」以保证安全。
//	hard == 0 ：不限制（无限）。用户未设步数上限，Bot 跑到任务完成为止，
//	           不会因步数预算耗尽而被腰斩（见 effectiveStepBudgets 注释）。
//	hard < 0  ：内置默认安全网（soft * defaultHardMultiplier），历史/内部语义。
//	hard > 0  ：有限硬上限。
//	hard < soft（且 hard != -1）：夹紧为 soft（hard 不得小于 soft）。
func newLoopController(soft, hard int) *loopController {
	lc := &loopController{
		soft:        soft,
		repeatLimit: defaultRepeatLimit,
	}

	if soft < 0 {
		lc.hard = -1 // 无限
		return lc
	}
	if soft == 0 {
		lc.hard = 1
		return lc
	}

	switch {
	case hard < 0:
		// 内置默认安全网
		hard = soft * defaultHardMultiplier
	case hard == 0:
		// 0 = 不限制（无限）
		hard = -1
	}
	// 有限硬上限时保证 >= soft；无限（-1）不夹紧
	if hard != -1 && hard < soft {
		hard = soft
	}
	lc.hard = hard
	return lc
}

// shouldContinue 判断第 step 步（0-based）是否可以开始执行。
func (lc *loopController) shouldContinue(step int) bool {
	if lc.soft < 0 || lc.hard < 0 {
		return true // 无限模式（soft=-1 或 hard=0→不限制）
	}
	if lc.stalled {
		return false // 已检测到死循环
	}
	if step < lc.soft {
		return true // 软预算内无条件放行
	}
	// soft..hard 区间：尚在推进（未 stalled）且未触及硬上限则继续。
	return step < lc.hard
}

// recordStep 在第 step 步（0-based）的工具调用完成后调用，用工具调用签名
// 更新重复检测状态。签名为空（该步无工具调用）时重置连续计数。
//
// 重复容忍度随进度收紧：软预算内允许连续 repeatLimit 次相同签名（容错重试），
// 超出软预算后收紧为 tightRepeatLimit 次即判定 stalled——已经跑了很多步，
// 不再容忍原地打转。
func (lc *loopController) recordStep(step int, sig string) {
	if sig == "" {
		lc.lastSig = ""
		lc.repeatCount = 0
		return
	}
	if sig == lc.lastSig {
		lc.repeatCount++
	} else {
		lc.lastSig = sig
		lc.repeatCount = 1
	}

	limit := lc.repeatLimit
	if lc.soft > 0 && step >= lc.soft {
		limit = tightRepeatLimit
	}
	if limit > 0 && lc.repeatCount >= limit {
		lc.stalled = true
	}
}

// toolCallSignature 从一组工具调用生成稳定签名（name+args 排序后 hash）。
// 参数顺序不影响结果。无工具调用时返回空字符串。
//
// 与 agent/pipeline/loop_detection.go 的 toolCallsDigest 思路一致，但作用域
// 不同：此函数用于「单次 orchestration 内、逐步」的重复检测，且位于 llm 包，
// 不能反向依赖 pipeline 包，故独立实现。
func toolCallSignature(calls []ToolCall) string {
	if len(calls) == 0 {
		return ""
	}

	type key struct {
		name string
		args string
	}
	keys := make([]key, 0, len(calls))
	for _, tc := range calls {
		argsJSON, err := json.Marshal(tc.Input)
		if err != nil {
			argsJSON = []byte("{}")
		}
		keys = append(keys, key{name: tc.ToolName, args: string(argsJSON)})
	}

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name != keys[j].name {
			return keys[i].name < keys[j].name
		}
		return keys[i].args < keys[j].args
	})

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k.name))
		h.Write([]byte{0})
		h.Write([]byte(k.args))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// stoppedByGuard 报告循环是否因步数守卫（重复循环或硬上限）而停止，
// 而非模型自然收尾（不再产生工具调用）。无限模式（hard<0，即 0=不限制）
// 永远不会因步数上限停止。
func (lc *loopController) stoppedByGuard(steps int) bool {
	if lc.stalled {
		return true
	}
	if lc.hard < 0 {
		return false // 不限制：不会因步数上限停止
	}
	return lc.soft >= 0 && steps >= lc.hard
}

// describeLoopStop 返回控制器停止原因的可读描述，用于日志。
func (lc *loopController) describeLoopStop(steps int) string {
	switch {
	case lc.stalled:
		return fmt.Sprintf("stalled: same tool calls repeated %d times", lc.repeatCount)
	case lc.soft >= 0 && steps >= lc.hard:
		return fmt.Sprintf("reached hard cap %d", lc.hard)
	default:
		return "no more tool calls"
	}
}

// logLoopStop 在编排循环结束后记录停止原因。仅当循环因步数守卫（撞硬顶
// 或陷入重复循环）而停止时才 Warn，便于排查「任务被腰斩 / 死循环」；模型
// 自然收尾则不噪声。
func logLoopStop(ctx context.Context, lc *loopController, steps int) {
	if lc == nil || !lc.stoppedByGuard(steps) {
		return
	}
	if logger := traceid.L(ctx); logger != nil {
		logger.Warnw("orchestration loop stopped by step guard",
			"reason", lc.describeLoopStop(steps),
			"steps", steps,
			"soft_max", lc.soft,
			"hard_max", lc.hard,
		)
	}
}
