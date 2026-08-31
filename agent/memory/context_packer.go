package memory

import (
	"context"
	"sort"
	"strings"
	"time"
)

// ============================================================================
// ContextPacker — 精细上下文打包器
//
// 参考 Memoh 的 context_packer.go：
//   - 字符预算管理（TargetItems, MaxTotalChars, MinItemChars, MaxItemChars）
//   - 四阶段打包：贪心填充 → 压缩腾位 → 重分配 → Anti-lost-in-the-middle 重排序
//
// Anti-lost-in-the-middle 原理：
//   LLM 的注意力呈 U 型曲线——开头和结尾最强，中间最弱。
//   将最高分的条目放在首尾位置，最大化信息利用。
// ============================================================================

// ContextPackerConfig 配置上下文打包器。
type ContextPackerConfig struct {
	// MaxTotalChars 总字符预算（默认 1800）。
	MaxTotalChars int
	// MinItemChars 单条最小字符数（默认 40）。
	// 超额时截断到此长度，保留核心信息。
	MinItemChars int
	// MaxItemChars 单条最大字符数（默认 360）。
	// 超过时截断。
	MaxItemChars int
	// TargetItems 目标条目数（默认 8）。
	TargetItems int
	// OverfetchRatio 过采样率（默认 3）。
	// 供调用方在检索阶段预取 TargetItems × OverfetchRatio 条候选，
	// 再交由 Pack 按评分筛选。Pack 本身不使用此值，
	// 但调用方应据此设定检索 limit 以保证候选池足够大。
	OverfetchRatio int
	// EnableReorder 是否启用 anti-lost-in-the-middle 重排序（默认 true）。
	EnableReorder bool
}

// DefaultContextPackerConfig 返回默认配置。
func DefaultContextPackerConfig() ContextPackerConfig {
	return ContextPackerConfig{
		MaxTotalChars:  1800,
		MinItemChars:   40,
		MaxItemChars:   360,
		TargetItems:    8,
		OverfetchRatio: 3,
		EnableReorder:  true,
	}
}

// ContextPacker 精细上下文打包器。
// 将检索到的记忆条目按字符预算和注意力优化打包为上下文。
type ContextPacker struct {
	config ContextPackerConfig
}

// NewContextPacker 创建上下文打包器。
//
// 如果传入 config，零值字段会被填充为默认值。
// EnableReorder 的零值 (false) 在显式传入 config 时被视为有效选择（即关闭重排序）。
// 如需默认行为（开启重排序），请使用 NewContextPacker() 不传参数。
func NewContextPacker(config ...ContextPackerConfig) *ContextPacker {
	if len(config) == 0 {
		return &ContextPacker{config: DefaultContextPackerConfig()}
	}
	cfg := config[0]
	if cfg.MaxTotalChars <= 0 {
		cfg.MaxTotalChars = 1800
	}
	if cfg.MinItemChars <= 0 {
		cfg.MinItemChars = 40
	}
	if cfg.MaxItemChars <= 0 {
		cfg.MaxItemChars = 360
	}
	if cfg.TargetItems <= 0 {
		cfg.TargetItems = 8
	}
	if cfg.OverfetchRatio <= 0 {
		cfg.OverfetchRatio = 3
	}
	return &ContextPacker{config: cfg}
}

// PackEntry 带有打包后的评分和截断信息。
type PackEntry struct {
	Entry     Entry
	Score     float64
	Truncated bool
}

// Pack 将记忆条目打包为上下文。
//
// 流程：
//  1. 按相关度/重要度/时间计算 score
//  2. 按分数降序排列
//  3. 贪心填充到字符预算（超长条目截断，已选条目可压缩腾位）
//  4. Anti-lost-in-the-middle 重排序
//
// 注意：过采样（OverfetchRatio）由调用方在检索阶段处理，
// 本方法接收已经过采样后的候选集，不自行扩大检索范围。
//
// 参数：
//   - entries: 候选记忆条目
//   - queryText: 查询文本（用于评分）
func (p *ContextPacker) Pack(_ context.Context, entries []Entry, queryText string) []PackEntry {
	if len(entries) == 0 {
		return nil
	}

	// 1. 评分
	scored := p.scoreEntries(entries, queryText)

	// 2. 按分数降序排列
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	// 3. 贪心填充
	maxChars := p.config.MaxTotalChars
	maxItems := p.config.TargetItems
	used := 0
	var selected []PackEntry

	for _, pe := range scored {
		if len(selected) >= maxItems {
			break
		}
		content := pe.Entry.Content
		contentLen := len([]rune(content))

		// 截断超长条目
		if contentLen > p.config.MaxItemChars {
			runes := []rune(content)
			content = string(runes[:p.config.MaxItemChars]) + "..."
			contentLen = p.config.MaxItemChars + 3
			pe.Truncated = true
			pe.Entry.Content = content
		}

		if used+contentLen > maxChars {
			// 尝试压缩已选条目腾位
			if p.config.MinItemChars > 0 && len(selected) > 0 {
				// 保存快照，压缩不可逆时用于回滚
				savedContents := make([]string, len(selected))
				savedTrunc := make([]bool, len(selected))
				for i, s := range selected {
					savedContents[i] = s.Entry.Content
					savedTrunc[i] = s.Truncated
				}
				savedUsed := used

				freed := p.compressToFit(selected, used+contentLen-maxChars)
				if freed > 0 {
					used -= freed
					if used+contentLen <= maxChars {
						selected = append(selected, pe)
						used += contentLen
					} else {
						// 腾出的空间不够，回滚已选条目的压缩
						for i := range selected {
							selected[i].Entry.Content = savedContents[i]
							selected[i].Truncated = savedTrunc[i]
						}
						used = savedUsed
					}
				}
			}
			continue
		}

		selected = append(selected, pe)
		used += contentLen
	}

	// 4. Anti-lost-in-the-middle 重排序
	if p.config.EnableReorder && len(selected) > 2 {
		selected = p.reorderAntiLostMiddle(selected)
	}

	return selected
}

// PackToText 将打包结果格式化为文本。
func (p *ContextPacker) PackToText(ctx context.Context, entries []Entry, queryText string) string {
	packed := p.Pack(ctx, entries, queryText)
	if len(packed) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, pe := range packed {
		sb.WriteString("- ")
		sb.WriteString(pe.Entry.Content)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// scoreEntries 计算每条记忆的打包分数。
func (p *ContextPacker) scoreEntries(entries []Entry, queryText string) []PackEntry {
	result := make([]PackEntry, len(entries))
	queryLower := strings.ToLower(queryText)

	for i, entry := range entries {
		score := p.scoreEntry(entry, queryLower)
		result[i] = PackEntry{Entry: entry, Score: score}
	}

	return result
}

// scoreEntry 计算单条记忆的分数。
// 分数组成：
//   - 关键词匹配（0~0.4）
//   - 重要度（0~0.3）
//   - 时间近因性（0~0.2）
//   - 分类权重（0~0.1）
func (p *ContextPacker) scoreEntry(entry Entry, queryLower string) float64 {
	var score float64

	// 1. 关键词匹配
	if queryLower != "" {
		contentLower := strings.ToLower(entry.Content)
		// 简单的关键词重叠度
		queryWords := strings.Fields(queryLower)
		if len(queryWords) > 0 {
			hits := 0
			for _, w := range queryWords {
				if len(w) >= 2 && strings.Contains(contentLower, w) {
					hits++
				}
			}
			score += float64(hits) / float64(len(queryWords)) * 0.4
		}
	}

	// 2. 重要度
	score += entry.Importance * 0.3

	// 3. 时间近因性（越新分数越高）
	if !entry.CreatedAt.IsZero() {
		daysSince := time.Since(entry.CreatedAt).Hours() / 24
		var recencyScore float64
		switch {
		case daysSince < 1:
			recencyScore = 0.2
		case daysSince < 7:
			recencyScore = 0.15
		case daysSince < 30:
			recencyScore = 0.08
		default:
			recencyScore = 0.02
		}
		score += recencyScore
	}

	// 4. 分类权重
	switch entry.Category {
	case "preference":
		score += 0.1
	case "fact":
		score += 0.08
	case "event":
		score += 0.05
	case "observation":
		score += 0.03
	default:
		score += 0.02
	}

	return score
}

// compressToFit 压缩已选条目以腾出空间。
// 返回实际释放的字符数。
func (p *ContextPacker) compressToFit(selected []PackEntry, need int) int {
	if need <= 0 {
		return 0
	}

	// 找到最长的条目，截断到 MinItemChars
	type indexedLen struct {
		index int
		runes int
	}

	var lengths []indexedLen
	for i, pe := range selected {
		runeLen := len([]rune(pe.Entry.Content))
		if runeLen > p.config.MinItemChars+10 { // 只压缩显著超长的
			lengths = append(lengths, indexedLen{i, runeLen})
		}
	}

	// 按 rune 长度降序排列
	sort.Slice(lengths, func(i, j int) bool {
		return lengths[i].runes > lengths[j].runes
	})

	freed := 0
	for _, il := range lengths {
		if freed >= need {
			break
		}
		targetLen := p.config.MinItemChars
		pe := &selected[il.index]
		content := pe.Entry.Content
		runes := []rune(content)
		if len(runes) > targetLen {
			gain := len(runes) - targetLen - 3 // "..." 占 3 字符
			if gain > 0 {
				freed += gain
			}
			pe.Entry.Content = string(runes[:targetLen]) + "..."
			pe.Truncated = true
		}
	}

	return freed
}

// reorderAntiLostMiddle 重排序：高分条目放首尾。
//
// 输入: [S1(最高), S2, S3, S4, S5, S6, S7, S8(最低)]
// 输出: [S1, S3, S5, S7, S8, S6, S4, S2]
//
// 奇数位从头排（S1→S3→S5→S7），偶数位从尾排（S8→S6→S4→S2）
// 最终效果：最高分在头尾，最低分在中间
func (p *ContextPacker) reorderAntiLostMiddle(entries []PackEntry) []PackEntry {
	n := len(entries)
	if n <= 2 {
		return entries
	}

	// 按分数降序排列
	sorted := make([]PackEntry, n)
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Score > sorted[j].Score
	})

	result := make([]PackEntry, n)
	left := 0
	right := n - 1

	for i, pe := range sorted {
		if i%2 == 0 {
			// 偶数索引（含0）放左边（头部）
			result[left] = pe
			left++
		} else {
			// 奇数索引放右边（尾部，从后往前）
			result[right] = pe
			right--
		}
	}

	return result
}
