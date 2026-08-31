package engagement

import "time"

// ============================================================================
// ConversationPhase — 对话阶段推断
//
// 参考 Houde et al. (2025) 发现：不同任务阶段（发散 vs 收敛）
// 需要不同的 Agent 行为配置。论文推测发散阶段（头脑风暴）需要
// 更高参与度，收敛阶段（筛选/总结）需要降低频率、专注总结。
//
// 本实现通过消息间隔模式推断对话阶段，供 TimingGate 的自适应
// 频率调整使用。
// ============================================================================

// ConversationPhase 表示推断的对话阶段。
type ConversationPhase string

const (
	// PhaseIdle 空闲：近期几乎没有消息。
	PhaseIdle ConversationPhase = "idle"
	// PhaseDivergent 发散：消息密集，对话活跃。
	// 适合：头脑风暴、热烈讨论。
	PhaseDivergent ConversationPhase = "divergent"
	// PhaseConvergent 收敛：消息间隔在变长，对话趋于平缓。
	// 适合：总结、投票、收敛阶段。
	PhaseConvergent ConversationPhase = "convergent"
)

// String 返回阶段的人类可读描述。
func (p ConversationPhase) String() string {
	switch p {
	case PhaseIdle:
		return "idle"
	case PhaseDivergent:
		return "divergent (active discussion)"
	case PhaseConvergent:
		return "convergent (slowing down)"
	default:
		return "unknown"
	}
}

// frequencyMultiplierForPhase 返回给定对话阶段建议的频率倍率。
// 参考 Houde et al. (2025)：
//   - 发散阶段：适度提升频率，但不喧宾夺主
//   - 收敛阶段：降低频率，避免在总结时插入无关内容
//   - 空闲：降低频率，避免在安静群组突兀插话
func frequencyMultiplierForPhase(phase ConversationPhase) float64 {
	switch phase {
	case PhaseDivergent:
		return 1.2
	case PhaseConvergent:
		return 0.7
	case PhaseIdle:
		return 0.5
	default:
		return 1.0
	}
}

// inferPhase 根据消息间隔样本推断当前对话阶段。
//
// 策略：
//  1. 如果最近 5 分钟无消息 → PhaseIdle
//  2. 如果间隔样本不足 3 个 → 默认 PhaseDivergent（保守假设活跃）
//  3. 计算平均间隔和趋势（后半 vs 前半）
//     - 间隔在增大（对话在降温）→ PhaseConvergent
//     - 间隔较短（<30s）→ PhaseDivergent
//     - 否则 → PhaseConvergent
func inferPhase(intervals []float64, now, lastMsgAt time.Time) ConversationPhase {
	// 空闲检测：5 分钟无消息
	if lastMsgAt.IsZero() || now.Sub(lastMsgAt) > 5*time.Minute {
		return PhaseIdle
	}

	if len(intervals) < 3 {
		return PhaseDivergent
	}

	// 计算平均间隔
	sum := 0.0
	for _, v := range intervals {
		sum += v
	}
	avg := sum / float64(len(intervals))

	// 计算趋势：后半段平均间隔 / 前半段平均间隔
	mid := len(intervals) / 2
	firstHalf := 0.0
	for _, v := range intervals[:mid] {
		firstHalf += v
	}
	firstHalf /= float64(mid)

	secondHalf := 0.0
	for _, v := range intervals[mid:] {
		secondHalf += v
	}
	secondHalf /= float64(len(intervals) - mid)

	// 间隔增大（对话在降温）→ 收敛
	if secondHalf > firstHalf*1.5 {
		return PhaseConvergent
	}

	// 间隔较短 → 发散
	if avg < 30 {
		return PhaseDivergent
	}

	// 中等间隔，根据趋势
	if secondHalf < firstHalf*0.7 {
		return PhaseDivergent // 间隔在缩短，对话在升温
	}

	return PhaseConvergent
}
