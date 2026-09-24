package memory

import (
	"context"
	"math"
	"regexp"
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
	// DefaultRenderedMaxChars 注入块里单条记忆的字符上限（默认 240，常开）。
	//
	// 存在理由（2026-09-24 实测）：bot scope 有两条 importance=1.00、
	// 14808/15314 字符的「luna 完整档案+项目全记录」。它们 sort 后永远排第一，
	// 单条就把 2200 字符预算吃满，实测注入块是 "showing 1 by importance"
	// ——bot 每轮看到的只是一份 15000 字档案的前 2200 字残片，其它记忆一条
	// 都进不来。补充召回的老记忆因此永远不可见。
	//
	// 取 240 的依据：本机 memory_entries 里 <=240 占 1824 条、241-480 占 317 条，
	// 短记忆是主体，设 240 不会动它们；>1600 的 395 条是档案/长笔记，
	// 截断后只剩断头，直接丢弃比注入残片好（需要完整内容时走 memory 工具）。
	// 与 RecalledMaxChars 同值：两个通道对「一条记忆该占多少预算」的口径应当一致。
	DefaultRenderedMaxChars = 240
	// DefaultNearDuplicateThreshold 近重复折叠的相似度阈值（默认 0.7，常开）。
	//
	// 存在理由（2026-09-24 实测）：注入块里出现过「变压器事件」×4、「@umeboshicc
	// 档案」×3 —— 同一事件被反复记成措辞略不同的几条，importance 相近，
	// 于是同时挤进 20 条名额，把其它记忆挤出去。折叠后本机注入从 10 条变 18 条。
	//
	// 相似度用 token 集合的 Jaccard（token 切分同 relevanceTokens，中文取 bigram）。
	// 阈值配合 nearDupSimilarity 的「长度可比时用包含度」口径（见该函数注释）：
	// 纯 Jaccard 在同源长记录上只有 0.5 上下，靠包含度才拉到 0.8+。本机真库
	// 实测 0.7 能折掉事件档案的多版本记录，同时不误折主题不同、只共享通用词的
	// 条目（这类一般低于 0.5）。
	DefaultNearDuplicateThreshold = 0.7
	// nearDupLengthRatio 允许改用包含度判定的 token 数之比上限。
	// 超过这个比例说明一条显著长于另一条，此时只认 Jaccard（见 nearDupSimilarity）。
	nearDupLengthRatio = 3
	// nearDupMinTokens 参与近重复比较的最小 token 数。
	//
	// 极短条目（如「好的」「嗯，收到」）只有 1~2 个 token，任意两条都可能
	// Jaccard=1.0，但它们携带的信息量本来就低、也不该被当成彼此的重复。
	nearDupMinTokens = 5
	// renderedSkipFactor 注入块里跳过超长条目的倍数门槛（= 上限 × 本值）。
	renderedSkipFactor = 4
	// recalledSkipChars 补充通道跳过超长条目的原始长度门槛（= 封顶值的倍数）。
	//
	// 超过这个长度的条目被 truncateRecalled 截断后只剩下断头残句，
	// 注入 prompt 比不注入更糟：本机实测会命中「【栞娜人格设定】」(701/951 字符)
	// 与「luna 完整档案」(2287 字符)，截断后是残缺的人格定义。
	// 这类内容属于 persona / profile 层的职责，不该由召回通道重复注入。
	recalledSkipFactor = 2
	// DefaultImportantRelevanceWeight 保底通道里「本轮最相关」条目能拿到的
	// importance 等效加成上限（默认 0.6）。
	//
	// 排序键 = importance + weight*归一化相关性，归一化取候选池内最高相关性为 1。
	// weight=0 时退化为纯 importance 排序（改动前的行为）。
	//
	// 为什么用归一化而不是原始相关性分：原始分被 Ochiai 的长度归一压得很小
	// （实测真库 100~300 字的记忆上只有 0.05~0.3），直接乘固定权重根本压不过
	// 0.95 的无关条目；归一化后「本轮最相关的那条」稳定拿到满额加成，
	// 0.80 + 0.6 > 0.95，而完全不相关的条目加成恒为 0。
	//
	// 取 0.6 而非 1.0：加成过大会让 importance 这一既有信号失去作用，
	// 0.6 足够跨过一到两档 importance 差，同时在相关性接近时仍由 importance 定序。
	DefaultImportantRelevanceWeight = 0.6
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
//   - CJK：连续 CJK 段取相邻二字（bigram）；段长只有 1 时保留该单字。
//
// CJK 为什么不再取单字（2026-09-24 实测修正）：单字与 bigram 会重复计权——
// 内容里出现一次「什么」，查询侧同时命中「什」「么」「什么」三个 token，
// 一个通用词顶三次命中。结果是「今天讨论了一下晚饭吃什么」的相关性(0.125)
// 反而高于真正的目标「@luna gets annoyed by bots...」(0.066)，
// 通用字压过实体名。改为只用 bigram 后同一组样本变为 0.044 vs 0.066，序正确。
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
					if i+1 < len(runes) && isCJK(runes[i+1]) {
						tokens[string(r)+string(runes[i+1])] = struct{}{}
					} else if len(runes) == 1 {
						// 单独成段的单字（前后都是分隔符）：没有 bigram 可用，保留
						tokens[string(r)] = struct{}{}
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

	tokenSets := tokenizeAll(candidates)
	idf := buildIDF(tokenSets)

	scored := make([]ScoredEntry, 0, len(candidates))
	for i, e := range candidates {
		if e.ID != "" {
			if _, ok := exclude[e.ID]; ok {
				continue
			}
		}

		score := idfScore(q, tokenSets[i], idf)
		if score <= 0 {
			continue
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

// tokenizeAll 批量切分候选条目，返回与 candidates 等长的 token 集合切片。
func tokenizeAll(candidates []Entry) []map[string]struct{} {
	out := make([]map[string]struct{}, 0, len(candidates))
	for _, e := range candidates {
		out = append(out, relevanceTokens(e.Content))
	}
	return out
}

// buildIDF 统计候选集内的文档频率并换算成 IDF 权重。
//
// 不做这一步的话，中文停用词会主导打分——实测 query「养鹦鹉的事」的 top1
// 是「睡了一觉缓过来了，什么天气了还在冒汗」（只共享「的/了/什么」），
// 而真正的「养鹦鹉」记忆排在后面。原因是短条目的 |c| 小，
// Ochiai 分母小 → 靠几个通用字就能拿高分。用 IDF 给高频无区分度的
// token 降权后，「鹦鹉」这类稀有词的权重会比「的/了」高一个量级。
func buildIDF(tokenSets []map[string]struct{}) map[string]float64 {
	df := make(map[string]int, 256)
	for _, ts := range tokenSets {
		for t := range ts {
			df[t]++
		}
	}

	total := float64(len(tokenSets))
	idf := make(map[string]float64, len(df))
	for t, cnt := range df {
		idf[t] = math.Log(1 + total/(1+float64(cnt)))
	}
	return idf
}

// idfScore 用 IDF 加权的 Ochiai 系数计算 query 与单条候选的相关性（0.0~1.0）。
func idfScore(q, c map[string]struct{}, idf map[string]float64) float64 {
	if len(q) == 0 || len(c) == 0 {
		return 0
	}

	var weighted float64
	for t := range q {
		if _, ok := c[t]; ok {
			weighted += idf[t]
		}
	}
	if weighted <= 0 {
		return 0
	}

	score := weighted / math.Sqrt(float64(len(q))*float64(len(c)))
	if score > 1 {
		score = 1
	}
	return score
}

// SelectImportant 取 importance 达到阈值的条目，按
// 「与 query 的相关性 * relevanceWeight + importance」降序返回至多 topK 条。
//
// 这是「高价值保底」通道：时间窗口再大也覆盖不到的跨月记忆，只要 importance
// 够高（长期事实/偏好）就该被想起。
//
// 为什么必须带 query（2026-09-24 实测）：
// 初版按纯 importance 排序，名额恒定被同一批 0.95 条目占满（心理状态汇总等，
// 与当轮话题无关），导致每轮固定注入几条无关记忆，而诊断真正想救的 0.800
// 长期事实（用户对 bot 行为的偏好等）一条都进不来。相关性参与排序后，
// 话题相关的老记忆可以反超一到两档 importance 差（0.80+0.6*rel > 0.95）。
// query 为空时相关性恒为 0，退化为纯 importance 排序，行为与初版一致。
//
// 排序在**内存**完成，不依赖 Query.Order + Limit 的组合。原因（2026-09-24 实测）：
// Limit 一旦小于满足阈值的条目总数，就退化成「只扫最早/最新的 N 条」，
// 而 Order 只能在两端二选一，救不了中间——本机 importance>=0.7 有 641 条，
// 用 OrderAsc+Limit(200) 时排在第 516/573 位的 1.00 分条目永久漏掉，
// 抓到的反而是最早的人格设定条目。真正的过滤交给 Query.MinImportance，
// Limit 只作安全网；命中安全网时打 WARN 提示上调。
//
// 检索依赖 Query.MinImportance；后端若不支持该过滤，本通道会退化成返回
// 该后端的最新若干条（与已有条目重复会被 exclude 掉），属于 fail-soft，
// 但会让本通道失去「跨时间捞高价值」的意义——后端必须实现 MinImportance。
//
// maxChars 是补充通道的单条字符上限（= SnapshotConfig.RecalledMaxChars）：
// 超过 recalledSkipFactor 倍的条目在打分前就被剔除，理由见 filterOversized
// ——给它们做分词是每轮开销的主要来源，而它们最终一定会被丢弃。
//
// 返回的 Score 是归一化后的相关性分（无 query 时为 0），用于 boostRelevance 抬升排序。
func SelectImportant(ctx context.Context, r Retriever, scopes []Scope, exclude map[string]struct{}, query string, minImportance float64, relevanceWeight float64, topK int, maxChars int, logger ...*zap.SugaredLogger) []ScoredEntry {
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

	var kept []Entry
	for _, e := range got {
		if e.ID != "" {
			if _, ok := exclude[e.ID]; ok {
				continue
			}
		}
		kept = append(kept, e)
	}
	candidates := filterOversized(kept, maxChars)
	if len(candidates) == 0 {
		return nil
	}

	// 相关性只在有 query 时计算；无 query 时 rel 全为 0，等价于纯 importance。
	//
	// 归一化：以候选池内最高相关性为 1，其余按比例缩放。原始分受长度归一压制，
	// 量级不稳定（见 DefaultImportantRelevanceWeight 的说明），不归一化则权重
	// 无法给出可预测的效果。
	rel := make([]float64, len(candidates))
	if strings.TrimSpace(query) != "" {
		q := relevanceTokens(query)
		if len(q) > 0 {
			tokenSets := tokenizeAll(candidates)
			idf := buildIDF(tokenSets)
			maxRel := 0.0
			for i := range candidates {
				rel[i] = idfScore(q, tokenSets[i], idf)
				if rel[i] > maxRel {
					maxRel = rel[i]
				}
			}
			if maxRel > 0 {
				for i := range rel {
					rel[i] /= maxRel
				}
			}
		}
	}

	scored := make([]ScoredEntry, 0, len(candidates))
	for i, e := range candidates {
		scored = append(scored, ScoredEntry{Entry: e, Score: rel[i]})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		si := scored[i].Score*relevanceWeight + scored[i].Entry.Importance
		sj := scored[j].Score*relevanceWeight + scored[j].Entry.Importance
		return si > sj
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

// filterOversized 剔除最终注定会被丢弃的超长条目（返回新切片）。
//
// 与 capRecalled 用同一门槛（maxChars*recalledSkipFactor）。放在打分之前，
// 是为了避免给这些条目做分词——它们是本轮开销的主要来源：本机有
// 14808/15314 字符的档案条目，不过滤时每轮快照 400ms，过滤后回到基线量级。
func filterOversized(entries []Entry, maxChars int) []Entry {
	return filterOversizedBy(entries, maxChars, recalledSkipFactor)
}

// filterOversizedBy 按指定倍数门槛剔除超长条目。
//
// factor 必须与该通道随后执行的截断函数一致（补充通道 recalledSkipFactor、
// 注入块 renderedSkipFactor），否则会先按更严的门槛丢掉一批、截断函数
// 本该保留的条目——实测复用补充通道的 2 倍门槛处理主通道时，丢弃线从 960
// 掉到 480，20 条里丢了 19 条，注入块只剩 1 条。
func filterOversizedBy(entries []Entry, maxChars int, factor int) []Entry {
	if maxChars <= 0 || factor <= 0 {
		return entries
	}
	skipAbove := maxChars * factor
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if len([]rune(e.Content)) > skipAbove {
			continue
		}
		out = append(out, e)
	}
	return out
}

// capRendered 限制注入块里单条记忆的长度，返回保留的条目与丢弃条数。
//
// 与 capRecalled 同一套取舍，但作用在**主通道**上，因此默认不启用
// （MaxRenderedEntryChars=0 时 Snapshot 不调用它）：
//   - 超过上限 renderedSkipFactor 倍的条目直接丢弃：本机是 14808/15314 字符的
//     完整档案，截断后是断头残片，注入比不注入更糟，完整内容应由 memory 工具按需取；
//   - 其余超长条目截断到上限：否则单条就能吃满 2200 字符预算。
//
// 注意必须先于 MaxEntries 条数截断执行，否则被丢弃的巨型条目仍会占掉名额。
func capRendered(entries []Entry, maxChars int) ([]Entry, int) {
	if maxChars <= 0 {
		return entries, 0
	}

	skipAbove := maxChars * renderedSkipFactor
	out := make([]Entry, 0, len(entries))
	dropped := 0
	for _, e := range entries {
		runes := []rune(e.Content)
		if len(runes) > skipAbove {
			dropped++
			continue
		}
		if len(runes) > maxChars {
			e.Content = string(runes[:maxChars]) + "…"
		}
		out = append(out, e)
	}
	return out, dropped
}

// dedupeNearDuplicates 折叠内容高度重合的条目，返回保留的条目与折叠掉的条数。
//
// 为什么必须放在 MaxEntries 条数截断之前：折叠腾出的名额要能被后面的条目
// 填上，否则「去重」只是少渲染几条，省下的预算一样浪费掉。同理它也必须在
// capRendered **之前**执行——截断后所有长条目都变成同样长度的残片，
// 既算不准相似度，也分不出哪条信息更全。
//
// 判定用 nearDupSimilarity（见 DefaultNearDuplicateThreshold 的定标说明）。
// 只与**已保留**的条目比较（greedy）：候选最多百余条而保留上限 20 条，
// 比较次数是 O(N*K)，不会像全量两两比较那样随条目数平方增长。
//
// 保留哪一条：同组内保留**内容更全（原始长度更大）**的一条，importance 取
// 组内最高值以保住原排序位次。这不是洁癖——本机实测的近重复大多是两类：
//   - 同一事件被反复记录（「变压器事件」×3），措辞略有不同的版本里长版信息更全；
//   - Misskey 回复链（见 dedupeText），链上最新一条天然包含被引用的上文。
//     实测折叠掉 [Reply] 链里的短条目后，保留下来的长条目仍含整链的原文。
//
// threshold <= 0 表示关闭，原样返回。
func dedupeNearDuplicates(entries []Entry, threshold float64) ([]Entry, int) {
	if threshold <= 0 || len(entries) < 2 {
		return entries, 0
	}

	sets := make([]map[string]struct{}, 0, len(entries))
	sizes := make([]int, 0, len(entries))
	for _, e := range entries {
		sets = append(sets, relevanceTokens(dedupeText(e.Content)))
		sizes = append(sizes, len([]rune(e.Content)))
	}

	kept := make([]Entry, 0, len(entries))
	keptSets := make([]map[string]struct{}, 0, len(entries))
	keptSizes := make([]int, 0, len(entries))
	folded := 0

	for i, e := range entries {
		set := sets[i]
		if len(set) >= nearDupMinTokens {
			dup := false
			for ki := range keptSets {
				if nearDupSimilarity(set, keptSets[ki]) < threshold {
					continue
				}
				dup = true
				// 组内 importance 取最高：折叠不该让一条记忆的排序位次下降，
				// 否则「同组里最该被想起的那条」反而被排到后面、被字符预算切掉。
				best := kept[ki].Importance
				if e.Importance > best {
					best = e.Importance
				}
				if sizes[i] > keptSizes[ki] {
					// 用信息更全的一条替换，importance 仍取组内最高。
					replacement := e
					replacement.Importance = best
					kept[ki] = replacement
					keptSets[ki] = set
					keptSizes[ki] = sizes[i]
				} else {
					kept[ki].Importance = best
				}
				break
			}
			if dup {
				folded++
				continue
			}
		}
		kept = append(kept, e)
		keptSets = append(keptSets, set)
		keptSizes = append(keptSizes, sizes[i])
	}
	return kept, folded
}

// replyQuoteRe 匹配 Misskey 回复类记忆的引用前缀，形如
// "[Reply to 茶泡梅干@我有颉压抑: 上文内容] 本条内容"。
//
// 贪婪匹配到最后一个 ']'：引用内容自身可能含 ']'（如引用里带表情短码或
// 嵌套括号），非贪婪会在第一个 ']' 处截断，把引用正文残留下来。
var replyQuoteRe = regexp.MustCompile(`^\[Reply\b.*\]\s*`)

// dedupeText 返回参与近重复比较的文本。
//
// 剥离引用前缀的原因（2026-09-24 实测）：Misskey 回复链上相邻两条记忆的
// 「上文引用」完全一致，只差最后一句新内容，Jaccard 实测 0.62~0.73，
// 会被当成重复折叠掉——丢的恰恰是每条独有的那一句。剥掉引用的共同部分后，
// 比较只剩各自的新内容，相似度回落到阈值以下。
//
// 剥离后若内容过短（不足 nearDupMinTokens），说明这条几乎只剩引用，
// 此时回退到全文比较，避免漏折。
func dedupeText(content string) string {
	if !strings.HasPrefix(content, "[Reply") {
		return content
	}
	stripped := replyQuoteRe.ReplaceAllString(content, "")
	if len(relevanceTokens(stripped)) >= nearDupMinTokens {
		return stripped
	}
	return content
}

// nearDupSimilarity 计算两条记忆的近重复相似度，0.0~1.0。
//
// 基础度量是 Jaccard（|A∩B| / |A∪B|）：用并集做分母，天然惩罚长度差，
// 避免「短笔记被长档案包含」就被判成重复。
//
// 但纯 Jaccard 对长条目过于苛刻（2026-09-24 实测）：同一事件的两条几百字记录
// 各自带一段独有细节，交集 400 token、各自 500/600 token，Jaccard 只有 ~0.55，
// 折不掉——而它们截断成 240 字进注入块后几乎一模一样，占着三个名额讲同一件事。
// 因此在**两条长度可比**时改用包含度 |A∩B| / min(|A|,|B|)：同源记录措辞不同但
// 主体重合，包含度能到 0.8+，而独有细节只把 Jaccard 拉低。
//
// 长度可比 = token 数之比不超过 nearDupLengthRatio。这条限制是防误伤的关键：
// 「用户会画画，几天画不了就心神不宁」的 token 集合几乎必然是某份长档案的子集
// （包含度接近 1），但它是一条独立的短记忆，不该被档案吃掉。
func nearDupSimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// 遍历较小集合，减少哈希查找次数。
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	inter := 0
	for t := range small {
		if _, ok := large[t]; ok {
			inter++
		}
	}
	if inter == 0 {
		return 0
	}
	jaccard := float64(inter) / float64(len(a)+len(b)-inter)
	if float64(len(large))/float64(len(small)) > nearDupLengthRatio {
		return jaccard
	}
	containment := float64(inter) / float64(len(small))
	if containment > jaccard {
		return containment
	}
	return jaccard
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

// boostRelevance 就地抬升补充条目的有效 importance。
//
// 抬升幅度 = 主通道最高 importance + 固定步长 + 相关性分数映射，保证：
//  1. 严格高于主通道所有条目 → 不会被 MaxEntries 截断挤掉，也不会被字符预算
//     挤掉。早期版本抬到「第 MaxEntries 位门槛」之上，实测仍进不了注入块：
//     门槛只是第 20 名的分数，而字符预算只装得下 6 条，补充条目排在 20 名
//     附近等于必然被预算切掉（目标老记忆 rel=1.0 排第一却仍不出现在块里）。
//  2. 彼此仍按相关性排序 → 最相关的排最前。
//
// 注意：importance 只参与排序，不渲染进 prompt（见 renderBlock），
// 因此抬升不会向模型泄露失真的「重要度」信息；这里允许超过 1.0 也是这个原因
// ——它是纯排序键，不是打分。
func boostRelevance(picked []ScoredEntry, gate float64) {
	for i := range picked {
		eff := gate + relevanceFloorStep + picked[i].Score*relevanceScoreSpan
		if eff > 2 {
			eff = 2
		}
		if picked[i].Entry.Importance < eff {
			picked[i].Entry.Importance = eff
		}
	}
}

// topImportance 返回条目中最高的 importance（空切片返回 0）。
//
// 用作 boostRelevance 的抬升基准：补充条目必须严格高于主通道的**最高**分，
// 而不是第 N 名的分数，否则会被后续的条数/字符双重截断挤掉。
func topImportance(entries []Entry) float64 {
	max := 0.0
	for _, e := range entries {
		if e.Importance > max {
			max = e.Importance
		}
	}
	return max
}
