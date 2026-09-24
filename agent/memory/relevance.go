package memory

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"

	"go.uber.org/zap"
)

// 相关性召回：在不引入 embedding / 向量库 / 外部依赖的前提下，用字面 token
// 重合度估计「当前消息」与「历史记忆」的主题相关程度。
//
// 存在理由（实测证据，2026-09-24）：
// 自动召回的候选集是「每 scope 最近 N 条」(见 Snapshot.doRefresh 的 Recent 调用)。
// 当某个 scope 的记忆量远大于 N 时——本机 misskey timeline scope 有 2472 条而 N=50，
// 最近 50 条全部落在同一天——窗口外的历史记忆永远不会进入候选集，
// 后续的 importance 排序再怎么排也救不回来：全表 importance 最高的一批
// （0.800，如用户对 bot 行为的偏好、长期人设事实）全部在窗口外。
//
// 相关性召回负责给这些「老但相关」的条目第二次机会：从更宽的候选窗口里
// 按与当前消息的相关性补足若干条，送回原有排序链路。
//
// 打分用 Ochiai 系数（二元向量的余弦）：|q∩c| / sqrt(|q|*|c|)。
// 相比纯覆盖率 |q∩c|/|q|，它对超长条目的天然优势做了长度归一，避免长记忆霸榜；
// 相比 Jaccard，它对长短差异更温和。

// 召回增强的默认值。
//
// 候选窗口大小由实测定标（2026-09-24，本机 misskey timeline scope 2472 条）：
// 最近 50 条只覆盖 1 天、300 条覆盖 4 天、1000 条覆盖 9 天、全量覆盖 38 天。
// 因此单靠时间窗口无法捞到跨月的高价值记忆（它们排在第 2400 位之后），
// 这才需要第二条「高价值保底」通道按 importance 直接取。
const (
	// DefaultRelevanceCandidates 默认候选窗口大小（每个 scope）。
	DefaultRelevanceCandidates = 1000
	// DefaultRelevanceTopK 默认相关性通道总补足条数（跨 scope 合计）。
	DefaultRelevanceTopK = 5
	// DefaultImportantTopK 默认高价值保底通道的补足条数（跨 scope 合计）。
	//
	// 实测：这类条目往往很长（数百字），取 5 条就能吃光 2200 字符预算，
	// 把主通道的近期记忆全部挤掉（注入只剩 "showing 5"）。取 3 条 + 单条
	// 长度封顶，才能在「记起老事」和「知道当下」之间保住平衡。
	DefaultImportantTopK = 3
	// DefaultRecalledMaxChars 补充条目单条字符上限（默认 240）。
	// 超出部分截断并加省略号，避免少数长记忆独占整个记忆块预算。
	DefaultRecalledMaxChars = 240
	// DefaultImportantMinImportance 高价值保底的 importance 阈值。
	//
	// 注意两张表的规模差了一个量级（2026-09-24 实测，本机）：
	//   - tiered_memories：importance>=0.7 仅 11 条
	//   - memory_entries：importance>=0.7 有 641 条，平均 3264 字符、最长 20696
	// 生产链路用 MergedRetriever，两个源都会命中，因此实际面对的是后者。
	// 任何按「只有个位数短条目」做的定标都是错的。
	DefaultImportantMinImportance = 0.7
	// importantScanLimit 高价值保底通道的单次扫描上限。
	//
	// 这是**防止后端返回过多的安全网**，不是筛选条件：真正的过滤由
	// Query.MinImportance 完成。它必须大于「满足阈值的条目总数」，
	// 否则会退化成「只看最早创建的 N 条」——本机 1.00 分条目按时间升序
	// 排在第 516/573 位，用 200 时会永久漏掉。命中上限时会打 WARN。
	importantScanLimit = 2000
	// recalledSkipChars 补充通道跳过超长条目的原始长度门槛（= 封顶值的倍数）。
	//
	// 超过这个长度的条目被 truncateRecalled 截断后只剩下断头残句，
	// 注入 prompt 比不注入更糟：本机实测会命中「【栞娜人格设定】」(701/951 字符)
	// 与「luna 完整档案」(2287 字符)，截断后是残缺的人格定义。
	// 这类内容属于 persona / profile 层的职责，不该由召回通道重复注入。
	recalledSkipFactor = 2
	// relevanceFloorStep 相关性条目相对主通道入选门槛的最小抬升量。
	// 抬升的目的见 Snapshot.doRefresh：不抬的话补回来的老记忆会在后续
	// importance 排序和字符预算截断里被再次挤掉，等于白补。
	relevanceFloorStep = 0.05
	// relevanceScoreSpan 相关性打分映射到的抬升区间宽度。
	relevanceScoreSpan = 0.10
)

// ScoredEntry 带相关性分数的记忆条目。
type ScoredEntry struct {
	Entry Entry
	Score float64
}

// isTokenRune 判断是否属于 token 组成字符（其余一律视为分隔符）。
func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// relevanceTokens 把文本切成 token 集合。
//
// 复用包内已有的 isCJK（think_filter.go）。注意与 dreaming_phases 的 tokenize
// 区别：那个按整词切分且不产生 CJK bigram，用于梦境阶段的词频统计；
// 这里需要 bigram，否则中文短查询（如「鹦鹉」）与长句的匹配会过于稀疏。
//
// 切分规则：
//   - 拉丁/数字：按非字母数字切分成词，长度 >= 2 才入集合（单字符噪音太大）；
//   - CJK：连续 CJK 段内取单字 + 相邻二字（bigram），兼顾「鹦鹉」这类双字词
//     与「养鹦鹉」这类局部搭配。
func relevanceTokens(s string) map[string]struct{} {
	tokens := make(map[string]struct{})
	if s == "" {
		return tokens
	}

	lower := strings.ToLower(s)
	segments := strings.FieldsFunc(lower, func(r rune) bool { return !isTokenRune(r) })

	for _, seg := range segments {
		runes := []rune(seg)
		hasCJK := false
		for _, r := range runes {
			if isCJK(r) {
				hasCJK = true
				break
			}
		}

		if hasCJK {
			for i, r := range runes {
				if isCJK(r) {
					tokens[string(r)] = struct{}{}
					if i+1 < len(runes) && isCJK(runes[i+1]) {
						tokens[string(r)+string(runes[i+1])] = struct{}{}
					}
				} else if len(runes) >= 2 {
					// CJK 段内夹杂的拉丁词（如「MCP」「AI」）
					tokens[seg] = struct{}{}
				}
			}
			continue
		}

		if len(runes) >= 2 {
			tokens[seg] = struct{}{}
		}
	}

	return tokens
}

// RelevanceScore 计算 query 与 content 的相关性，取值 0.0 ~ 1.0。
// 任一侧为空或无 token 重合时返回 0。
func RelevanceScore(query, content string) float64 {
	q := relevanceTokens(query)
	if len(q) == 0 {
		return 0
	}
	c := relevanceTokens(content)
	if len(c) == 0 {
		return 0
	}

	inter := 0
	for t := range q {
		if _, ok := c[t]; ok {
			inter++
		}
	}
	if inter == 0 {
		return 0
	}

	score := float64(inter) / math.Sqrt(float64(len(q))*float64(len(c)))
	if score > 1 {
		score = 1
	}
	return score
}

// SelectRelevant 从 candidates 中排除 exclude 里的条目，按与 query 的相关性
// 降序返回至多 topK 条。分数为 0 的条目不入选（完全不相关不该占用名额）。
//
// exclude 用记忆 ID 判定；ID 为空的条目无法去重，会被保留（宁可重复也别丢条）。
func SelectRelevant(query string, candidates []Entry, exclude map[string]struct{}, topK int) []ScoredEntry {
	if topK <= 0 || strings.TrimSpace(query) == "" {
		return nil
	}

	q := relevanceTokens(query)
	if len(q) == 0 {
		return nil
	}

	// IDF 加权：先统计候选集内的文档频率。
	//
	// 不做这一步的话，中文停用词会主导打分——实测 query「养鹦鹉的事」的 top1
	// 是「睡了一觉缓过来了，什么天气了还在冒汗」（只共享「的/了/什么」），
	// 而真正的「养鹦鹉」记忆排在后面。原因是短条目的 |c| 小，
	// Ochiai 分母小 → 靠几个通用字就能拿高分。用 IDF 给高频无区分度的
	// token 降权后，「鹦鹉」这类稀有词的权重会比「的/了」高一个量级。
	df := make(map[string]int, 256)
	tokenSets := make([]map[string]struct{}, 0, len(candidates))
	for _, e := range candidates {
		ts := relevanceTokens(e.Content)
		tokenSets = append(tokenSets, ts)
		for t := range ts {
			df[t]++
		}
	}

	total := float64(len(candidates))
	idf := make(map[string]float64, len(df))
	for t, cnt := range df {
		idf[t] = math.Log(1 + total/(1+float64(cnt)))
	}

	scored := make([]ScoredEntry, 0, len(candidates))
	for i, e := range candidates {
		if e.ID != "" {
			if _, ok := exclude[e.ID]; ok {
				continue
			}
		}

		var weighted float64
		for t := range q {
			if _, ok := tokenSets[i][t]; ok {
				weighted += idf[t]
			}
		}
		if weighted <= 0 {
			continue
		}

		score := weighted / math.Sqrt(float64(len(q))*float64(len(tokenSets[i])))
		if score > 1 {
			score = 1
		}
		scored = append(scored, ScoredEntry{Entry: e, Score: score})
	}

	if len(scored) == 0 {
		return nil
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Score > scored[j].Score
	})

	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored
}

// SelectImportant 取 importance 达到阈值、且不在 exclude 中的条目，
// 按 importance 降序返回至多 topK 条。
//
// 这是「高价值保底」通道：时间窗口再大也覆盖不到的跨月记忆，只要 importance
// 够高（长期事实/偏好）就该被想起。
//
// 排序在**内存**完成，不依赖 Query.Order + Limit 的组合。原因（2026-09-24 实测）：
// Limit 一旦小于满足阈值的条目总数，就退化成「只扫最早/最新的 N 条」，
// 而 Order 只能在两端二选一，救不了中间——本机 importance>=0.7 有 641 条，
// 用 OrderAsc+Limit(200) 时排在第 516/573 位的 1.00 分条目永久漏掉，
// 抓到的反而是最早的人格设定条目。真正的过滤交给 Query.MinImportance，
// Limit 只作安全网；命中安全网时打 WARN 提示上调。
//
// 检索依赖 Query.MinImportance；后端若不支持该过滤，本通道退化为返回
// 最新若干条（与已有条目重复会被 exclude 掉），属于无害的 fail-soft。
//
// 返回的 Score 恒为 0：保底通道与当前输入无关，没有相关性分可言。
// 抬升时只保证越过主通道门槛即可（见 boostRelevance）。
func SelectImportant(ctx context.Context, r Retriever, scopes []Scope, exclude map[string]struct{}, minImportance float64, topK int, logger ...*zap.SugaredLogger) []ScoredEntry {
	if topK <= 0 || minImportance <= 0 || r == nil {
		return nil
	}

	got, err := r.Retrieve(ctx, Query{
		Scopes:        scopes,
		MinImportance: minImportance,
		Limit:         importantScanLimit,
	})
	if err != nil || len(got) == 0 {
		return nil
	}

	if len(got) >= importantScanLimit && len(logger) > 0 && logger[0] != nil {
		// WARN：扫描上限被打满意味着 MinImportance 之上的条目没取全，
		// 本通道会静默退化成「按时间截断」，需要上调 importantScanLimit。
		logger[0].Warnw("memory: important scan limit reached, high-value recall truncated",
			"limit", importantScanLimit, "minImportance", minImportance)
	}

	scored := make([]ScoredEntry, 0, len(got))
	for _, e := range got {
		if e.ID != "" {
			if _, ok := exclude[e.ID]; ok {
				continue
			}
		}
		scored = append(scored, ScoredEntry{Entry: e, Score: 0})
	}
	if len(scored) == 0 {
		return nil
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].Entry.Importance > scored[j].Entry.Importance
	})
	if len(scored) > topK {
		scored = scored[:topK]
	}
	return scored
}

// capRecalled 限制补充条目的单条字符数，返回应实际补入的条目。
//
// 两件事：
//  1. 封顶：高 importance 的记忆常是长摘要（实测有 400+ 字的心理状态汇总），
//     不封顶的话三五条就会吃满整个记忆块字符预算，把主通道的近期记忆全挤出去
//     ——实测开启后注入从 20 条掉到 "showing 5"，bot 反而不知道当下发生了什么。
//  2. 跳过超长条目：原始长度超过封顶值 recalledSkipFactor 倍的条目直接丢弃，
//     不做截断。截断后的残句没有阅读价值，实测会命中「【栞娜人格设定】」
//     (701/951 字符) 与「luna 完整档案」(2287 字符)，注入后是断头的人格定义。
//     这类内容属于 persona / profile 层，不该由召回通道重复注入。
//
// maxChars <= 0 表示不限制，原样返回。
func capRecalled(entries []Entry, maxChars int) []Entry {
	if maxChars <= 0 {
		return entries
	}

	skipAbove := maxChars * recalledSkipFactor
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		runes := []rune(e.Content)
		if len(runes) > skipAbove {
			continue
		}
		if len(runes) > maxChars {
			e.Content = string(runes[:maxChars]) + "…"
		}
		out = append(out, e)
	}
	return out
}

// relevanceGate 返回主通道按 importance 降序取前 maxEntries 条时的入选门槛
// （即末位条目的 importance）。候选不足时返回最低 importance。
//
// 用于把相关性条目的有效 importance 抬到门槛之上：抬到门槛之上才不会被
// 后续「按 importance 降序截断」和「字符预算逐条截断」二次挤掉。
func relevanceGate(entries []Entry, maxEntries int) float64 {
	if len(entries) == 0 {
		return 0
	}

	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Importance > sorted[j].Importance
	})

	idx := len(sorted) - 1
	if maxEntries > 0 && maxEntries < len(sorted) {
		idx = maxEntries - 1
	}
	return sorted[idx].Importance
}

// boostRelevance 就地抬升相关性条目的有效 importance。
//
// 抬升幅度 = 门槛 + 固定步长 + 相关性分数映射，保证：
//  1. 高于主通道入选门槛 → 不会被 importance 截断挤掉；
//  2. 彼此仍按相关性排序 → 最相关的排最前，优先占字符预算。
//
// 注意：importance 只参与排序，不渲染进 prompt（见 renderBlock），
// 因此抬升不会向模型泄露失真的「重要度」信息。
func boostRelevance(picked []ScoredEntry, gate float64) {
	for i := range picked {
		eff := gate + relevanceFloorStep + picked[i].Score*relevanceScoreSpan
		if eff > 1 {
			eff = 1
		}
		if picked[i].Entry.Importance < eff {
			picked[i].Entry.Importance = eff
		}
	}
}
