package workflow

import "strings"

// ============================================================================
// 节点确定性指标（NodeGrades）
//
// 借鉴 gh-aw 的 graders 思路：**能用代码算的，不要问 LLM**。
//
// 背景：节点质量此前完全依赖 Review SubAgent 的自评——「通过 / 不通过」
// 是一个 LLM 判断，既不稳定也不可归因。这里的指标全部从已有运行数据
// 确定性算出，不引入任何额外 LLM 调用，也不受模型波动影响。
//
// 刻意**不含** token 类指标（gh-aw 的 working-set-rebuild-factor、
// context-growth）：它们需要按节点归因的 token 数据（见阶段 4 的
// WorkflowUsage 明细表），得等那条链路落地后才能算。先做不依赖它的部分。
// ============================================================================

// NodeGrades 节点级确定性指标。
type NodeGrades struct {
	// Retries 执行重试次数（不含首次）。
	Retries int `json:"retries"`
	// ReviewIterations Review 迭代轮数。0 表示未经迭代。
	ReviewIterations int `json:"reviewIterations"`
	// ReviewPassedAt 第几轮 Review 通过。0 = 首轮即通过。
	// 用于区分「一次做对」与「反复打磨才过」——两者的真实成本差很多。
	ReviewPassedAt int `json:"reviewPassedAt"`
	// DurationSec 节点从开始到完成的耗时（秒）。无有效时间戳时为 0。
	DurationSec float64 `json:"durationSec"`
	// ResultLen 产物长度（字符数）。过短通常意味着没做出实质内容。
	ResultLen int `json:"resultLen"`
	// FeedbackLen 最后一次审查意见的长度。
	FeedbackLen int `json:"feedbackLen"`
	// Loops 疑似原地打转的轮数：相邻两轮审查意见高度相似。
	//
	// 相似度用 bigram 的**包含度**（overlap coefficient）而非精确相等——
	// 同一批问题的措辞每次都略有不同，精确比较会漏掉绝大多数真实打转。
	//
	// 为什么不用更常见的 Jaccard：Jaccard 的分母是并集，对「追加内容」
	// 很敏感。而「在上一轮意见的基础上再补两句」恰恰是最典型的原地打转，
	// 此时 Jaccard 会因新增内容被稀释到 0.7 左右而漏判。
	// 包含度的分母是较短一侧，专门刻画「旧意见是否被原样保留」。
	Loops int `json:"loops"`
}

// gradeLoopSimilarity 判定「相邻两轮审查意见是否实质相同」的阈值。
//
// 取值依据：bigram Jaccard 对「同一批问题的不同表述」通常在 0.85 以上，
// 而对「不同批问题」一般低于 0.5。取 0.85 是保守的——宁可漏判也不误判，
// 误判会让正常的迭代打磨被算成打转。
const gradeLoopSimilarity = 0.85

// Grade 从节点的运行数据算出确定性指标。
// 纯函数，无副作用，可在任意时刻调用。
func Grade(n *DAGNode) NodeGrades {
	g := NodeGrades{
		Retries:          n.RetryCount,
		ReviewIterations: n.IterationCount,
		ResultLen:        len(n.Result),
		FeedbackLen:      len(n.ReviewFeedback),
	}

	if n.StartedAt != nil && n.CompletedAt != nil {
		g.DurationSec = n.CompletedAt.Sub(*n.StartedAt).Seconds()
	}

	// 第一次通过的轮次
	for _, r := range n.ReviewHistory {
		if r.Passed {
			g.ReviewPassedAt = r.Iteration
			break
		}
	}

	g.Loops = countSimilarPairs(n.ReviewHistory)

	return g
}

// countSimilarPairs 统计 ReviewHistory 中「相邻两轮意见高度相似」的对数。
//
// 这些是原地打转的信号：审查给了意见、节点改了、下一轮审查又给了
// 几乎一样的意见——说明反馈没能推动收敛。
func countSimilarPairs(history []ReviewRecord) int {
	if len(history) < 2 {
		return 0
	}
	count := 0
	for i := 1; i < len(history); i++ {
		prev := strings.TrimSpace(history[i-1].Feedback)
		cur := strings.TrimSpace(history[i].Feedback)
		if prev == "" || cur == "" {
			continue
		}
		if bigramOverlap(prev, cur) >= gradeLoopSimilarity {
			count++
		}
	}
	return count
}

// bigramOverlap 计算两段文本的 bigram 包含度（overlap coefficient，0~1）：
//
//	|A ∩ B| / min(|A|, |B|)
//
// 衡量「较短那段是否被较长那段完整包含」。
//
// 为什么用它而不是 Jaccard（|A∩B| / |A∪B|）：打转的典型形态是
// 「上一轮的意见原样保留，再补两句新的」。此时交集虽大，但并集因追加
// 内容而膨胀，Jaccard 会被稀释到 0.7 上下而漏判；包含度的分母是较短侧，
// 只要旧意见被原样保留就接近 1，正合「没进展」这个语义。
//
// 用 rune 而非 byte 切分：中文是主要工作语言，按字节切会切碎汉字，
// 相似度计算失去意义。
//
// 任一侧为空时返回 0——没有共同内容，而不是「相似」。
func bigramOverlap(a, b string) float64 {
	setA := bigramSet(a)
	setB := bigramSet(b)
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}

	inter := 0
	for k := range setA {
		if setB[k] {
			inter++
		}
	}

	shorter := len(setA)
	if len(setB) < shorter {
		shorter = len(setB)
	}
	if shorter == 0 {
		return 0
	}
	return float64(inter) / float64(shorter)
}

// bigramSet 提取文本的二元组集合。
func bigramSet(s string) map[string]bool {
	runes := []rune(s)
	if len(runes) < 2 {
		if len(runes) == 1 {
			return map[string]bool{string(runes[0]): true}
		}
		return nil
	}
	set := make(map[string]bool, len(runes)-1)
	for i := 0; i < len(runes)-1; i++ {
		set[string(runes[i:i+2])] = true
	}
	return set
}
